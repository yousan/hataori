package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/holder"
	"github.com/yousan/hataori/internal/host"
)

// How the panes of a window hataori started read, from what list-panes
// printed in paneFormat (testdata/list-panes-dead-3.5a.txt, tmux 3.5a; -3.8,
// where a dead pane has no process): a pane that runs, dead panes whose
// process exited with 0 and 3, which a tmux condition would read as false
// for 0, one a SIGTERM ended, and a session name with spaces. The server's
// process id and the terminals are replaced.
func TestPanesOfAWindowHataoriStarted(t *testing.T) {
	for _, v := range []string{"3.5a", "3.8"} {
		t.Run(v, func(t *testing.T) { testPanes(t, v) })
	}
}

func testPanes(t *testing.T, version string) {
	b, err := os.ReadFile("testdata/list-panes-dead-" + version + ".txt")
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
		if !ok || server != 4242 || (p.pid > 0) == (p.dead && version == "3.8") || p.tty != "/dev/ttys009" {
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

// A signal by its number, as an older tmux prints it, or by a name with
// "SIG", is named as the own host names it.
func TestSignalNames(t *testing.T) {
	for in, want := range map[string]string{"15": "terminated", "SIGKILL": "killed", "kill": "killed"} {
		if s := signalName(in); s != want {
			t.Errorf("%q: %q, want %q", in, s, want)
		}
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
	out, err := on("new-session", "-d", "-s", "a person's", "-P", "-F", "#{pane_id} #{pane_pid}", "exec sleep 600").Output()
	if err != nil {
		t.Fatal(err)
	}
	id, spid, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	pid, err := strconv.Atoi(spid)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rb, _ := json.Marshal(ref{Socket: sock, Server: ended.ProcessState.Pid(), Pane: id, Dir: dir})
	h := &Holder{Tmux: prog, Prefix: "hataori-t-"}
	if err := h.Record(context.Background(), holder.Window{Holder: Name, Ref: string(rb), Owned: true}); err != nil {
		t.Fatal(err)
	}
	// Also when the new server came after the last look at the pane.
	if err := h.removePane(context.Background(), ref{Socket: sock, Server: ended.ProcessState.Pid(), Pane: id, Dir: dir}, pane{pid: pid}); err != nil {
		t.Fatal(err)
	}
	if out, err := on("list-panes", "-a", "-F", "#{pane_id}").Output(); err != nil || strings.TrimSpace(string(out)) != id {
		t.Fatalf("the new server's pane after Record: %q %v", out, err)
	}
}

// A session a start left before its respawn, whose pane still runs the
// placeholder, is cleared by the next start; a person's session beside it
// stays.
func TestALeftoverBeforeTheRespawnIsCleared(t *testing.T) {
	prog, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("no tmux")
	}
	sock := filepath.Join(t.TempDir(), "s")
	server := []string{"-S", sock, "-f", "/dev/null"}
	on := func(args ...string) *exec.Cmd {
		return exec.Command(prog, append(append([]string{}, server...), args...)...)
	}
	t.Cleanup(func() { on("kill-server").Run() })
	dir := filepath.Join(t.TempDir(), "7")
	h := &Holder{Tmux: prog, Prefix: "hataori-t-", Server: server}
	if err := on("new-session", "-d", "-s", "hataori-t-7", placeholder).Run(); err != nil {
		t.Fatal(err)
	}
	if err := on("new-session", "-d", "-s", "theirs", placeholder).Run(); err != nil {
		t.Fatal(err)
	}
	h.clearLeftover(context.Background(), dir)
	out, err := on("list-sessions", "-F", "#{session_name}").Output()
	if err != nil || strings.TrimSpace(string(out)) != "theirs" {
		t.Fatalf("sessions after clearing: %q %v", out, err)
	}
}

// started is a window as Start makes it, in a session of its own, with
// program in it, marked as Start's: its pane, that pane's process, and its state directory.
func (s *server) started(h *Holder, program string) (holder.Window, string, string) {
	s.t.Helper()
	dir := filepath.Join(s.dir, "7")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.t.Fatal(err)
	}
	out := s.run("new-session", "-d", "-s", h.Prefix+"7", "-P", "-F", "#{pane_id} #{pid} #{pane_pid}"+formatEnd, program+" # exec "+quote(dir))
	whole, ok := strings.CutSuffix(out, formatDone)
	if !ok {
		s.t.Fatalf("incomplete new-session format: %q", out)
	}
	f := strings.Fields(whole)
	if len(f) != 3 {
		s.t.Fatalf("new-session: %q", f)
	}
	s.run("set-option", "-w", "-t", f[0], "remain-on-exit", "on")
	server, _ := strconv.Atoi(f[1])
	pid, _ := strconv.Atoi(f[2])
	rb, _ := json.Marshal(ref{Socket: s.sock, Server: server, Pane: f[0], Dir: dir})
	return holder.Window{Holder: Name, Ref: string(rb), Owned: true, Process: pid}, f[0], dir
}

// panes is each pane of the server with its process and its session.
func (s *server) panes() map[string]string {
	s.t.Helper()
	out, err := exec.Command(s.tmux, "-S", s.sock, "list-panes", "-a", "-F", "#{pane_id} #{pane_pid} #{s/[ \r\n]/_/:session_name}"+formatEnd).Output()
	m := map[string]string{}
	if err != nil && len(out) == 0 {
		return m
	}
	for _, l := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		whole, ok := strings.CutSuffix(l, formatDone)
		if !ok {
			s.t.Fatalf("incomplete pane listing: %q", out)
		}
		if f := strings.SplitN(whole, " ", 2); len(f) == 2 {
			m[f[0]] = f[1]
		}
	}
	return m
}

// A person who switched to the window hataori started may open windows and
// splits there: they are in hataori's session, and in the agent's window.
// Ending the agent's window, however it ends, leaves them and their
// programs as they are, and a session of theirs; a session that holds only
// the agent's pane goes with it.
func TestThePersonsWindowsInTheSessionStay(t *testing.T) {
	for _, theirs := range []bool{true, false} {
		for how, end := range map[string]func(s *server, h *Holder, w holder.Window, dir string){
			"Stop": func(s *server, h *Holder, w holder.Window, dir string) {
				if _, err := h.Stop(context.Background(), w, time.Second, time.Second); err != nil {
					s.t.Error(err)
				}
			},
			"Record": func(s *server, h *Holder, w holder.Window, dir string) {
				if err := h.Record(context.Background(), w); err != nil {
					s.t.Error(err)
				}
			},
			"a window opened during Record": func(s *server, h *Holder, w holder.Window, dir string) {
				if !theirs {
					s.t.Skip()
				}
				late := &Holder{Prefix: h.Prefix, Tmux: s.before(2, s.tmux+" -S "+s.sock+" new-window -d -t ="+h.Prefix+"7: 'exec sleep 604'")}
				if err := late.Record(context.Background(), w); err != nil {
					s.t.Error(err)
				}
				if !strings.Contains(s.run("list-panes", "-a", "-F", "#{pane_start_command}"), "sleep 604") {
					s.t.Error("the window opened meanwhile went")
				}
			},
			"a leftover of an earlier run": func(s *server, h *Holder, w holder.Window, dir string) { h.clearLeftover(context.Background(), dir) },
			"a window opened while a leftover is cleared": func(s *server, h *Holder, w holder.Window, dir string) {
				if !theirs {
					s.t.Skip()
				}
				late := &Holder{Prefix: h.Prefix, Server: h.Server, Tmux: s.before(2, s.tmux+" -S "+s.sock+" new-window -d -t ="+h.Prefix+"7: 'exec sleep 604'")}
				late.clearLeftover(context.Background(), dir)
				if !strings.Contains(s.run("list-panes", "-a", "-F", "#{pane_start_command}"), "sleep 604") {
					s.t.Error("the window opened meanwhile went")
				}
			},
		} {
			t.Run(fmt.Sprintf("%s, theirs %v", how, theirs), func(t *testing.T) {
				s := newServer(t)
				h := &Holder{Prefix: "hataori-t-", Server: []string{"-S", s.sock}}
				s.run("new-session", "-d", "-s", "hataori-t-72", "exec sleep 600") // another session's
				w, agent, dir := s.started(h, "exec sleep 600")
				want := s.panes()
				delete(want, agent)
				if theirs {
					s.run("new-window", "-d", "-t", "=hataori-t-7:", "exec sleep 601")
					s.run("split-window", "-d", "-t", agent, "exec sleep 602")
					s.run("new-session", "-d", "-s", "mine", "-t", "=hataori-t-7") // a session that shares its windows
					s.run("new-window", "-d", "-t", "=mine:", "exec sleep 603")
					s.run("new-session", "-d", "-s", "other", "-n", "hataori-t-7", "exec sleep 605") // a window of that name
					want = s.panes()
					delete(want, agent)
				}
				end(s, h, w, dir)
				got := s.panes()
				if v, ok := got[agent]; ok {
					t.Errorf("the agent's pane is still there: %s", v)
				}
				for id, v := range want {
					if got[id] != v {
						t.Errorf("the pane %s was %q, is now %q", id, v, got[id])
					}
					pid, _, _ := strings.Cut(v, " ")
					if n, _ := strconv.Atoi(pid); syscall.Kill(n, 0) != nil {
						t.Errorf("the process %s of pane %s was ended", pid, id)
					}
				}
				if _, err := exec.Command(s.tmux, "-S", s.sock, "has-session", "-t", "=hataori-t-7").Output(); (err == nil) != theirs {
					t.Errorf("hataori's session: %v", err)
				}
			})
		}
	}
}

