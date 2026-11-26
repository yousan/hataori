package hostterm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/binary"
	"slices"
)

// TestStandIn is the program the tests below run in a pane, in place of a
// hataori command such as `hataori attach`: it writes what os.Executable()
// gives it, and waits.
func TestStandIn(t *testing.T) {
	out := os.Getenv("HOSTTERM_STANDIN")
	if out == "" {
		t.Skip("only as the stand-in of TestCheckFindsHataoriUnderEachOfItsNames")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out+".tmp", []byte(exe), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(out+".tmp", out); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Minute)
}

// copySelf copies the test's binary to path.
func copySelf(t *testing.T, path string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// With the layout of design section 6.1, "one binary per version, and a link
// to the one in use", a process is named after the file or the link, as it
// was started, whichever of the two the daemon was (#575). A pane that runs
// one, as `hataori attach` does, must be found hataori's: by the process
// table, and by the command tmux reports, which alone is read before each
// batch of keys.
func TestCheckFindsHataoriUnderEachOfItsNames(t *testing.T) {
	sock, tmux := ownTmux(t)
	bin := t.TempDir()
	version := filepath.Join(bin, "hataori-0123456789ab")
	copySelf(t, version)
	link := filepath.Join(bin, "hataori")
	if err := os.Symlink(filepath.Base(version), link); err != nil {
		t.Fatal(err)
	}
	for i, started := range []string{link, version, "./hataori"} {
		t.Run(fmt.Sprint(i, filepath.Base(started)), func(t *testing.T) {
			said := filepath.Join(bin, fmt.Sprintf("said%d", i))
			name := fmt.Sprintf("own%d", i)
			if _, err := tmux("new-session", "-d", "-s", name, "-c", bin,
				fmt.Sprintf("exec env HOSTTERM_STANDIN='%s' '%s' -test.run '^TestStandIn$'", said, started)); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the stand-in", func() bool { _, err := os.Stat(said); return err == nil })
			exe, err := os.ReadFile(said)
			if err != nil {
				t.Fatal(err)
			}
			pane, err := tmux("display-message", "-p", "-t", name+":0", "#{pane_id}")
			if err != nil {
				t.Fatal(err)
			}
			pane = strings.TrimSpace(pane)
			ctx := context.Background()
			command := ""
			waitFor(t, "the pane's command", func() bool {
				ps, _ := (&Lister{Tmux: sock}).panes(ctx)
				for _, p := range ps {
					if p.Tmux.Pane == pane {
						command = p.Process
					}
				}
				return command != "" && command != "env" && !strings.HasSuffix(command, "sh")
			})
			// What a daemon may take for its own path, however it was started:
			// os.Executable() follows the link on Linux and not on macOS.
			for _, own := range []string{string(exe), link, version} {
				names, err := binary.Names(own)
				if err != nil {
					t.Fatal(err)
				}
				l := &Lister{Tmux: sock, Own: names}
				for _, procs := range []bool{true, false} {
					if p, isOwn, err := l.Check(ctx, pane, nil, procs); err != nil || p == nil || !isOwn {
						t.Errorf("the daemon is %s, the pane's command %q, with the process table %v: Check %+v %v %v", own, command, procs, p, isOwn, err)
					}
				}
			}
		})
	}
}

func TestMatchesOwn(t *testing.T) {
	own := []string{"hataori", "hataori-0123456789ab", ""}
	for comm, want := range map[string]bool{
		"hataori": true, "hataori-0123456789ab": true, "hataori-0123456": true, // Linux's 15 bytes
		"./hataori": true, "bin/hataori": true, "HATAORI": true,
		"": false, "hat": false, "hataori-01234567": false, "hataorid": false, "hataori/sh": false,
	} {
		if got := matchesOwn(comm, own); got != want {
			t.Errorf("%q: %v", comm, got)
		}
	}
}

// Adapt the captured process table to a container: the daemon is PID 1,
// the person's tmux server is its adopted child, and a separate session
// host owns an agent terminal. This tests ownership, not ps parsing.
func TestPIDOneDoesNotOwnAdoptedTerminals(t *testing.T) {
	ps := parsePS(readTestdata(t, "ps-linux-procps-ng-4.0.4.txt"))
	ps[0].comm = "hataori"
	ps[1].ppid = 1
	ps[4].comm, ps[4].tty, ps[4].ppid = "hataori", "", 1
	own := []string{"hataori"}
	pids := ownPIDs(ps, own)
	if !slices.Equal(pids, []int{1248409}) {
		t.Fatalf("own PIDs: %v", pids)
	}
	for _, recorded := range [][]int{nil, {1248409}} {
		hidden := ownedDevices(ps, slices.Concat(recorded, pids))
		if hidden["/dev/pts/62"] || hidden["/dev/pts/63"] {
			t.Errorf("adopted tmux terminals hidden: %v", hidden)
		}
		if !hidden["/dev/pts/64"] {
			t.Errorf("session host's agent terminal exposed: %v", hidden)
		}
	}
}

// Exercise both entry points with captured output adapted to the same
// topology. No tmux server or process-table command runs in this test.
func TestPIDOneListsAndChecksPane(t *testing.T) {
	ps := strings.ReplaceAll(string(readTestdata(t, "ps-linux-procps-ng-4.0.4.txt")), "systemd", "hataori")
	ps = strings.ReplaceAll(ps, "1248395    2146", "1248395       1")
	ps = strings.ReplaceAll(ps, "1248409 1248395 1248409 1248409 pts/64   sh", "1248409       1 1248409      -1 ?        hataori")
	panes := readTestdata(t, "list-panes-tmux-3.4.txt")
	l := &Lister{Own: []string{"hataori"}, run: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
		if name == "ps" {
			return []byte(ps), nil
		}
		if name == "tmux" && args[0] == "list-panes" {
			return panes, nil
		}
		return nil, fmt.Errorf("unexpected command %s %v", name, args)
	}}
	list, err := l.List(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Tmux.Pane != "%0" || list[1].Tmux.Pane != "%1" {
		t.Fatalf("listed: %+v", list)
	}
	for pane, wantOwn := range map[string]bool{"%0": false, "%2": true} {
		p, own, err := l.Check(context.Background(), pane, nil, true)
		if err != nil || p == nil || own != wantOwn {
			t.Errorf("Check(%s): %v %v %v", pane, p, own, err)
		}
	}
	// An attach still refuses input even though its terminal is not under
	// a session host in the process tree.
	ps = strings.ReplaceAll(ps, "pts/63   sh", "pts/63   hataori")
	list, err = l.List(context.Background(), nil)
	if err != nil || len(list) != 1 || list[0].Tmux.Pane != "%0" {
		t.Fatalf("attach listed: %+v, %v", list, err)
	}
	if p, own, err := l.Check(context.Background(), "%1", nil, true); err != nil || p == nil || !own {
		t.Fatalf("attach: %v %v %v", p, own, err)
	}
}
