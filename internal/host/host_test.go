package host

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/term"

	"github.com/yousan/hataori/internal/proctest"
)

// The test binary is also the host and every command the tests run:
// `<test binary> role <name> args...` runs one of the roles below.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "role" {
		os.Exit(role(os.Args[2], os.Args[3:]))
	}
	os.Exit(m.Run())
}

func role(name string, args []string) int {
	switch name {
	case "host":
		if err := Run(args[0]); err != nil {
			return 1
		}
		return 0
	case "printenv":
		wd, _ := os.Getwd()
		fmt.Printf("cwd=%s\n", wd)
		for _, kv := range os.Environ() {
			fmt.Printf("env=%s\n", kv)
		}
		return 0
	case "lines":
		os.Stdout.Write(testLines())
		return 0
	case "size":
		w, h, err := term.GetSize(0)
		fmt.Printf("size=%dx%d err=%v\n", h, w, err)
		r := bufio.NewScanner(os.Stdin) // then answer input until the terminal closes
		for r.Scan() {
			fmt.Printf("got %s\n", r.Text())
		}
		return 0
	case "rawread": // rawread <file>: raw mode, then everything up to the paste end
		old, err := term.MakeRaw(0)
		if err != nil {
			fmt.Println("raw:", err)
			return 1
		}
		defer term.Restore(0, old)
		fmt.Print("ready\r\n")
		var got []byte
		buf := make([]byte, 4096)
		for !bytes.HasSuffix(got, []byte(pasteEnd)) {
			n, err := os.Stdin.Read(buf)
			got = append(got, buf[:n]...)
			if err != nil {
				break
			}
		}
		os.WriteFile(args[0], got, 0o600)
		fmt.Print("done\r\n")
		return 0
	case "echo": // answer every input line
		r := bufio.NewScanner(os.Stdin)
		for r.Scan() {
			fmt.Printf("got %s\n", r.Text())
		}
		return 0
	case "exit":
		n, _ := strconv.Atoi(args[0])
		return n
	case "starter": // starter <state> <terminal>: start a host, then wait to be killed
		_, err := Start(hostCommand(), args[0], Spec{Argv: []string{os.Args[0], "role", "echo"}, Terminal: args[1] == "true", Env: []string{}})
		if err != nil {
			fmt.Println("start:", err)
			return 1
		}
		fmt.Println("started")
		select {}
	}
	if f, ok := extraRoles[name]; ok {
		return f(args)
	}
	return 99
}

// extraRoles are roles defined by other test files.
var extraRoles = map[string]func(args []string) int{}

const pasteStart, pasteEnd = "\x1b[200~", "\x1b[201~"

func hostCommand() Command { return Command{Path: os.Args[0], Args: []string{"role", "host"}} }

func testLines() []byte {
	var b bytes.Buffer
	b.WriteString(`{"json":true}` + "\n")
	b.WriteString("not json at all\n")
	b.Write([]byte{0xff, 0xfe, 'x', 0xc3, '\n'}) // not UTF-8
	b.WriteString(strings.Repeat("m", 1<<20) + "\n")
	b.WriteString("\n") // an empty line
	b.WriteString("last\n")
	return b.Bytes()
}

// waitFor polls cond until it holds or the time limit passes.
// waitFor waits up to 10 seconds for cond.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitForWithin(t, what, 10*time.Second, cond)
}

// waitForWithin waits up to d for cond.
func waitForWithin(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// killGroups kills the host and the command of stateDir, and only those:
// a pid file left by one that ended may name another process by now.
func killGroups(stateDir string) {
	for _, f := range []string{PIDFile, HostPIDFile} {
		proctest.Kill(filepath.Join(stateDir, f)) // both lead their own group
	}
}

// startHost starts a host for spec and arranges that nothing it started
// survives the test.
func startHost(t *testing.T, spec Spec) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { killGroups(dir) })
	if _, err := Start(hostCommand(), dir, spec); err != nil {
		t.Fatal(err)
	}
	return dir
}

func read(t *testing.T, dir, name string) []byte {
	b, _ := os.ReadFile(filepath.Join(dir, name))
	return b
}

func self(args ...string) []string { return append([]string{os.Args[0], "role"}, args...) }