// Start sets up only the pane it made: not a person's of the same id, on a
// server started again meanwhile, and not one a person gave a program.
func TestStartSetsUpOnlyThePaneItMade(t *testing.T) {
	for how, do := range map[string]string{
		"the server started again": "%[1]s kill-server; while %[1]s has-session 2> /dev/null; do sleep 0.02; done; %[1]s -f /dev/null new-session -d -s person 'exec sleep 611'",
		"a person's respawn":       "%[1]s respawn-pane -k -t =hataori-t-7: 'exec sleep 611'",
	} {
		t.Run(how, func(t *testing.T) {
			s := newServer(t)
			h := s.starter()
			dir := filepath.Join(s.dir, "7")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			look := "#{pane_id} #{pane_pid} #{pane_pipe} #{pane_dead} #{pane_start_command}"
			then := filepath.Join(s.dir, "then")
			h.Tmux = s.before(3, fmt.Sprintf(do, s.tmux+" -S "+s.sock)+"; "+s.tmux+" -S "+s.sock+" list-panes -a -F '"+look+"' > "+then)
			if w, err := h.Start(t.Context(), dir, sleeper); !errors.Is(err, errGone) {
				t.Errorf("Start: %+v %v", w, err)
			}
			want, _ := os.ReadFile(then)
			if !strings.Contains(string(want), "sleep 611") {
				t.Fatalf("not the person's: %q", want)
			}
			if got := s.run("list-panes", "-a", "-F", look); got != strings.TrimSpace(string(want)) {
				t.Errorf("the person's pane was %q, is %q", want, got)
			}
			for _, scope := range []string{"-p", "-w"} {
				if got := s.run("show-options", scope, "-t", "%0", "remain-on-exit"); got != "" {
					t.Errorf("set on the person's pane: %q", got)
				}
			}
		})
	}
}

// A person's hooks on the commands Start uses may print anything, also what
// hataori prints itself when the pane is not the one it made.
func TestStartBesideAPersonsHooks(t *testing.T) {
	s := newServer(t)
	s.run("new-session", "-d", "-s", "person", "exec sleep 600")
	s.run("set-hook", "-g", "after-set-option", "display-message -p "+notDone)
	h := s.starter()
	dir := filepath.Join(s.dir, "7")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	w, err := h.Start(t.Context(), dir, sleeper)
	if err != nil {
		t.Fatal(err)
	}
	if live, err := h.Alive(t.Context(), w); err != nil || live.State != holder.Running {
		t.Errorf("the agent: %+v %v", live, err)
	}
	if _, err := h.Stop(t.Context(), w, time.Second, time.Second); err != nil {
		t.Error(err)
	}
}

// A person's hook may make tmux wait between Start's commands: the pane a
// person gave a program meanwhile keeps it.
func TestStartWhileAPersonsHookWaits(t *testing.T) {
	s := newServer(t)
	s.run("new-session", "-d", "-s", "person", "exec sleep 600")
	s.run("set-hook", "-g", "after-pipe-pane", "run-shell '"+s.tmux+" -S "+s.sock+" respawn-pane -k -t =hataori-t-7: \"exec sleep 611\"'")
	h := s.starter()
	dir := filepath.Join(s.dir, "7")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if w, err := h.Start(t.Context(), dir, sleeper); !errors.Is(err, errGone) {
		t.Errorf("Start: %+v %v", w, err)
	}
	if got := s.run("list-panes", "-t", "=hataori-t-7:", "-F", "#{pane_dead} #{pane_start_command}"); !strings.Contains(got, "0 ") || !strings.Contains(got, "sleep 611") {
		t.Errorf("the person's pane is %q", got)
	}
}

