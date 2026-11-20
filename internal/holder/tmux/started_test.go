package tmux

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/holder"
)

// How the panes of a window hataori started read, from what list-panes
// printed in paneFormat (testdata/list-panes-dead-3.5a.txt, tmux 3.5a): a
// pane that runs, dead panes whose process exited with 0 and 3, which a
// tmux condition would read as false for 0, one a SIGTERM ended, and a
// session name with spaces. The server's process id and the terminals are
// replaced.
func TestPanesOfAWindowHataoriStarted(t *testing.T) {
	b, err := os.ReadFile("testdata/list-panes-dead-3.5a.txt")
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		dead    bool
		code    int
		sig     string
		session string
	}
	wants := map[string]want{
		"%4": {false, 0, "", "a_person's_window"},
		"%0": {false, 0, "", "hataori-x-1"},
		"%1": {true, 0, "", "hataori-x-1"},
		"%2": {true, 3, "", "hataori-x-1"},
		"%3": {true, -1, "terminated", "hataori-x-1"},
	}
	n := 0
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		server, id, p, ok := parsePane(l)
		if !ok || server != 4242 || p.pid <= 0 || p.tty != "/dev/ttys009" {
			t.Fatalf("%q: %d %s %+v %v", l, server, id, p, ok)
		}
		w, ok := wants[id]
		if !ok || p.dead != w.dead || p.session != w.session {
			t.Fatalf("%s: %+v", id, p)
		}
		n++
		if !p.dead {
			continue
		}
		st := statusOf(p)
		if !st.Exited || st.Code != w.code || st.Signal != w.sig {
			t.Errorf("%s: %+v, want code %d signal %q", id, st, w.code, w.sig)
		}
	}
	if n != len(wants) {
		t.Fatalf("%d panes read, want %d", n, len(wants))
	}
}

// A signal by its number, as an older tmux prints it, is named as the own
// host names it.
func TestSignalByNumber(t *testing.T) {
	if s := signalName("15"); s != "terminated" {
		t.Fatalf("%q", s)
	}
}

// A window whose server ended is let go of without touching the server
// that answers on its socket now, whose pane may have the same id.
func TestRecordLeavesANewServerAlone(t *testing.T) {
	prog, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("no tmux")
	}
	sock := filepath.Join(t.TempDir(), "s")
	on := func(args ...string) *exec.Cmd {
		return exec.Command(prog, append([]string{"-S", sock, "-f", "/dev/null"}, args...)...)
	}
	t.Cleanup(func() { on("kill-server").Run() })
	ended := exec.Command("true")
	if err := ended.Run(); err != nil {
		t.Fatal(err)
	}
	out, err := on("new-session", "-d", "-s", "a person's", "-P", "-F", "#{pane_id}", "exec sleep 600").Output()
	if err != nil {
		t.Fatal(err)
	}
	pane := strings.TrimSpace(string(out))
	dir := t.TempDir()
	rb, _ := json.Marshal(ref{Socket: sock, Server: ended.ProcessState.Pid(), Pane: pane, Dir: dir})
	h := &Holder{Tmux: prog, Prefix: "hataori-t-"}
	if err := h.Record(context.Background(), holder.Window{Holder: Name, Ref: string(rb), Owned: true}); err != nil {
		t.Fatal(err)
	}
	if out, err := on("list-panes", "-a", "-F", "#{pane_id}").Output(); err != nil || strings.TrimSpace(string(out)) != pane {
		t.Fatalf("the new server's pane after Record: %q %v", out, err)
	}
}
