package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/hostterm"
	"github.com/yousan/hataori/internal/wire"
)

// orphanTerminal is `<copy of the test binary> orphan-terminal FILE`: it
// starts a command on a terminal of its own, writes its own pid and the
// command's to FILE, and holds the terminal until it is killed. The test
// starts it from a shell that exits at once, so neither process is under
// hataori's own processes, and the copy has another name than the test
// binary, so the host's terminal list does not take it for hataori's.
func orphanTerminal(file string) int {
	// Its own pid first, so the test can end it whatever happens next.
	if err := os.WriteFile(file+".holder", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return 1
	}
	cmd := exec.Command("sleep", "600")
	f, err := pty.Start(cmd)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer f.Close()
	if err := os.WriteFile(file+".tmp", []byte(fmt.Sprintf("%d %d", os.Getpid(), cmd.Process.Pid)), 0o600); err != nil {
		return 1
	}
	os.Rename(file+".tmp", file)
	go func() { // drain the terminal; nothing is written to it
		buf := make([]byte, 256)
		for {
			if _, err := f.Read(buf); err != nil {
				return
			}
		}
	}()
	cmd.Wait()
	return 0
}

// hostTerminalRig is a tmux server of the test's own with one pane, and a
// terminal outside tmux, both removed when the test ends.
type hostTerminalRig struct {
	socket  []string // what comes before every tmux command for this server
	pane    string   // the pane's id, such as %0
	device  string   // the pane's terminal
	outside string   // the terminal outside tmux
	input   string   // a file holding every byte the pane's command gets as input
}

func newHostTerminalRig(t *testing.T) hostTerminalRig {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("no tmux")
		}
		t.Skip("no tmux")
	}
	r := hostTerminalRig{socket: []string{"-L", fmt.Sprintf("hataori-dash-test-%d", os.Getpid()), "-f", "/dev/null"}}
	tmux := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("tmux", append(append([]string{}, r.socket...), args...)...)
		cmd.Env = append(os.Environ(), "TMUX=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// The cleanup comes first, so a failure below leaves nothing behind:
	// only this test's own server, and its socket.
	t.Cleanup(func() {
		path := exec.Command("tmux", append(append([]string{}, r.socket...), "display-message", "-p", "#{socket_path}")...)
		path.Env = append(os.Environ(), "TMUX=")
		out, _ := path.Output()
		kill := exec.Command("tmux", append(append([]string{}, r.socket...), "kill-server")...)
		kill.Env = append(os.Environ(), "TMUX=")
		kill.Run()
		if p := strings.TrimSpace(string(out)); p != "" {
			os.Remove(p)
		}
	})
	// The pane's command keeps every byte it gets as input in a file, so a
	// test sees any that reached it, echoed or not.
	r.input = filepath.Join(t.TempDir(), "pane-input")
	tmux("new-session", "-d", "-s", "work", "-n", "editor", "-x", "80", "-y", "24",
		fmt.Sprintf(`sh -c 'printf "pane-marker 幅の広い文字\n"; exec cat > "%s"'`, r.input))
	got := strings.Fields(tmux("list-panes", "-a", "-F", "#{pane_id} #{pane_tty}"))
	if len(got) != 2 {
		t.Fatalf("panes: %q", got)
	}
	r.pane, r.device = got[0], got[1]

	dir := t.TempDir()
	holder := filepath.Join(dir, "terminal-holder")
	b, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(holder, b, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "pids")
	// The cleanup comes first too: it ends the holder and its command from
	// what they wrote, whatever got written.
	t.Cleanup(func() {
		for _, f := range []string{file, file + ".holder"} {
			b, _ := os.ReadFile(f)
			for _, w := range strings.Fields(string(b)) {
				if pid, err := strconv.Atoi(w); err == nil && pid > 1 {
					syscall.Kill(pid, syscall.SIGKILL) // only the processes it started
				}
			}
		}
	})
	if out, err := exec.Command("sh", "-c", `"$0" orphan-terminal "$1" </dev/null >/dev/null 2>&1 &`, holder, file).CombinedOutput(); err != nil {
		t.Fatalf("terminal holder: %v\n%s", err, out)
	}
	var holderPID, pid int
	eventually(t, "the terminal outside tmux", 10*time.Second, func() bool {
		b, err := os.ReadFile(file)
		if err != nil {
			return false
		}
		_, err = fmt.Sscan(string(b), &holderPID, &pid)
		return err == nil && pid > 1
	})
	_ = holderPID
	out, _ := exec.Command("ps", "-o", "tty=", "-p", strconv.Itoa(pid)).Output()
	tty := strings.TrimSpace(string(out))
	if tty == "" || tty == "?" || tty == "??" {
		t.Fatalf("the terminal outside tmux has no device: %q", out)
	}
	r.outside = "/dev/" + tty
	return r
}