// A person may give the agent's pane a program of their own (respawn-pane):
// it is theirs then. The agent's window is gone; the program is not typed
// to, not signalled, and its pane stays, also when that happens right before
// the pane is removed.
func TestAPaneAPersonRespawnedIsTheirs(t *testing.T) {
	newPane := func(t *testing.T) (*server, *Holder, holder.Window, string, func()) {
		s := newServer(t)
		h := &Holder{Prefix: "hataori-t-"}
		w, pane, _ := s.started(h, "exec sleep 600")
		return s, h, w, pane, func() { s.run("respawn-pane", "-k", "-t", pane, "exec cat > "+s.dir+"/typed") }
	}
	still := func(s *server, pane string) {
		s.t.Helper()
		time.Sleep(200 * time.Millisecond)
		if got := s.run("list-panes", "-a", "-F", "#{pane_id} #{pane_dead} #{pane_start_command}"); !strings.Contains(got, pane+" 0 ") || !strings.Contains(got, "typed") {
			s.t.Errorf("the person's program: %q", got)
		}
		if b, _ := os.ReadFile(filepath.Join(s.dir, "typed")); len(b) > 0 {
			s.t.Errorf("it read %q", b)
		}
	}
	t.Run("before", func(t *testing.T) {
		s, h, w, pane, respawn := newPane(t)
		respawn()
		if l, err := h.Alive(context.Background(), w); err != nil || l.State != holder.Gone {
			t.Errorf("Alive: %+v %v", l, err)
		}
		if err := h.Input(context.Background(), w, []agent.Part{{Paste: "x"}, {Key: agent.Enter}}); !errors.Is(err, errGone) {
			t.Errorf("Input: %v", err)
		}
		if ch, err := h.Screen(t.Context(), w, "", 0); err != nil {
			t.Errorf("Screen: %v", err)
		} else if c, ok := <-ch; ok {
			t.Errorf("Screen sent %q", c.Data)
		}
		if tty, err := h.Device(context.Background(), w); !errors.Is(err, errGone) {
			t.Errorf("Device: %q %v", tty, err)
		}
		if res, err := h.Stop(context.Background(), w, time.Second, time.Second); err != nil || !res.Already {
			t.Errorf("Stop: %+v %v", res, err)
		}
		if err := h.Record(context.Background(), w); err != nil {
			t.Errorf("Record: %v", err)
		}
		still(s, pane)
	})
	t.Run("before, by a program that looks alike", func(t *testing.T) {
		for _, program := range []string{"exec sleep 600 # exec %s2", "sleep 600 # exec %s", "exec sleep 600 # exec %s; true", "exec sleep 600 # noexec %s"} {
			s, h, w, pane, _ := newPane(t)
			s.run("respawn-pane", "-k", "-t", pane, fmt.Sprintf(program, filepath.Join(s.dir, "7")))
			if l, err := h.Alive(context.Background(), w); err != nil || l.State != holder.Gone {
				t.Errorf("%s: Alive: %+v %v", program, l, err)
			}
		}
	})
	t.Run("before, with a pipe", func(t *testing.T) { // theirs, or opened right before hataori's closes
		for _, late := range []bool{false, true} {
			s := newServer(t)
			h := s.starter()
			dir := filepath.Join(s.dir, "7")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			w, err := h.Start(t.Context(), dir, sleeper)
			if err != nil {
				t.Fatal(err)
			}
			r, _ := parseRef(w)
			pipe := func(runs bool) {
				t.Helper()
				for deadline := time.Now().Add(10 * time.Second); host.PipeRuns(dir) != runs; time.Sleep(10 * time.Millisecond) {
					if time.Now().After(deadline) {
						t.Fatalf("hataori's pipe runs: %v", !runs)
					}
				}
			}
			pipe(true)
			s.run("respawn-pane", "-k", "-t", r.Pane, "exec sleep 601")
			theirs := []string{"pipe-pane", "-O", "-t", r.Pane, "exec cat > /dev/null"}
			if late {
				h.Tmux = s.before(3, s.tmux+" -S "+s.sock+" respawn-pane -k -t "+r.Pane+" 'exec sleep 602' ';' pipe-pane -O -t "+r.Pane+" 'exec cat > /dev/null'")
			} else {
				s.run(theirs...)
				pipe(false)
			}
			if l, err := h.Alive(t.Context(), w); err != nil || l.State != holder.Gone {
				t.Errorf("Alive: %+v %v", l, err)
			}
			if got := s.run("display-message", "-p", "-t", r.Pane, "#{pane_pipe}"); got != "1" {
				t.Errorf("late %v: the person's pipe is gone", late)
			}
		}
	})
	t.Run("before, and it ended", func(t *testing.T) {
		s, h, w, pane, _ := newPane(t)
		s.run("respawn-pane", "-k", "-t", pane, "exec true")
		for deadline := time.Now().Add(10 * time.Second); !strings.HasPrefix(s.how(pane), "1") && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		}
		if l, err := h.Alive(context.Background(), w); err != nil || l.State != holder.Gone {
			t.Errorf("Alive: %+v %v", l, err)
		}
		if err := h.Record(context.Background(), w); err != nil {
			t.Errorf("Record: %v", err)
		}
		if s.panes()[pane] == "" {
			t.Error("the person's pane went")
		}
	})
	// The log of the pane ends, and the next start does not wait for it.
	t.Run("before the next start", func(t *testing.T) {
		s := newServer(t)
		h := s.starter()
		dir := filepath.Join(s.dir, "日本 it's $7")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		w, err := h.Start(t.Context(), dir, sleeper)
		if err != nil {
			t.Fatal(err)
		}
		r, _ := parseRef(w)
		for deadline := time.Now().Add(10 * time.Second); !host.PipeRuns(dir) && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		}
		if !host.PipeRuns(dir) {
			t.Fatal("no pipe runs")
		}
		s.run("respawn-pane", "-k", "-t", r.Pane, "exec cat > '"+s.dir+"/typed'")
		if l, err := h.Alive(t.Context(), w); err != nil || l.State != holder.Gone {
			t.Errorf("Alive: %+v %v", l, err)
		}
		again, err := h.Start(t.Context(), dir, sleeper)
		if err != nil {
			t.Fatalf("the next start: %v", err)
		}
		if l, err := h.Alive(t.Context(), again); err != nil || l.State != holder.Running {
			t.Errorf("Alive: %+v %v", l, err)
		}
		still(s, r.Pane)
	})
	t.Run("without a command", func(t *testing.T) { // the agent again
		s := newServer(t)
		t.Setenv("LC_ALL", "C") // as a daemon's may be
		h := s.starter()
		dir := filepath.Join(s.dir, `a;{~} \"b" it's $HOME;`, "日本 it's $7")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		w, err := h.Start(t.Context(), dir, sleeper)
		if err != nil {
			t.Fatal(err)
		}
		r, _ := parseRef(w)
		s.run("respawn-pane", "-k", "-t", r.Pane)
		if l, err := h.Alive(t.Context(), w); err != nil || l.State != holder.Running {
			t.Errorf("Alive: %+v %v", l, err)
		}
		if err := h.Input(t.Context(), w, []agent.Part{{Key: agent.Escape}}); err != nil {
			t.Errorf("Input: %v", err)
		}
		if res, err := h.Stop(t.Context(), w, 5*time.Second, 5*time.Second); err != nil || res.Already {
			t.Errorf("Stop: %+v %v", res, err)
		}
		if got := s.panes(); len(got) != 0 {
			t.Errorf("panes left: %v", got)
		}
	})
	// Each of the holder's commands comes right after the respawn.
	respawned := func(s *server, pane string, n int) *Holder {
		return &Holder{Prefix: "hataori-t-", Server: []string{"-S", s.sock},
			Tmux: s.before(n, s.tmux+" -S "+s.sock+" respawn-pane -k -t "+pane+" 'exec cat > "+s.dir+"/typed'")}
	}
	// The agent may have ended before.
	ended := func(s *server, pane string) {
		s.run("send-keys", "-t", pane, "C-c")
		for deadline := time.Now().Add(10 * time.Second); !strings.HasPrefix(s.how(pane), "1") && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		}
	}
	for _, dead := range []bool{false, true} {
		t.Run(fmt.Sprintf("during Record, the agent ended: %v", dead), func(t *testing.T) {
			s, _, w, pane, _ := newPane(t)
			n := 2
			if dead {
				ended(s, pane)
				if s.run("display-message", "-p", "-t", pane, "x#{pane_pid}x") == "xx" {
					n = 3 // since 3.8, the look takes a second command
				}
			}
			if err := respawned(s, pane, n).Record(context.Background(), w); err != nil {
				t.Error(err)
			}
			still(s, pane)
		})
		t.Run(fmt.Sprintf("during Stop, the agent ended: %v", dead), func(t *testing.T) {
			for n := 2; n <= 4; n++ {
				s, _, w, pane, _ := newPane(t)
				if dead {
					ended(s, pane)
				}
				if _, err := respawned(s, pane, n).Stop(context.Background(), w, time.Second, time.Second); err != nil && !strings.Contains(err.Error(), "changed as it was looked at") {
					t.Errorf("before command %d: %v", n, err)
				}
				still(s, pane)
			}
		})
	}
	t.Run("at the end of Start", func(t *testing.T) {
		s := newServer(t)
		h := s.starter()
		dir := filepath.Join(s.dir, "7")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		h.Tmux = s.before(4, s.tmux+" -S "+s.sock+" respawn-pane -k -t =hataori-t-7: 'exec cat > "+s.dir+"/typed'")
		if w, err := h.Start(t.Context(), dir, sleeper); !errors.Is(err, errGone) {
			t.Errorf("Start: %+v %v", w, err)
		}
		still(s, "%0")
	})
	t.Run("after a Start whose agent ended at once", func(t *testing.T) {
		s := newServer(t)
		h := s.starter()
		dir := filepath.Join(s.dir, "7")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		h.Tmux = s.before(4, "sleep 1")
		spec := sleeper
		spec.Argv = []string{"true"}
		w, err := h.Start(t.Context(), dir, spec)
		if err != nil {
			t.Fatal(err)
		}
		s.run("respawn-pane", "-k", "-t", "=hataori-t-7:", "exec cat > "+s.dir+"/typed")
		if l, err := h.Alive(t.Context(), w); err != nil || l.State != holder.Gone {
			t.Errorf("Alive: %+v %v", l, err)
		}
		still(s, "%0")
	})
	// The agent run again right before its pane is removed is not counted as
	// ended.
	t.Run("without a command, during Record", func(t *testing.T) {
		s := newServer(t)
		h := s.starter()
		dir := filepath.Join(s.dir, "7")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		w, err := h.Start(t.Context(), dir, sleeper)
		if err != nil {
			t.Fatal(err)
		}
		r, _ := parseRef(w)
		h.Tmux = s.before(2, s.tmux+" -S "+s.sock+" respawn-pane -k -t "+r.Pane)
		if err := h.Record(t.Context(), w); err == nil {
			t.Error("Record: no error")
		}
		if l, err := h.Alive(t.Context(), w); err != nil || l.State != holder.Running {
			t.Errorf("Alive: %+v %v", l, err)
		}
	})
	t.Run("during Input", func(t *testing.T) {
		s, _, w, pane, _ := newPane(t)
		if err := respawned(s, pane, 2).Input(context.Background(), w, []agent.Part{{Key: agent.Escape}}); !errors.Is(err, errGone) {
			t.Error(err)
		}
		still(s, pane)
	})
	t.Run("while a leftover is cleared", func(t *testing.T) {
		s := newServer(t)
		dir := filepath.Join(s.dir, "7")
		pane := s.run("new-session", "-d", "-s", "hataori-t-7", "-P", "-F", "#{pane_id}", placeholder)
		s.run("set-option", "-w", "-t", pane, "remain-on-exit", "on")
		respawned(s, pane, 2).clearLeftover(context.Background(), dir)
		still(s, pane)
	})
}