func TestDirectoryAndEnvironment(t *testing.T) {
	// The command sees its directory with links resolved: on macOS the
	// temporary directory is under /var, a link to /private/var.
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := []string{"ONE=1", "TWO=two words", "EMPTY="}
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint("terminal=", terminal), func(t *testing.T) {
			dir := t.TempDir()
			t.Cleanup(func() { killGroups(dir) })
			// The host has an environment of its own, which must not leak.
			hc := hostCommand()
			hc.Env = []string{"HOST_ONLY=leaked"}
			if _, err := Start(hc, dir, Spec{Argv: self("printenv"), Dir: work, Env: env, Terminal: terminal}); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the command to end", func() bool { return Gone(dir) })
			out := read(t, dir, EventsFile)
			if terminal {
				out = bytes.ReplaceAll(read(t, dir, TerminalLog), []byte("\r\n"), []byte("\n"))
			}
			want := "cwd=" + work + "\n"
			for _, kv := range env {
				want += "env=" + kv + "\n"
			}
			if string(out) != want {
				t.Fatalf("command saw:\n%s\nwant:\n%s", out, want)
			}
		})
	}
}

func TestPipeLinesByteForByte(t *testing.T) {
	dir := startHost(t, Spec{Argv: self("lines"), Env: []string{}})
	waitFor(t, "the command to end", func() bool { return Gone(dir) })
	if got := read(t, dir, EventsFile); !bytes.Equal(got, testLines()) {
		t.Fatalf("events.ndjson differs from the output: %d bytes, want %d", len(got), len(testLines()))
	}
}

func TestTerminalSizeAndBoundedLog(t *testing.T) {
	dir := startHost(t, Spec{Argv: self("size"), Env: []string{}, Terminal: true, Rows: 40, Cols: 123, LogLimit: 64 << 10})
	waitFor(t, "the size line", func() bool { return bytes.Contains(read(t, dir, TerminalLog), []byte("size=40x123 err=<nil>")) })

	// Fill the log past its limit with the command's answers. (The
	// terminal's own echo is not waited for: the kernel may drop it.)
	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	chunk := []byte(strings.Repeat("x", 1000) + "\n")
	for range 200 {
		if err := c.Input(chunk); err != nil {
			t.Fatal(err)
		}
	}
	c.Input([]byte("end-of-fill\n"))
	waitFor(t, "the last answer", func() bool { return bytes.Contains(read(t, dir, TerminalLog), []byte("got end-of-fill")) })
	st, _ := os.Stat(filepath.Join(dir, TerminalLog))
	if st.Size() > 64<<10 {
		t.Fatalf("terminal log is %d bytes, limit %d", st.Size(), 64<<10)
	}
	if bytes.Contains(read(t, dir, TerminalLog), []byte("size=")) {
		t.Fatal("the oldest part of the log was not dropped")
	}
}

func TestTerminalPasteArrivesUnchanged(t *testing.T) {
	out := filepath.Join(t.TempDir(), "got")
	dir := startHost(t, Spec{Argv: self("rawread", out), Env: []string{}, Terminal: true})
	waitFor(t, "raw mode", func() bool { return bytes.Contains(read(t, dir, TerminalLog), []byte("ready")) })

	var body bytes.Buffer
	body.WriteString("-leading dash ")
	for body.Len() < 100<<10 {
		body.WriteString("日本語のテキスト ✓ \x01\x03\x04\x1a\x7f\t\r\n tab;")
	}
	body.WriteString(" trailing;")
	paste := []byte(pasteStart + body.String() + pasteEnd)

	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < len(paste); i += 7000 { // several writes, in order
		if err := c.Input(paste[i:min(i+7000, len(paste))]); err != nil {
			// What the host and the reader say about why (#155).
			term := read(t, dir, TerminalLog)
			t.Fatalf("input at byte %d: %v\nhost.log:\n%s\nend of the terminal log: %q", i, err, read(t, dir, HostLog), term[max(0, len(term)-500):])
		}
	}
	// "done" comes after the file is written. The file exists as soon as it
	// is created, so waiting for it alone can read it half written.
	waitFor(t, "the reader to finish", func() bool { return bytes.Contains(read(t, dir, TerminalLog), []byte("done")) })
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, paste) {
		t.Fatalf("the command read %d bytes, want %d identical bytes", len(got), len(paste))
	}
}

func TestInputInOrderAndSecondClient(t *testing.T) {
	dir := startHost(t, Spec{Argv: self("echo"), Env: []string{}})
	for client := range 2 {
		c, err := Dial(dir)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 50 {
			if err := c.Input(fmt.Appendf(nil, "c%d-%d\n", client, i)); err != nil {
				t.Fatal(err)
			}
		}
		c.Close()
	}
	var want strings.Builder
	for client := range 2 {
		for i := range 50 {
			fmt.Fprintf(&want, "got c%d-%d\n", client, i)
		}
	}
	waitFor(t, "every echo", func() bool { return string(read(t, dir, EventsFile)) == want.String() })
}

