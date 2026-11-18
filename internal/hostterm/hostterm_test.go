package hostterm

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/yousan/hataori/internal/control"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Real list-panes output of tmux 3.4, with a window name holding a space.
// The directory is the only part changed: it was a path on the capturing
// machine.
func TestParsePanes(t *testing.T) {
	got := parsePanes(readTestdata(t, "list-panes-tmux-3.4.txt"))
	if len(got) != 3 {
		t.Fatalf("%d panes: %+v", len(got), got)
	}
	at := time.Unix(1794987263, 0).UTC()
	want := control.HostTerminal{Device: "/dev/pts/63", Process: "sh", Activity: &at,
		Tmux: &control.TmuxPane{Pane: "%1", Session: "build", Window: "1", WindowName: "my editor", Index: "0", Dir: "/work dir"}}
	g := got[1]
	if g.Device != want.Device || g.Process != want.Process || !g.Activity.Equal(at) || *g.Tmux != *want.Tmux {
		t.Errorf("got %+v %+v, want %+v %+v", g, g.Tmux, want, want.Tmux)
	}
}

// Real ps output of procps-ng 4.0.4: a process without a terminal, the
// tmux server (a command with a space), and a pipeline in one pane.
func TestParsePS(t *testing.T) {
	ps := parsePS(readTestdata(t, "ps-linux-procps-ng-4.0.4.txt"))
	if len(ps) != 7 {
		t.Fatalf("%d processes: %+v", len(ps), ps)
	}
	if ps[0].tty != "" || ps[1].comm != "tmux: server" || ps[2].tty != "/dev/pts/62" {
		t.Errorf("%+v", ps[:3])
	}
	o := others(ps)
	var devs []string
	for _, t := range o {
		devs = append(devs, t.Device+" "+t.Process)
	}
	if want := []string{"/dev/pts/62 sleep", "/dev/pts/63 sh", "/dev/pts/64 sh"}; !slices.Equal(devs, want) {
		t.Errorf("terminals %q, want %q", devs, want)
	}
	// The pane of the pipeline, under the shell 1248409, is hidden; so is
	// every pane under the tmux server.
	if h := ownedDevices(ps, []int{1248409}); len(h) != 1 || !h["/dev/pts/64"] {
		t.Errorf("hidden %v", h)
	}
	if h := ownedDevices(ps, []int{1248395}); len(h) != 3 {
		t.Errorf("hidden %v", h)
	}
	// A process of hataori's own name without a terminal counts as owned,
	// as a session host does before its pid is recorded.
	if own := ownPIDs(ps, "tmux: server"); !slices.Equal(own, []int{1248395}) {
		t.Errorf("own pids %v", own)
	}
	if own := ownPIDs(ps, "sleep"); len(own) != 0 {
		t.Errorf("a process with a terminal counts as owned: %v", own)
	}
}

// tmux 3.4 prints a tab in a window's name as it is. Captured with the old
// format, the line has one field too many; with paneFormat the tab is a
// space, which TestOnlyTheFixedCommandsRun checks on a live server.
func TestTabInAWindowName(t *testing.T) {
	line := strings.TrimSpace(string(readTestdata(t, "list-panes-tmux-3.4-tabs.txt")))
	if n := len(strings.Split(line, "\t")); n != 11 {
		t.Fatalf("the capture has %d fields", n)
	}
	if got := parsePanes([]byte(line)); len(got) == 1 && got[0].Device == "/dev/pts/61" {
		t.Errorf("parsed a line with a stray tab: %+v", got[0])
	}
}

// recorder runs the commands for real and records every argv.
type recorder struct{ argvs [][]string }

func (r *recorder) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.argvs = append(r.argvs, append([]string{name}, args...))
	var l Lister
	return l.exec(ctx, command{name, args})
}

// startTmux starts a tmux server on a socket of its own with one pane
// running cmd, and returns the arguments that reach it.
func startTmux(t *testing.T, cmd string) []string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("tmux is not installed; CI must run this test")
		}
		t.Skip("no tmux")
	}
	t.Setenv("TMUX", "")
	sock := []string{"-L", fmt.Sprintf("hataori-hostterm-test-%d", os.Getpid()), "-f", "/dev/null"}
	tmux := func(args ...string) error {
		out, err := exec.Command("tmux", slices.Concat(sock, args)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("tmux %v: %v: %s", args, err, out)
		}
		return nil
	}
	if err := tmux("new-session", "-d", "-s", "hataori-test", "-x", "80", "-y", "24", cmd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tmux("kill-server") })
	return sock
}