// A dead agent's children that are left in its group are ended by Stop, or
// reported where tmux does not tell when the agent ended; also of an agent a
// person ran again.
func TestStopEndsWhatADeadAgentLeft(t *testing.T) {
	for _, again := range []bool{false, true} {
		t.Run(fmt.Sprintf("again %v", again), func(t *testing.T) {
			s := newServer(t)
			h := s.starter()
			dir := filepath.Join(s.dir, "7")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			child := filepath.Join(s.dir, "child")
			w, err := h.Start(t.Context(), dir, holder.Spec{Terminal: true,
				Argv: []string{"/bin/sh", "-c", "trap '' HUP; sleep 600 & echo $! >> " + child + "; sleep 0.3; exit 3"}})
			if err != nil {
				t.Fatal(err)
			}
			r, _ := parseRef(w)
			pids := func(n int) (pids []int) {
				t.Helper()
				for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
					b, _ := os.ReadFile(child)
					dead := s.how(r.Pane) == "1 3"
					if f := strings.Fields(string(b)); len(f) == n && dead {
						for _, x := range f {
							pid, _ := strconv.Atoi(x)
							pids = append(pids, pid)
						}
						return pids
					}
					if time.Now().After(deadline) {
						t.Fatalf("the agent: children %q, dead %v", b, dead)
					}
				}
			}
			endLater := func(pid int) {
				pgid, _ := syscall.Getpgid(pid)
				t.Cleanup(func() {
					if now, _ := syscall.Getpgid(pid); pgid > 1 && now == pgid {
						syscall.Kill(pid, syscall.SIGKILL)
					}
				})
			}
			left := pids(1)
			endLater(left[0])
			if again {
				s.run("respawn-pane", "-t", r.Pane)
				left = pids(2)
				endLater(left[1])
			}
			tells := s.run("display-message", "-p", "-t", r.Pane, "#{pane_dead_time}") != ""
			res, err := h.Stop(t.Context(), w, time.Second, time.Second)
			if err != nil {
				t.Fatalf("Stop: %+v %v", res, err)
			}
			last := left[len(left)-1]
			runs := !zombie(t.Context(), last) && syscall.Kill(last, 0) == nil // in this order: a zombie may be reaped in between
			if tells && (runs || res.Left != "" || res.Already || !strings.HasPrefix(res.Signals, "SIGINT")) {
				t.Errorf("the child runs: %v; Stop: %+v", runs, res)
			}
			if !tells && (!runs || res.Left == "" || res.Signals != "") {
				t.Errorf("the child runs: %v; Stop: %+v", runs, res)
			}
			if p := s.panes()[r.Pane]; p != "" {
				t.Errorf("the pane is still there: %q", p)
			}
		})
	}
}

// An agent that closed its terminal is dead to tmux, or has no process, as it
// runs on: Stop ends it.
func TestStopEndsAnAgentThatClosedItsTerminal(t *testing.T) {
	s := newServer(t)
	h := s.starter()
	dir := filepath.Join(s.dir, "7")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	closed := filepath.Join(s.dir, "closed")
	w, err := h.Start(t.Context(), dir, holder.Spec{Terminal: true,
		Argv: []string{"/bin/sh", "-c", "trap '' HUP; sleep 0.3; exec < /dev/null > /dev/null 2>&1; echo $$ > " + closed + "; while :; do sleep 0.05; done"}})
	if err != nil {
		t.Fatal(err)
	}
	pid := 0
	for deadline := time.Now().Add(10 * time.Second); pid == 0; time.Sleep(10 * time.Millisecond) {
		b, _ := os.ReadFile(closed)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		if time.Now().After(deadline) {
			t.Fatal("the agent did not start")
		}
	}
	t.Cleanup(func() {
		if ps, err := processes(context.Background()); err == nil && slices.ContainsFunc(ps, func(p member) bool { return p.pid == pid && p.group == pid }) && !groupGone(pid) {
			syscall.Kill(-pid, syscall.SIGKILL)
		}
	})
	time.Sleep(300 * time.Millisecond) // tmux sees the terminal's end
	res, err := h.Stop(t.Context(), w, time.Second, time.Second)
	if err != nil || res.Already || res.Left != "" || res.Signals == "" {
		t.Errorf("Stop: %+v %v", res, err)
	}
	if !groupGone(pid) {
		t.Error("the agent runs on")
	}
}

// The number of the process a pane had may be another group's by now: Stop
// does not signal that one.
func TestStopLeavesAnothersGroupAlone(t *testing.T) {
	const older = 4 * time.Second // times are in whole seconds
	for _, c := range []struct {
		name  string
		argv  []string
		attr  *syscall.SysProcAttr
		runs  bool // tmux has not told the pane's end
		after time.Duration
		left  bool
		fails bool
		early bool // the other comes before the end
	}{
		{"in a session of another's", []string{"sleep", "600"}, &syscall.SysProcAttr{Setpgid: true}, false, older, false, false, false},
		{"in a session of its own", []string{"sleep", "600"}, &syscall.SysProcAttr{Setsid: true}, false, older, false, false, false},
		{"at once, in a session of its own", []string{"sleep", "600"}, &syscall.SysProcAttr{Setsid: true}, false, 0, false, false, false},
		{"in a session whose leader ended", []string{"sh", "-c", "sleep 600 & echo $!"}, &syscall.SysProcAttr{Setsid: true}, false, older, true, false, false},
		{"of which the leader ended, in a session of another's", []string{"sh", "-c", "sleep 600 & echo $!"}, &syscall.SysProcAttr{Setpgid: true}, false, 0, false, false, true},
		{"before tmux tells the end", []string{"sleep", "600"}, &syscall.SysProcAttr{Setsid: true}, true, 0, false, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t)
			h := &Holder{Prefix: "hataori-t-"}
			w, pane, dir := s.started(h, "exec sleep 600")
			s.run("set-option", "-p", "-t", pane, "remain-on-exit", "on")
			end := func() {
				if !c.runs {
					s.ended(w, pane)
				}
			}
			if !c.early {
				end()
			}
			time.Sleep(c.after)
			other := exec.Command(c.argv[0], c.argv[1:]...)
			other.SysProcAttr = c.attr
			out := filepath.Join(s.dir, "other")
			f, err := os.Create(out)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			other.Stdout = f
			if err := other.Start(); err != nil {
				t.Fatal(err)
			}
			pid := other.Process.Pid
			ended := make(chan struct{})
			go func() { other.Wait(); close(ended) }()
			t.Cleanup(func() { other.Process.Kill(); <-ended })
			if c.argv[0] == "sh" { // its child is what is left
				<-ended
				b, _ := os.ReadFile(out)
				if pid, err = strconv.Atoi(strings.TrimSpace(string(b))); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if group, _ := syscall.Getpgid(pid); group == other.Process.Pid {
						syscall.Kill(pid, syscall.SIGKILL)
					}
				})
			}
			if c.early {
				end()
			}
			s.renumber(h, &w, dir, other.Process.Pid)
			res, err := h.Stop(t.Context(), w, time.Second, time.Second)
			if (err != nil) != c.fails || res.Already == c.fails || res.Signals != "" || (res.Left != "") != c.left {
				t.Errorf("Stop: %+v %v", res, err)
			}
			// An agent that may run keeps its pane, with nothing recorded.
			if _, err := os.Stat(filepath.Join(dir, StatusFile)); c.fails != errors.Is(err, os.ErrNotExist) {
				t.Errorf("the status file: %v", err)
			}
			if _, got := s.panes()[pane]; got != c.fails {
				t.Errorf("the pane is there: %v", got)
			}
			time.Sleep(200 * time.Millisecond)
			if ps, err := processes(t.Context()); err != nil || !slices.ContainsFunc(ps, func(p member) bool { return p.pid == pid && p.group == other.Process.Pid }) {
				t.Errorf("the other's process was ended (%v)", err)
			}
			select {
			case <-ended:
				if pid == other.Process.Pid {
					t.Error("the other's process was ended")
				}
			default:
			}
		})
	}
}

