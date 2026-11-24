package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	f := strings.Fields(s.run("new-session", "-d", "-s", h.Prefix+"7", "-P", "-F", "#{pane_id} #{pid} #{pane_pid}", program+" # exec "+quote(dir)))
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
	out, _ := exec.Command(s.tmux, "-S", s.sock, "list-panes", "-a", "-F", "#{pane_id} #{pane_pid} #{session_name}").Output()
	m := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.SplitN(l, " ", 2); len(f) == 2 {
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

// The process of a window hataori did not start is the agent's, which may run
// below the pane's own, a shell.
func TestInputToAnAgentBelowThePanesProcess(t *testing.T) {
	s := newServer(t)
	pane := s.run("new-session", "-d", "-P", "-F", "#{pane_id}", "cat; exec cat")
	if err := (&Holder{}).Input(t.Context(), s.window(pane, os.Getpid()), []agent.Part{{Paste: "x"}, {Key: agent.Enter}}); err != nil {
		t.Error(err)
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
		for deadline := time.Now().Add(10 * time.Second); s.run("display-message", "-p", "-t", pane, "#{pane_dead}") != "1" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
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
		dir := filepath.Join(s.dir, "日本 it's $7")
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
		for deadline := time.Now().Add(10 * time.Second); s.run("display-message", "-p", "-t", pane, "#{pane_dead}") != "1" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
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
		h.Tmux = s.before(8, s.tmux+" -S "+s.sock+" respawn-pane -k -t =hataori-t-7: 'exec cat > "+s.dir+"/typed'")
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
		h.Tmux = s.before(8, "sleep 1")
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

// The test binary is also `hataori exec` and `hataori pipe`.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 {
		var err error
		switch os.Args[1] {
		case "exec":
			err = ExecMain(os.Args[2])
		case "pipe":
			err = host.Pipe(os.Args[2], os.Stdin)
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