// hostTerminals reads the route: its status, body and list.
func hostTerminalsOf(t *testing.T, base string, c dashSession) (int, string, []control.HostTerminal) {
	t.Helper()
	code, body := dashGet(t, base, c, "/api/hostterminals")
	var ts []control.HostTerminal
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &ts); err != nil {
			t.Fatalf("/api/hostterminals: %s: %v", body, err)
		}
	}
	return code, body, ts
}

// The daemon tells hataori's own processes by every name of its binary: those
// of links next to it and of the link the daemon was started through (#575).
func TestDaemonKnowsTheNamesOfItsBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	next := filepath.Join(filepath.Dir(exe), fmt.Sprintf("hataori-next-%d", os.Getpid()))
	as := filepath.Join(t.TempDir(), "hataori-started-as")
	for _, link := range []string{next, as} {
		if err := os.Symlink(exe, link); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(link) })
	}
	arg := os.Args[0]
	os.Args[0] = as
	t.Cleanup(func() { os.Args[0] = arg })
	got := make(chan []string, 1)
	hostTermsHook = func(l *hostterm.Lister) { got <- l.Own }
	t.Cleanup(func() { hostTermsHook = nil })
	root := terminalRoot(t)
	setConfig(t, root, func(cfg map[string]any) { cfg["host_terminals"] = true })
	runDaemon(t, root)
	own := <-got
	for _, path := range []string{exe, next, as} {
		if !slices.Contains(own, filepath.Base(path)) {
			t.Errorf("the daemon's names %q lack %q", own, filepath.Base(path))
		}
	}
}