// renumber makes it as if w's process had had the number pid, which another
// has now.
func (s *server) renumber(h *Holder, w *holder.Window, dir string, pid int) {
	s.t.Helper()
	agents, others := strconv.Itoa(w.Process), strconv.Itoa(pid)
	noted := filepath.Join(dir, PidFile)
	if err := os.WriteFile(noted, []byte(others), 0o600); err != nil {
		s.t.Fatal(err)
	}
	before := time.Now().Add(-10 * time.Second)
	if err := os.Chtimes(noted, before, before); err != nil {
		s.t.Fatal(err)
	}
	w.Process = pid
	s.wrap(h, `for a; do shift; set -- "$@" "$(printf %s "$a" | sed 's/,`+others+`}/,`+agents+`}/g')"; done`, agents, others)
}

// wrap makes h's tmux tell the process to where it is from, after a first
// command of sh.
func (s *server) wrap(h *Holder, first, from, to string) {
	s.t.Helper()
	h.Tmux = filepath.Join(s.dir, "renumbered")
	syscall.ForkLock.RLock()
	err := os.WriteFile(h.Tmux, []byte("#!/bin/sh\n"+first+`
`+s.tmux+` "$@" | sed 's/\([ ,]\)`+from+`\([ ,]\)/\1`+to+`\2/'
`), 0o700)
	syscall.ForkLock.RUnlock()
	if err != nil {
		s.t.Fatal(err)
	}
}

// closes is a program that closes its terminal once the file kept is there,
// and runs on.
const closes = `exec sh -c 'until [ -e kept ]; do sleep 0.05; done; trap "" HUP; exec </dev/null >/dev/null 2>&1; exec sleep 600'`

// child reports whether pid is a child of w's server that leads a group.
func (s *server) child(w holder.Window, pid int) bool {
	r, _ := parseRef(w)
	ps, err := processes(context.Background())
	return err == nil && slices.ContainsFunc(ps, func(p member) bool { return p.pid == pid && p.parent == r.Server && p.group == pid })
}

// closed reports whether tmux has seen the end of the pane's terminal: since
// 3.8, one of which the program runs on is not dead and has no process.
func (s *server) closed(pane string) bool {
	s.t.Helper()
	l := s.run("display-message", "-p", "-t", pane, "#{pane_dead},#{pane_pid}")
	return l == "0," || strings.HasPrefix(l, "1,")
}

// seesClose reports whether tmux sees the end of a terminal whose program
// runs on: not 3.7, and not on macOS (3.8), where it stays a pane that is
// alive and tells its process.
func (s *server) seesClose() bool {
	v, _ := exec.Command(s.tmux, "-V").Output()
	return runtime.GOOS != "darwin" && !strings.HasPrefix(string(v), "tmux 3.7")
}

// wait fails the test with what unless ok is true soon.
func (s *server) wait(what string, ok func() bool) {
	s.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			s.t.Fatal(what)
		}
	}
}

// The number of the process a pane had may be another child's of the same
// server: of another pane, also one tmux takes for dead, or of a popup. Stop
// does not signal it, and fails where the agent may have the number still.
func TestStopLeavesAnotherChildOfTheServerAlone(t *testing.T) {
	for _, c := range []struct {
		name         string
		agent, other string
		popup        bool
	}{
		{"another pane's, the agent ended", "exec sleep 600", "exec sleep 600", false},
		{"another pane's, the agent's end not read", closes, "exec sleep 600", false},
		{"a dead pane's, the agent's end not read", closes, closes, false},
		{"no pane's, the agent ended", "exec sleep 600", "", true},
		{"no pane's, the agent's end not read", closes, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t)
			if c.agent == closes && !s.seesClose() {
				t.Skip("tmux takes a pane whose terminal is closed for alive, and tells its process")
			}
			h := &Holder{Prefix: "hataori-t-"}
			kept := func(program string) string { return strings.Replace(program, "kept", filepath.Join(s.dir, "kept"), 1) }
			w, pane, dir := s.started(h, kept(c.agent))
			s.run("set-option", "-g", "remain-on-exit", "on")
			s.run("new-window", "-d", "exec sleep 601") // the server stays
			if err := os.WriteFile(filepath.Join(s.dir, "kept"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			end := func(pid int) {
				t.Cleanup(func() {
					if s.child(w, pid) {
						syscall.Kill(-pid, syscall.SIGKILL)
					}
				})
			}
			end(w.Process)
			if c.agent != closes {
				s.ended(w, pane) // as tmux has read its end, its number may be another's
			} else {
				s.wait("the agent's pane is not dead", func() bool { return s.closed(pane) })
			}
			pid := 0
			if c.popup {
				self, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				out := filepath.Join(s.dir, "other")
				s.run("run-shell", "-b", "exec "+quote(self)+" leader "+quote(out))
				s.wait("no other child", func() bool {
					b, _ := os.ReadFile(out)
					pid, _ = strconv.Atoi(string(b))
					return pid > 0
				})
			} else {
				f := strings.Fields(s.run("new-window", "-d", "-P", "-F", "#{pane_id} #{pane_pid}", kept(c.other)))
				pid, _ = strconv.Atoi(f[1])
				if c.other == closes {
					s.wait("the other pane is not dead", func() bool { return s.closed(f[0]) })
				}
			}
			end(pid)
			if !s.child(w, pid) {
				t.Fatalf("%d is not a child of the server that leads a group", pid)
			}
			// tmux up to 3.2 does not tell that it read a signal's end; 3.8 does
			// not take a pane for dead whose program's end it has not read.
			old := !tellsEnds.MatchString(s.run("display-message", "-p", "#{version}"))
			if c.agent == closes && c.other != "exec sleep 600" && !old {
				t.Skip("the agent has the number: tmux tells each end it has read")
			}
			fails := old && (c.popup || c.other == closes) || s.run("display-message", "-p", "-t", pane, "#{pane_dead}") == "0"
			s.renumber(h, &w, dir, pid)
			res, err := h.Stop(t.Context(), w, time.Second, time.Second)
			if (err != nil) != fails || res.Already == fails || res.Signals != "" || res.Left != "" {
				t.Errorf("Stop: %+v %v", res, err)
			}
			if _, err := os.Stat(filepath.Join(dir, StatusFile)); fails != errors.Is(err, os.ErrNotExist) {
				t.Errorf("the status file: %v", err)
			}
			if _, got := s.panes()[pane]; got != fails {
				t.Errorf("the pane is there: %v", got)
			}
			time.Sleep(200 * time.Millisecond)
			if !s.child(w, pid) {
				t.Error("the other's process was ended")
			}
		})
	}
}