func TestHostOutlivesItsStarter(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint("terminal=", terminal), func(t *testing.T) {
			dir := t.TempDir()
			t.Cleanup(func() { killGroups(dir) })
			starter := exec.Command(os.Args[0], "role", "starter", dir, fmt.Sprint(terminal))
			out, _ := starter.StdoutPipe()
			if err := starter.Start(); err != nil {
				t.Fatal(err)
			}
			line, _ := bufio.NewReader(out).ReadString('\n')
			if line != "started\n" {
				starter.Process.Kill()
				starter.Wait()
				t.Fatalf("starter: %q", line)
			}
			starter.Process.Signal(syscall.SIGKILL)
			starter.Wait()

			logFile := EventsFile
			if terminal {
				logFile = TerminalLog
			}
			waitFor(t, "the host's socket", func() bool { _, err := os.Stat(SocketPath(dir)); return err == nil })
			c, err := Dial(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if err := c.Input([]byte("after the starter died\n")); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the command's answer", func() bool {
				return bytes.Contains(read(t, dir, logFile), []byte("got after the starter died"))
			})
		})
	}
}

func TestExitIsRecordedAndSocketRemoved(t *testing.T) {
	dir := startHost(t, Spec{Argv: self("exit", "7"), Env: []string{}})
	waitFor(t, "the status", func() bool { _, ok, _ := ReadStatus(dir); return ok })
	st, _, err := ReadStatus(dir)
	if err != nil || !st.Exited || st.Code != 7 {
		t.Fatalf("status %+v, %v", st, err)
	}
	if _, err := os.Stat(SocketPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("socket after exit: %v", err)
	}
}

// The package reads nothing under /proc.
func TestNoProc(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		if b, _ := os.ReadFile(f); bytes.Contains(b, []byte("/proc")) {
			t.Errorf("%s mentions /proc", f)
		}
	}
}

// A state directory too deep for a socket: the host and a caller with
// another environment agree on the short path.
func TestDeepStateDirSocket(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("deep/", 20))
	os.MkdirAll(dir, 0o700)
	// Stopped, not killed, so that the host removes its socket directory.
	t.Cleanup(func() { Stop(dir, time.Second, time.Second); killGroups(dir) })
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // the caller's; the host has none
	t.Setenv("TMPDIR", t.TempDir())
	if _, err := Start(hostCommand(), dir, Spec{Argv: self("echo"), Env: []string{}}); err != nil {
		t.Fatal(err)
	}
	want := strings.TrimPrefix(HookEnv(dir), HookEnvVar+"=")
	if SocketPath(dir) != want || len(want) > maxSocketLen {
		t.Fatalf("host listens at %s, hooks are told %s", SocketPath(dir), want)
	}
	if _, err := Call(want, []byte(`{"x":1}`), false, 5*time.Second); err != nil {
		t.Fatalf("a hook with the caller's path: %v", err)
	}
}

// waitGoneT waits until the host of dir has recorded its end.
func waitGoneT(t *testing.T, dir string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !Gone(dir); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the host did not end")
		}
	}
}

// The directory a host makes for its socket outside a deep state directory
// goes when the host ends; after a killed host, the next host takes it over
// and removes it when it ends.
func TestSocketDirectoryIsRemoved(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("deep/", 20))
	os.MkdirAll(dir, 0o700)
	t.Cleanup(func() { killGroups(dir) })
	sockDir := filepath.Dir(defaultSocket(dir))
	if sockDir == dir {
		t.Fatal("the state directory is not deep enough for the test")
	}
	start := func(args ...string) {
		t.Helper()
		os.Remove(filepath.Join(dir, StatusFile))
		if _, err := Start(hostCommand(), dir, Spec{Argv: self(args...), Env: []string{}}); err != nil {
			t.Fatal(err)
		}
	}
	start("exit", "0")
	waitGoneT(t, dir)
	if _, err := os.Stat(sockDir); !os.IsNotExist(err) {
		t.Fatalf("after a clean end the socket directory is there: %v", err)
	}

	start("echo") // runs until killed
	// The host dies first, with its socket in place, then its command.
	b, _ := os.ReadFile(filepath.Join(dir, HostPIDFile))
	if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 1 {
		syscall.Kill(-pid, syscall.SIGKILL)
	}
	time.Sleep(100 * time.Millisecond)
	killGroups(dir)
	if _, err := os.Stat(filepath.Join(sockDir, SocketFile)); err != nil {
		t.Fatalf("a killed host's socket: %v", err)
	}
	start("exit", "0")
	waitGoneT(t, dir)
	if _, err := os.Stat(sockDir); !os.IsNotExist(err) {
		t.Fatalf("after the next host ended the socket directory is there: %v", err)
	}
}