// Only the fixed commands run, and none of them changes anything: ps, and
// tmux's list-panes and capture-pane with a pane id.
func TestOnlyTheFixedCommandsRun(t *testing.T) {
	readOnly := map[string]bool{"ps -A": true, "tmux list-panes": true, "tmux capture-pane": true}
	for _, c := range Commands {
		if !readOnly[c.name+" "+c.args[0]] {
			t.Errorf("%s %s is not a read-only command", c.name, c.args[0])
		}
	}
	sock := startTmux(t, "sh -c 'echo fixed; exec sleep 600'")
	if out, err := exec.Command("tmux", slices.Concat(sock, []string{"new-window", "-d", "-t", "hataori-test:", "-n", "a\tb", "exec sleep 600"})...).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	rec := &recorder{}
	l := &Lister{Tmux: sock, run: rec.run}
	ctx := context.Background()
	list, err := l.List(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var pane string
	for _, ht := range list {
		if ht.Tmux == nil || ht.Tmux.Session != "hataori-test" {
			continue
		}
		if ht.Tmux.Window == "0" {
			pane = ht.Tmux.Pane
		} else if ht.Tmux.WindowName != "a b" || !strings.HasPrefix(ht.Device, "/dev/") || ht.Process != "sleep" {
			t.Errorf("a tab in the window's name: %+v %+v", ht, ht.Tmux)
		}
	}
	if pane == "" {
		t.Fatalf("the pane is not listed: %+v", list)
	}
	if _, ok, err := l.Screen(ctx, pane, nil); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if empty, _ := (&Lister{Tmux: []string{"-L", "hataori-hostterm-none"}, run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, nil
	}}).List(ctx, nil); empty == nil {
		t.Error("an empty list is nil, which is null on the wire")
	}
	if _, ok, err := l.Screen(ctx, "%0; kill-server", nil); err != nil || ok {
		t.Fatal("a pane id with more in it was read", ok, err)
	}
	allowed := func(argv []string) bool {
		for _, c := range Commands {
			want := []string{c.name}
			if c.name == "tmux" {
				want = append(want, sock...)
			}
			want = append(want, c.args...)
			if c.name == argv[0] && slices.Equal(argv, want) {
				return true
			}
			if c.args[0] == "capture-pane" && len(argv) == len(want)+1 && slices.Equal(argv[:len(want)], want) &&
				paneID.MatchString(argv[len(want)]) {
				return true
			}
		}
		return false
	}
	ran := map[string]bool{}
	for _, argv := range rec.argvs {
		if !allowed(argv) {
			t.Errorf("ran %q", argv)
		}
		ran[strings.Join(argv[:1], "")] = true
	}
	if !ran["ps"] || !ran["tmux"] || len(rec.argvs) < 5 {
		t.Errorf("ran %q", rec.argvs)
	}
}

// The package imports nothing that opens a file or a terminal, so it
// writes to none, and no string in it names /proc.
func TestNoWriteAndNoProc(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			switch p, _ := strconv.Unquote(imp.Path.Value); p {
			case "os", "syscall", "io/fs", "golang.org/x/sys/unix", "github.com/creack/pty", "net":
				t.Errorf("%s imports %s", name, p)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "/proc") {
				t.Errorf("%s: %s names /proc", fset.Position(lit.Pos()), lit.Value)
			}
			return true
		})
	}
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A terminal that a process under a session host uses is not listed. The
// test holds the terminal as a session host does: it keeps the controlling
// side and starts the command on the other.
func TestSessionHostTerminalIsLeftOut(t *testing.T) {
	cmd := exec.Command("sleep", "600")
	m, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait(); m.Close() })
	ctx := context.Background()
	l := &Lister{Tmux: []string{"-L", fmt.Sprintf("hataori-hostterm-none-%d", os.Getpid())}}
	var dev string
	for deadline := time.Now().Add(10 * time.Second); dev == "" && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		list, err := l.List(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, ht := range list {
			if ht.Process == "sleep" && ht.Tmux == nil {
				procs, _ := l.exec(ctx, psCmd)
				for _, p := range parsePS(procs) {
					if p.pid == cmd.Process.Pid && p.tty == ht.Device {
						dev = ht.Device
					}
				}
			}
		}
	}
	if dev == "" {
		t.Fatal("the terminal is not listed when no session host is named")
	}
	list, err := l.List(ctx, []int{os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	for _, ht := range list {
		if ht.Device == dev {
			t.Fatalf("%s is listed although a session host holds it", dev)
		}
	}
}