// Stop ends the agent beside a dead pane whose program had the same number,
// and one tmux takes for alive also without ps, from tmux 3.3 on.
func TestStopEndsAnAgentTmuxTellsOf(t *testing.T) {
	for _, c := range []struct {
		name, agent string
		stale       bool
	}{
		{"without ps", "exec sleep 600", false},
		{"beside a dead pane", "exec sleep 600", true},
		{"beside a dead pane, its terminal closed", closes, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t)
			h := &Holder{Prefix: "hataori-t-", Tmux: s.tmux}
			kept := filepath.Join(s.dir, "kept")
			w, pane, dir := s.started(h, strings.Replace(c.agent, "kept", kept, 1))
			t.Cleanup(func() {
				if !groupGone(w.Process) && s.child(w, w.Process) {
					syscall.Kill(-w.Process, syscall.SIGKILL)
				}
			})
			if err := os.WriteFile(filepath.Join(dir, PidFile), []byte(strconv.Itoa(w.Process)), 0o600); err != nil {
				t.Fatal(err)
			}
			s.run("set-option", "-g", "remain-on-exit", "on")
			if c.stale {
				f := strings.Fields(s.run("new-window", "-d", "-P", "-F", "#{pane_id} #{pane_pid}", "exit 3"))
				s.wait("the other pane is not dead", func() bool { return s.how(f[0]) == "1 3" })
				s.wrap(h, "", f[1], strconv.Itoa(w.Process))
			}
			if err := os.WriteFile(kept, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if c.agent == closes && s.seesClose() {
				s.wait("the agent's terminal is not closed", func() bool { return s.closed(pane) })
			}
			if !c.stale {
				if !tellsEnds.MatchString(s.run("display-message", "-p", "#{version}")) {
					t.Skip("tmux does not tell each end it has read")
				}
				t.Setenv("PATH", "/nowhere")
			}
			res, err := h.Stop(t.Context(), w, time.Second, time.Second)
			if err != nil || res.Already || res.Left != "" || res.Signals == "" {
				t.Errorf("Stop: %+v %v", res, err)
			}
			if !groupGone(w.Process) {
				t.Error("the agent runs on")
			}
		})
	}
}

// A process that ended and that its parent does not reap stays in the agent's
// group, and signals still go to it. Stop does not count it, and counts what
// runs.
func TestStopBesideAnUnreapedProcess(t *testing.T) {
	s := newServer(t)
	h := s.starter()
	dir := filepath.Join(s.dir, "7")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	made := filepath.Join(s.dir, "made")
	w, err := h.Start(t.Context(), dir, holder.Spec{Terminal: true, Argv: []string{"/bin/sh", "-c", h.Hataori + " unreaped " + made + " & exec sleep 600"}})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		ps, err := group(t.Context(), w.Process)
		if _, serr := os.Stat(made); serr == nil && err == nil && slices.ContainsFunc(ps, func(p member) bool { return p.zombie }) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no zombie in the group: %v %v", ps, err)
		}
	}
	if groupGone(w.Process) {
		t.Error("the group is taken for gone while its agent runs")
	}
	res, err := h.Stop(t.Context(), w, time.Second, time.Second)
	if err != nil || res.Signals != "SIGINT" || res.Status.Signal != syscall.SIGINT.String() && s.tellsSignals() {
		t.Errorf("Stop: %+v %v", res, err)
	}
	if got := s.panes(); len(got) != 0 {
		t.Errorf("panes left: %v", got)
	}
}