// The host terminals route answers the control API's HostTerminals for the
// owner with the feature on: a real tmux pane and a real terminal outside
// tmux, and not a session host's terminal; one fixed 403 when the feature
// is off or the caller is not the owner; 401 signed out; a foreign Host or
// Origin refused; nothing but GET or HEAD.
func TestDashboardHostTerminalsRoute(t *testing.T) {
	rig := newHostTerminalRig(t)
	hostTermsHook = func(l *hostterm.Lister) { l.Tmux = rig.socket }
	t.Cleanup(func() { hostTermsHook = nil })
	root := terminalRoot(t)
	setConfig(t, root, func(cfg map[string]any) { cfg["host_terminals"] = true })
	stop := runDaemon(t, root)
	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	id, err := c.Start(ctx, control.StartRequest{Key: "k1", Scope: "s", Thread: "t1", Repo: bareRepo(t), Input: control.Input{Text: "reply one"}})
	if err != nil {
		t.Fatal(err)
	}
	var session string
	eventually(t, "the thread with its terminal", 20*time.Second, func() bool {
		d, err := c.Thread(ctx, id, 0, 0)
		session = d.Terminal
		return err == nil && d.State == control.Idle && session != ""
	})
	base, cookie := dashSignIn(t, root)

	// 1. The owner, the feature on: the list HostTerminals gives.
	var got []control.HostTerminal
	eventually(t, "the pane and the terminal outside tmux", 20*time.Second, func() bool {
		code, _, ts := hostTerminalsOf(t, base, cookie)
		got = ts
		has := func(dev string) bool {
			return slices.ContainsFunc(ts, func(h control.HostTerminal) bool { return h.Device == dev })
		}
		return code == http.StatusOK && has(rig.device) && has(rig.outside)
	})
	want, err := c.HostTerminals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The test's own pane and terminal, field by field, as HostTerminals
	// gives them. The host's other terminals may come and go between the
	// two reads, so they are not compared.
	find := func(ts []control.HostTerminal, dev string) string {
		for _, h := range ts {
			if h.Device == dev {
				b, _ := json.Marshal(h)
				return string(b)
			}
		}
		return ""
	}
	for _, dev := range []string{rig.device, rig.outside} {
		if g, w := find(got, dev), find(want, dev); g == "" || g != w {
			t.Fatalf("%s: the route gives %s, HostTerminals %s", dev, g, w)
		}
	}
	for _, h := range got {
		switch h.Device {
		case rig.device:
			if h.Tmux == nil || h.Tmux.Pane != rig.pane || h.Tmux.Session != "work" || h.Tmux.WindowName != "editor" {
				t.Fatalf("the pane: %+v %+v", h, h.Tmux)
			}
		case rig.outside:
			if h.Tmux != nil || h.Process != "sleep" {
				t.Fatalf("the terminal outside tmux: %+v %+v", h, h.Tmux)
			}
		case session:
			t.Fatalf("the session host's terminal %s is listed: %+v", session, h)
		}
	}

	// 3. Signed out, an unknown Host, a foreign Origin.
	u, _ := url.Parse(base)
	if code, _ := rawGet(t, "GET", base, "/api/hostterminals", "", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("signed out: %d, want 401", code)
	}
	if code, body := rawGet(t, "GET", base, "/api/hostterminals", cookie, "elsewhere.example:80", ""); code != http.StatusForbidden || strings.Contains(body, "/dev/") {
		t.Fatalf("an unknown Host: %d %s", code, body)
	}
	if code, body := rawGet(t, "GET", base, "/api/hostterminals", cookie, u.Host, "https://elsewhere.example"); code != http.StatusForbidden || strings.Contains(body, "/dev/") {
		t.Fatalf("a foreign Origin: %d %s", code, body)
	}

	// 5. Nothing but GET or HEAD.
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if code, body := rawGet(t, m, base, "/api/hostterminals", cookie, "", ""); code != http.StatusMethodNotAllowed || strings.Contains(body, "/dev/") {
			t.Fatalf("%s: %d %s, want 405", m, code, body)
		}
	}
	if code, _ := rawGet(t, "HEAD", base, "/api/hostterminals", cookie, "", ""); code != http.StatusOK {
		t.Fatalf("HEAD: %d", code)
	}

	// 2. Not the owner: one fixed 403.
	dashboardHook = func(cl *control.Caller) { cl.Scopes, cl.Principal = []string{"s"}, "grace" }
	t.Cleanup(func() { dashboardHook = nil })
	codeOther, bodyOther, _ := hostTerminalsOf(t, base, cookie)
	if codeOther != http.StatusForbidden || bodyOther != http.StatusText(http.StatusForbidden)+"\n" {
		t.Fatalf("not the owner: %d %q, want a fixed 403", codeOther, bodyOther)
	}
	dashboardHook = func(cl *control.Caller) { cl.Scopes = []string{"s"} }

	// Off: the same 403.
	c.Close()
	stop()
	setConfig(t, root, func(cfg map[string]any) { cfg["host_terminals"] = false })
	runDaemon(t, root)
	base, cookie = dashSignIn(t, root)
	codeOff, bodyOff, _ := hostTerminalsOf(t, base, cookie)
	if codeOff != http.StatusForbidden || bodyOff != bodyOther {
		t.Fatalf("off: %d %q; not the owner: %d %q; want the same 403", codeOff, bodyOff, codeOther, bodyOther)
	}
}