// unreaped starts a process in its own group as it was, does not reap it, and
// stays, in another group, until the file made is removed.
func unreaped(made string) error {
	signal.Ignore(syscall.SIGHUP, syscall.SIGINT)
	pgid := syscall.Getpgrp()
	if err := syscall.Setpgid(0, 0); err != nil {
		return err
	}
	c := exec.Command("/bin/sh", "-c", ":") // there is no PATH here
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := c.Start(); err != nil {
		return err
	}
	if err := os.WriteFile(made, nil, 0o600); err != nil {
		return err
	}
	for {
		if _, err := os.Stat(made); err != nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A dead pane whose process tmux did not reap does not tell how it ended
// until tmux looks at its children again: Stop, Record and Alive have it
// look.
func TestHowAnUnreapedAgentEnded(t *testing.T) {
	for _, how := range []string{"Stop", "Record", "Alive"} {
		t.Run(how, func(t *testing.T) {
			s := newServer(t)
			h := &Holder{Prefix: "hataori-t-"}
			end := filepath.Join(s.dir, "end")
			w, pane, dir := s.started(h, "exec sh -c 'until [ -e "+end+" ]; do sleep 0.02; done; exit 3'")
			if err := os.WriteFile(end, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			s.ended(holder.Window{}, pane)
			// As if the pane's process were this one, not reaped, and tmux had
			// not looked yet.
			c := exec.Command("true")
			c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := c.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Wait() })
			w.Process = c.Process.Pid
			missed := strconv.Itoa(w.Process) + " 1"
			if s.run("display-message", "-p", "-t", pane, "#{pane_pid}") == "" { // since tmux 3.8
				missed = "- 0"
			}
			if how != "Alive" { // without the file it is the process of the window
				if err := os.WriteFile(filepath.Join(dir, PidFile), []byte(strconv.Itoa(w.Process)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			looked := filepath.Join(s.dir, "looked")
			h.Tmux = filepath.Join(s.dir, "late-tmux")
			syscall.ForkLock.RLock()
			err := os.WriteFile(h.Tmux, []byte(fmt.Sprintf(`#!/bin/sh
case "$*" in *run-shell*) touch %[1]s;; esac
if [ -e %[1]s ]; then exec %[2]s "$@"; fi
%[2]s "$@" | sed 's/ [0-9-]* 1 x[0-9]* [^ ]* / %[3]s x - /'
`, looked, s.tmux, missed)), 0o700)
			syscall.ForkLock.RUnlock()
			if err != nil {
				t.Fatal(err)
			}
			var st *host.Status
			switch how {
			case "Stop":
				res, serr := h.Stop(t.Context(), w, time.Second, time.Second)
				st, err = &res.Status, serr
			case "Record":
				err = h.Record(t.Context(), w)
				got, _ := recorded(ref{Dir: dir})
				st = &got
			default:
				var l holder.Liveness
				l, err = h.Alive(t.Context(), w)
				st = l.Status
			}
			if err != nil || st == nil || st.Code != 3 {
				t.Errorf("%+v %v", st, err)
			}
		})
	}
}

// How long a process has run, as ps prints it ([[days-]hours:]minutes:seconds).
func TestElapsed(t *testing.T) {
	for s, want := range map[string]time.Duration{"00:07": 7 * time.Second, "21:05": 21*time.Minute + 5*time.Second,
		"01:02:03": time.Hour + 2*time.Minute + 3*time.Second, "2-03:00:01": 51*time.Hour + time.Second} {
		if got, ok := elapsed(s); !ok || got != want {
			t.Errorf("%s: %v %v", s, got, ok)
		}
	}
	for _, s := range []string{"", "-", "7", "x:y", "1-2"} {
		if got, ok := elapsed(s); ok {
			t.Errorf("%s: %v", s, got)
		}
	}
}

// tellsSignals reports whether tmux tells the signal that ended a program:
// up to 3.2 it does not.
func (s *server) tellsSignals() bool {
	s.t.Helper()
	out, err := exec.Command(s.tmux, "-V").Output()
	if err != nil {
		s.t.Fatal(err)
	}
	return !regexp.MustCompile(`^tmux 3\.[0-2]`).Match(out)
}

// how is whether the pane's program ended, and its status or signal. tmux may
// miss that: it looks again after the end of another child.
func (s *server) how(pane string) string {
	s.t.Helper()
	pid, said, _ := strings.Cut(s.run("display-message", "-p", "-t", pane, "#{pane_pid},#{pane_dead} #{pane_dead_status}#{pane_dead_signal}"), ",")
	if said = strings.TrimSpace(said); said == "1" || said == "0" && pid == "" { // the latter since 3.8
		s.run("run-shell", "-b", "true")
	}
	return said
}

// ended makes the pane's program end and waits until tmux has seen it.
func (s *server) ended(w holder.Window, pane string) {
	s.t.Helper()
	if w.Process > 0 {
		syscall.Kill(w.Process, syscall.SIGTERM)
	}
	old := !s.tellsSignals()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		dead, how, _ := strings.Cut(s.how(pane), " ")
		if dead == "1" && (how != "" || old) {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("the pane is not dead: %q %q", dead, how)
		}
	}
}

// Since tmux 3.8 a dead pane has no process. Its removal is refused once a
// person ran a command of theirs there after the holder's look, also one that
// ended as well.
func TestADeadPaneAPersonRespawnedStays(t *testing.T) {
	for _, how := range []string{"Record", "Stop", "a leftover"} {
		t.Run(how, func(t *testing.T) {
			for n := 2; ; n++ {
				s := newServer(t)
				h := &Holder{Prefix: "hataori-t-", Server: []string{"-S", s.sock, "-f", "/dev/null"}}
				w, pane, dir := s.started(h, "exec sleep 600")
				s.ended(w, pane)
				if s.run("display-message", "-p", "-t", pane, "#{pane_pid}") != "" {
					t.Skip("a dead pane keeps its process in this tmux")
				}
				on := s.tmux + " -S " + s.sock
				done := filepath.Join(s.dir, "theirs")
				h.Tmux = s.before(n, on+" respawn-pane -t "+pane+" 'echo > "+done+"'; until [ -e "+done+" ] && [ \"$("+on+" display-message -p -t "+pane+" '#{pane_dead}')\" = 1 ]; do sleep 0.02; done")
				var err error
				switch how {
				case "Record":
					err = h.Record(t.Context(), w)
				case "Stop":
					_, err = h.Stop(t.Context(), w, time.Second, time.Second)
				default:
					h.clearLeftover(t.Context(), dir)
				}
				if _, serr := os.Stat(done); serr != nil {
					if n == 2 {
						t.Error("no command of the holder's came after the respawn")
					}
					break
				}
				if err != nil {
					t.Errorf("before command %d: %v", n, err)
				}
				if s.panes()[pane] == "" {
					t.Errorf("before command %d: the person's pane went", n)
				}
			}
		})
	}
}

// A pane whose program ended after the holder's look is removed.
func TestAPaneThatEndedSinceIsRemoved(t *testing.T) {
	s := newServer(t)
	h := &Holder{Prefix: "hataori-t-"}
	w, pane, _ := s.started(h, "exec sleep 600")
	r, _ := parseRef(w)
	p, err := h.findStarted(t.Context(), r, w.Process)
	if err != nil || p.pid != w.Process {
		t.Fatalf("%+v %v", p, err)
	}
	s.ended(w, pane)
	if err := h.removePane(t.Context(), r, p); err != nil {
		t.Error(err)
	}
	if got := s.panes(); len(got) != 0 {
		t.Errorf("panes left: %v", got)
	}
}

// The dead pane of a window a person started is there, without the process
// tmux up to 3.7 tells of it too.
func TestADeadPaneOfAPersonsWindowIsThere(t *testing.T) {
	s := newServer(t)
	h := &Holder{Prefix: "hataori-t-"}
	pane, pid := s.pane("exec sleep 600")
	s.run("set-option", "-w", "-t", pane, "remain-on-exit", "on")
	w := s.window(pane, pid)
	s.ended(holder.Window{Process: pid}, pane)
	if tty, err := h.Device(t.Context(), w); err != nil || tty != "" {
		t.Errorf("Device: %q %v", tty, err)
	}
}

// The agent's own dead pane is removed, whatever its state directory's path.
func TestADeadPaneOfAnyStateDirectoryGoes(t *testing.T) {
	for _, locale := range []string{"C", "C.UTF-8", "en_US.UTF-8"} {
		for _, name := range []string{"a b", "it's", `a"b`, "a#b", "#{x}", "#[x]#(x)##", "a,b", "a}b{", "日本é", "a_b", `a\b\n\101`, "[a]*?", "$HOME;~`", ":"} {
			t.Run(locale+" "+name, func(t *testing.T) {
				s := newServer(t)
				t.Setenv("LC_ALL", locale)
				in := *s
				in.dir = filepath.Join(s.dir, name)
				h := &Holder{Prefix: "hataori-t-"}
				w, pane, _ := in.started(h, "exec sleep 600")
				s.ended(w, pane)
				if err := h.Record(t.Context(), w); err != nil {
					t.Error(err)
				}
				if got := s.panes(); len(got) != 0 {
					t.Errorf("panes left: %v", got)
				}
			})
		}
	}
}

// A zombie as ps prints it, also one whose other threads run.
func TestIsZombie(t *testing.T) {
	for stat, want := range map[string]bool{"Z": true, "Z+": true, "ZN": true, "Zs": true, "Zl": false, "Zsl+": false, "S": false, "Ss+": false, "": false} {
		if got := isZombie(stat); got != want {
			t.Errorf("%q: %v", stat, got)
		}
	}
}

// A listing of ps may miss a process that one it lists as ended started
// meanwhile: the group is gone when two in a row have nothing that runs.
func TestAGroupIsGoneWhenTwoLooksSaySo(t *testing.T) {
	dir := t.TempDir()
	again := filepath.Join(dir, "again")
	syscall.ForkLock.RLock()
	err := os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\necho '71 1 71 00:01 Z'\nif [ -e "+again+" ]; then echo '70 1 71 00:00 S'; fi\n: > "+again+"\n"), 0o700)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if ps, err := group(t.Context(), 71); err != nil || len(ps) != 1 || !ps[0].zombie {
		t.Fatalf("the first look: %+v %v", ps, err)
	}
	if err := os.Remove(again); err != nil {
		t.Fatal(err)
	}
	if onlyZombies(71) {
		t.Error("the process that the first look missed runs")
	}
}

// The test binary is also `hataori exec` and `hataori pipe`, agentBelow's shell,
// unreaped and a process that leads its own session.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 {
		var err error
		switch os.Args[1] {
		case "exec":
			err = ExecMain(os.Args[2])
		case "unreaped":
			err = unreaped(os.Args[2])
		case "pipe":
			err = host.Pipe(os.Args[2], os.Stdin)
		case "shell":
			err = shellMain(os.Args[2])
		case "leader":
			err = leaderMain(os.Args[2])
		default:
			os.Exit(m.Run())
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// leaderMain leads a session, tells its number in the file out and sleeps.
func leaderMain(out string) error {
	if _, err := syscall.Setsid(); err != nil {
		return err
	}
	if err := writePrivate(out, []byte(strconv.Itoa(os.Getpid()))); err != nil {
		return err
	}
	time.Sleep(10 * time.Minute)
	return nil
}

// sleeper is an agent that only runs.
var sleeper = holder.Spec{Terminal: true, Argv: []string{"/bin/sleep", "600"}}

// starter is a Holder that starts windows on s.
func (s *server) starter() *Holder {
	s.t.Helper()
	p, err := os.Executable()
	if err != nil {
		s.t.Fatal(err)
	}
	return &Holder{Hataori: p, Prefix: "hataori-t-", Server: []string{"-S", s.sock, "-f", "/dev/null"}}
}

// tmux prints the window it made cut short, with no error, when its server
// does not get to run meanwhile. The captures are of a tmux 3.8 with a pause
// of 150 ms in the variable of their name; the processes' ids are replaced.
// Such a window is removed and made again, and an agent is never started in
// one; when every one is cut, the start fails.
func TestAWindowWhoseAnswerIsCutIsMadeAgain(t *testing.T) {
	for _, at := range []string{"pane-pid", "version"} {
		for _, cuts := range []struct{ made, listed int }{{1, 2}, {1, 9}, {9, 2}} {
			t.Run(fmt.Sprintf("%s %+v", at, cuts), func(t *testing.T) {
				cut, listing := capture(t, "new-session-cut-"+at+"-3.8.txt"), capture(t, "list-panes-cut-3.8.txt")
				s := newServer(t)
				dir := filepath.Join(s.dir, "7")
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				s.run("new-session", "-d", "-s", "theirs", placeholder)
				before := s.panes()
				h := s.starter()
				made, listed := filepath.Join(s.dir, "made"), filepath.Join(s.dir, "listed")
				h.Tmux = s.script(`case "$*" in *" new-session "*)
	echo x >> ` + made + `
	if [ $(wc -l < ` + made + `) -le ` + strconv.Itoa(cuts.made) + ` ]; then
		` + s.tmux + ` "$@" > /dev/null && exec cat ` + quote(cut) + `
	fi;;
*" list-panes -s "*) # after a window was made
	if [ -e ` + made + ` ] && echo x >> ` + listed + ` && [ $(wc -l < ` + listed + `) -le ` + strconv.Itoa(cuts.listed) + ` ]; then exec cat ` + quote(listing) + `; fi;;
esac
exec ` + s.tmux + ` "$@"`)
				w, err := h.Start(t.Context(), dir, sleeper)
				after := s.panes()
				switch {
				case cuts.listed == 9:
					// The pane cannot be found: no second one beside it, and
					// the next start removes it.
					if err == nil || !strings.Contains(err.Error(), "list-panes printed") || len(after) != len(before)+1 {
						t.Errorf("a start: %v; the panes after it: %v, before it %v", err, after, before)
					}
					h.Tmux = ""
					if w, err = h.Start(t.Context(), dir, sleeper); err != nil {
						t.Fatal(err)
					}
					after = s.panes()
				case cuts.made == 9:
					if b, _ := os.ReadFile(cut); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("printed %q", b)) {
						t.Errorf("a start: %v", err)
					}
					if !maps.Equal(after, before) {
						t.Errorf("the panes after it: %v, before it %v", after, before)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(after) != len(before)+1 {
					t.Errorf("the panes after the start: %v, before it %v", after, before)
				}
				if l, err := h.Alive(t.Context(), w); err != nil || l.State != holder.Running {
					t.Errorf("Alive: %+v %v", l, err)
				}
			})
		}
	}
}

// capture is the path of a capture.
func capture(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// With windows of a person's left in its session, the next start makes the
// agent's window there, and what a client shows there stays as it is.
func TestStartInASessionWithAPersonsWindow(t *testing.T) {
	s := newServer(t)
	h := s.starter()
	dir := filepath.Join(s.dir, "7")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	w, err := h.Start(t.Context(), dir, sleeper)
	if err != nil {
		t.Fatal(err)
	}
	theirs := s.run("new-window", "-t", "=hataori-t-7:", "-P", "-F", "#{pane_id}", "exec sleep 601")
	if err := h.Record(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	if w, err = h.Start(t.Context(), dir, sleeper); err != nil {
		t.Fatalf("the second start: %v", err)
	}
	r, _ := parseRef(w)
	got := s.panes()
	if len(got) != 2 || !strings.HasSuffix(got[r.Pane], " hataori-t-7") || !strings.HasSuffix(got[theirs], " hataori-t-7") || r.Pane == theirs {
		t.Errorf("panes: %v, the agent's %s, the person's %s", got, r.Pane, theirs)
	}
	if shown := s.run("display-message", "-p", "-t", "=hataori-t-7:", "#{pane_id}"); shown != theirs {
		t.Errorf("the session now shows %s", shown)
	}
	if l, _ := h.Alive(t.Context(), w); l.State != holder.Running {
		t.Errorf("Alive: %+v", l)
	}
	split := s.run("split-window", "-d", "-t", r.Pane, "-P", "-F", "#{pane_id}", "exec sleep 1")
	// Deliberately observe after the old one-second lifetime has elapsed.
	time.Sleep(1200 * time.Millisecond)
	if err := h.Record(t.Context(), w); err != nil {
		t.Error(err)
	}
	if s.panes()[split] == "" {
		t.Error("the person's split went")
	}
	// Its window does not outlive their program.
	for deadline := time.Now().Add(10 * time.Second); s.panes()[split] != "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
	}
	if got := s.panes(); len(got) != 1 || got[theirs] == "" {
		t.Errorf("panes after the second stop: %v", got)
	}
}

// An earlier run that could not be ended keeps the next from starting beside
// it, in the same state directory.
func TestNoStartBesideALeftover(t *testing.T) {
	// The first look at what is left fails, or each does.
	for _, fails := range []string{"[ -e %[1]s ] ||", ""} {
		s := newServer(t)
		_, pane, dir := s.started(&Holder{Prefix: "hataori-t-"}, "exec sleep 600")
		h := s.starter()
		p := filepath.Join(s.dir, "tmux-fails")
		script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *list-panes*) "+fails+" { >%[1]s; echo 'made to fail' >&2; exit 1; };; esac\nexec %[2]s \"$@\"\n", filepath.Join(s.dir, "failed"), s.tmux)
		if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		h.Tmux = p
		if _, err := h.Start(t.Context(), dir, sleeper); err == nil || !strings.Contains(err.Error(), "earlier run") {
			t.Errorf("Start: %v", err)
		}
		if got := s.panes(); len(got) != 1 || got[pane] == "" {
			t.Errorf("panes: %v", got)
		}
	}
}

// A start that fails removes the pane it made, and no other: not a session
// whose name only starts with its own, not a window a person opened.
func TestAFailedStartRemovesOnlyItsPane(t *testing.T) {
	for _, c := range []struct {
		at   string // the command that fails
		do   string // right before it, with the server's name after tmux
		left int
	}{
		{"pipe-pane", "kill-session -t =hataori-t-7", 1},
		{"pipe-pane", "new-window -d -t =hataori-t-7: 'exec sleep 601'", 2},
		{"pipe-pane", "respawn-pane -k -t =hataori-t-7: 'exec sleep 601'", 2}, // the person's now
		{"respawn-pane -k", "has-session; TMUX \"$@\"", 1},                    // done, but reported as failed
		{"respawn-pane -k", "respawn-pane -k -t =hataori-t-7: 'exec sleep 601'", 2},
	} {
		s := newServer(t)
		s.run("new-session", "-d", "-s", "hataori-t-72", "exec sleep 600")
		h := s.starter()
		dir := filepath.Join(s.dir, "7")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(s.dir, "tmux-fails")
		script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *'%s'*) %s -S %s %s; echo 'made to fail' >&2; exit 1;; esac\nexec %s \"$@\"\n", c.at, s.tmux, s.sock, strings.ReplaceAll(c.do, "TMUX", s.tmux), s.tmux)
		if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		h.Tmux = p
		if _, err := h.Start(t.Context(), dir, sleeper); err == nil {
			t.Fatal("the start did not fail")
		}
		out := s.run("list-panes", "-a", "-F", "#{session_name} #{pane_start_command}")
		if n := len(strings.Split(out, "\n")); n != c.left || !strings.Contains(out, "hataori-t-72") || strings.Contains(out, "86400") || strings.Contains(out, " exec '") {
			t.Errorf("%s at %s: panes left:\n%s", c.do, c.at, out)
		}
	}
}
