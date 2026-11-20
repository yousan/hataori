package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yousan/hataori/internal/holder"
	"github.com/yousan/hataori/internal/host"
)

// Files a window hataori starts keeps in the session's state directory.
const (
	// ExecFile is what `hataori exec` executes: the agent's arguments,
	// environment and directory. It is on no command line.
	ExecFile = "tmux-exec.json"
	// StatusFile is how the agent ended, recorded from #{pane_dead_status}
	// before the pane goes.
	StatusFile = "tmux-status.json"
)

// execSpec is what ExecFile holds.
type execSpec struct {
	Argv []string `json:"argv"`
	Env  []string `json:"env"`
	Dir  string   `json:"dir"`
}

// placeholder is the pane's command until respawn-pane replaces it: it
// prints nothing and waits.
const placeholder = "exec sleep 86400"

// Start starts the agent in a tmux window of hataori's own (design, section
// 4.3, "tmux", steps 2 to 4), once the relay for stateDir listens: a session
// named Prefix and the state directory's name, on the server Server names
// (the default server when empty), made with a placeholder command and the
// spec's size; remain-on-exit on its pane, so that its exit status can be
// read after the agent ends; pipe-pane -O to `hataori pipe`, which writes
// the bounded raw log from the agent's first byte; and respawn-pane -k with
// `hataori exec`, which executes the spec with exactly its environment. The
// environment is in ExecFile, on no command line.
func (h *Holder) Start(ctx context.Context, stateDir string, spec holder.Spec) (holder.Window, error) {
	if h.Hataori == "" {
		return holder.Window{}, holder.ErrCannotStart
	}
	if !spec.Terminal || len(spec.Argv) == 0 {
		return holder.Window{}, errors.New("tmux: only an agent in a terminal is started in a window")
	}
	b, err := json.Marshal(execSpec{Argv: spec.Argv, Env: spec.Env, Dir: spec.Dir})
	if err != nil {
		return holder.Window{}, err
	}
	if err := writePrivate(filepath.Join(stateDir, ExecFile), b); err != nil {
		return holder.Window{}, err
	}
	// A session of this name is one an earlier run left, as when a daemon
	// died between making it and recording it: its agent is ended, and it
	// goes, before anything of this run is written.
	h.clearLeftover(ctx, stateDir)
	os.Remove(filepath.Join(stateDir, StatusFile)) // an earlier run's
	// The log of this run starts empty, at position 0, before the pipe
	// writes to it: a reader never takes an earlier run's for this one's.
	if err := host.ResetPipe(stateDir); err != nil {
		return holder.Window{}, err
	}
	cols, rows := spec.Cols, spec.Rows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	out, err := h.serverCmd(ctx, "new-session", "-d", "-s", h.Prefix+filepath.Base(stateDir),
		"-x", strconv.Itoa(int(cols)), "-y", strconv.Itoa(int(rows)), "-P", "-F", "#{pane_id} #{pid} #{socket_path}", placeholder)
	if err != nil {
		return holder.Window{}, err
	}
	f := strings.Fields(string(out))
	server := 0
	if len(f) == 3 {
		server, _ = strconv.Atoi(f[1])
	}
	if len(f) != 3 || !paneID.MatchString(f[0]) || server <= 0 {
		h.clearLeftover(ctx, stateDir)
		return holder.Window{}, fmt.Errorf("tmux: new-session printed %q", out)
	}
	r := ref{Socket: f[2], Server: server, Pane: f[0], Dir: stateDir}
	// remain-on-exit is a pane option since tmux 3.3, and a window option
	// before; it is set as both, and again after the respawn, which can
	// set a pane's options back.
	steps := [][]string{
		{"set-option", "-w", "-t", r.Pane, "remain-on-exit", "on"},
		{"set-option", "-p", "-t", r.Pane, "remain-on-exit", "on"},
		{"pipe-pane", "-O", "-t", r.Pane, "exec " + quote(h.Hataori) + " pipe " + quote(stateDir)},
		{"respawn-pane", "-k", "-t", r.Pane, "exec " + quote(h.Hataori) + " exec " + quote(stateDir)},
		{"set-option", "-p", "-t", r.Pane, "remain-on-exit", "on"},
	}
	for _, s := range steps {
		if _, err := h.tmux(ctx, r, nil, s...); err != nil {
			h.tmux(context.WithoutCancel(ctx), r, nil, "kill-session", "-t", h.Prefix+filepath.Base(stateDir))
			return holder.Window{}, err
		}
	}
	p, err := h.find(ctx, r)
	if err != nil {
		h.startedStop(context.WithoutCancel(ctx), r, time.Second, time.Second)
		return holder.Window{}, err
	}
	rb, _ := json.Marshal(r)
	return holder.Window{Holder: Name, Ref: string(rb), Owned: true, Process: p.pid}, nil
}

// serverCmd runs a tmux command on the server Server names, or the default
// one.
func (h *Holder) serverCmd(ctx context.Context, args ...string) ([]byte, error) {
	prog := h.Tmux
	if prog == "" {
		prog = "tmux"
	}
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	return runTmux(ctx, prog, h.Server, args, nil)
}

// quote quotes s for the shell tmux runs a command in.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func writePrivate(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("tmux: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("tmux: %w", err)
	}
	return nil
}

// statusOf is how the agent of a dead pane ended.
func statusOf(p pane) host.Status {
	st := host.Status{Exited: true, Code: -1}
	// "x" and the status, so that a status of 0, which a tmux condition
	// reads as false, is printed; only "x" when a signal ended it.
	if n, err := strconv.Atoi(strings.TrimPrefix(p.status, "x")); err == nil {
		st.Code = n
	}
	if p.signal != "-" && p.signal != "" {
		st.Signal = signalName(p.signal)
	}
	return st
}

// recorded is how the agent of the window ended, as Stop or Record wrote it.
func recorded(r ref) (host.Status, bool) {
	if r.Dir == "" {
		return host.Status{}, false
	}
	b, err := os.ReadFile(filepath.Join(r.Dir, StatusFile))
	if err != nil {
		return host.Status{}, false
	}
	var st host.Status
	if json.Unmarshal(b, &st) != nil {
		return host.Status{}, false
	}
	return st, true
}

func record(r ref, st host.Status) error {
	if r.Dir == "" {
		return nil
	}
	b, _ := json.Marshal(st)
	return writePrivate(filepath.Join(r.Dir, StatusFile), b)
}

// startedAlive is Alive for a window hataori started: Running while the
// pane's process runs; Exited, with how it ended, once the pane is dead or
// its end was recorded; Gone when the pane or its server went with nothing
// recorded, as when the server was killed; Unknown when tmux cannot tell.
func (h *Holder) startedAlive(ctx context.Context, r ref) holder.Liveness {
	p, err := h.find(ctx, r)
	switch {
	case errors.Is(err, errGone):
		if st, ok := recorded(r); ok {
			return holder.Liveness{State: holder.Exited, Status: &st}
		}
		return holder.Liveness{State: holder.Gone}
	case err != nil:
		return holder.Liveness{State: holder.Unknown}
	case p.dead:
		st := statusOf(p)
		return holder.Liveness{State: holder.Exited, Status: &st}
	}
	return holder.Liveness{State: holder.Running}
}

// Record writes how the agent of a window hataori started ended, from the
// dead pane, and removes the window. Nothing is signalled.
func (h *Holder) Record(ctx context.Context, w holder.Window) error {
	r, err := parseRef(w)
	if err != nil || !w.Owned {
		return holder.ErrNotOwned
	}
	p, err := h.find(ctx, r)
	if errors.Is(err, errGone) {
		return nil // nothing of it to read or remove: on another server, the same id is another pane
	}
	if err != nil {
		return err
	}
	if p.dead {
		if err := record(r, statusOf(p)); err != nil {
			return err
		}
	}
	return h.removeWindow(ctx, r, p)
}

// removeWindow removes the window's pane p, which find just found on the
// window's own server: with its whole tmux session when that is still
// hataori's, by its name, else only the pane, as when a person moved it
// into a session of theirs.
func (h *Holder) removeWindow(ctx context.Context, r ref, p pane) error {
	ctx = context.WithoutCancel(ctx)
	var err error
	if name := h.Prefix + filepath.Base(r.Dir); h.Prefix != "" && r.Dir != "" && p.session == name {
		_, err = h.tmux(ctx, r, nil, "kill-session", "-t", "="+name)
	} else {
		_, err = h.tmux(ctx, r, nil, "kill-pane", "-t", r.Pane)
	}
	if err != nil && (noServer(err) || strings.Contains(err.Error(), "can't find")) {
		return nil
	}
	return err
}

// killWait bounds the wait for the group to vanish after SIGKILL.
const killWait = 10 * time.Second

// startedStop ends the agent of a window hataori started, as hataori's own
// host does: SIGINT to the pane's process group, then SIGTERM after
// interrupt, then SIGKILL after terminate, stopping as soon as no process of
// the group is left. It records how the agent ended, from
// #{pane_dead_status}, and removes the pane.
func (h *Holder) startedStop(ctx context.Context, r ref, interrupt, terminate time.Duration) (holder.StopResult, error) {
	var res holder.StopResult
	p, err := h.find(ctx, r)
	if errors.Is(err, errGone) {
		res.Already = true
		if st, ok := recorded(r); ok {
			res.Status = st
		}
		return res, nil
	}
	if err != nil {
		return res, err
	}
	pgid := p.pid
	var sent []string
	// The group, whenever a process of it is left: also after the pane's
	// own process ended, leaving children.
	if pgid > 1 && !groupGone(pgid) {
		for _, s := range []struct {
			sig   syscall.Signal
			name  string
			grace time.Duration
		}{{syscall.SIGINT, "SIGINT", interrupt}, {syscall.SIGTERM, "SIGTERM", terminate}, {syscall.SIGKILL, "SIGKILL", killWait}} {
			if err := syscall.Kill(-pgid, s.sig); errors.Is(err, syscall.ESRCH) {
				break
			}
			sent = append(sent, s.name)
			if waitGone(pgid, s.grace) {
				break
			}
		}
		if !groupGone(pgid) {
			return res, fmt.Errorf("tmux: processes of the group %d survived SIGKILL", pgid)
		}
	} else {
		res.Already = true
	}
	res.Signals = strings.Join(sent, ",")
	// remain-on-exit keeps the dead pane, with its status, until it is
	// removed; tmux may take a moment to see the end.
	st := host.Status{Exited: true, Code: -1}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		q, err := h.find(ctx, r)
		if err == nil && q.dead {
			st = statusOf(q)
			break
		}
		if err != nil || time.Now().After(deadline) {
			break
		}
	}
	if len(sent) > 0 {
		st.Stopped, st.StopNote = true, res.Signals
	}
	res.Status = st
	if err := record(r, st); err != nil {
		return res, err
	}
	q, err := h.find(ctx, r)
	if errors.Is(err, errGone) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	return res, h.removeWindow(ctx, r, q)
}

func groupGone(pgid int) bool { return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) }

func waitGone(pgid int, d time.Duration) bool {
	for deadline := time.Now().Add(d); ; time.Sleep(5 * time.Millisecond) {
		if groupGone(pgid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
	}
}

// Device is the terminal of the window's pane, for a window that is there.
func (h *Holder) Device(ctx context.Context, w holder.Window) (string, error) {
	r, err := parseRef(w)
	if err != nil {
		return "", err
	}
	p, err := h.find(ctx, r)
	if err != nil {
		return "", err
	}
	if p.dead {
		return "", nil
	}
	return p.tty, nil
}

// ExecMain is `hataori exec <state-dir>`, the command respawn-pane runs in
// a window hataori started: it executes the agent of ExecFile in its
// directory with exactly its environment, as hataori's own host starts it.
// It returns only when that cannot be done.
func ExecMain(stateDir string) error {
	b, err := os.ReadFile(filepath.Join(stateDir, ExecFile))
	if err != nil {
		return fmt.Errorf("tmux: %w", err)
	}
	var s execSpec
	if err := json.Unmarshal(b, &s); err != nil || len(s.Argv) == 0 {
		return fmt.Errorf("tmux: %s: not an agent to execute", ExecFile)
	}
	argv0, err := host.LookPath(s.Argv[0], s.Env, s.Dir)
	if err != nil {
		return err
	}
	if s.Dir != "" {
		if err := os.Chdir(s.Dir); err != nil {
			return fmt.Errorf("tmux: %w", err)
		}
	}
	env := s.Env
	if env == nil {
		env = []string{} // never the pane's own
	}
	return syscall.Exec(argv0, s.Argv, env)
}

// signalName is the name hataori's own host records for the signal tmux
// names, as a number or as a name such as "term".
func signalName(s string) string {
	if n, err := strconv.Atoi(s); err == nil {
		return syscall.Signal(n).String()
	}
	if n := unix.SignalNum("SIG" + strings.ToUpper(strings.TrimPrefix(strings.ToUpper(s), "SIG"))); n != 0 {
		return n.String()
	}
	return s
}

// clearLeftover ends the agent of the session of stateDir's name that an
// earlier run left, and removes it: only its panes hataori started for
// that state directory, and the session only when nothing else is in it.
func (h *Holder) clearLeftover(ctx context.Context, stateDir string) {
	name := h.Prefix + filepath.Base(stateDir)
	out, err := h.serverCmd(ctx, "list-panes", "-s", "-t", "="+name, "-F", "#{pane_id} #{pane_pid} #{pane_dead} #{pane_start_command}")
	if err != nil {
		return // none
	}
	// The pane Start made runs the placeholder until the respawn, and
	// `hataori exec <stateDir>` after it.
	ours := " exec " + quote(stateDir)
	mine, theirs := []string{}, 0
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.SplitN(l, " ", 4)
		if len(f) != 4 || !(strings.Contains(f[3], ours) || strings.Trim(f[3], `"`) == placeholder) {
			theirs++
			continue
		}
		mine = append(mine, f[0])
		if pid, err := strconv.Atoi(f[1]); err == nil && pid > 1 && f[2] != "1" {
			syscall.Kill(-pid, syscall.SIGKILL)
			waitGone(pid, killWait)
		}
	}
	ctx = context.WithoutCancel(ctx)
	if theirs == 0 {
		h.serverCmd(ctx, "kill-session", "-t", "="+name)
		return
	}
	for _, id := range mine {
		h.serverCmd(ctx, "kill-pane", "-t", id)
	}
}

// Attached reports whether a tmux client shows the window's session now, as
// when a person looks at it or types in it.
func (h *Holder) Attached(ctx context.Context, w holder.Window) (bool, error) {
	r, err := parseRef(w)
	if err != nil {
		return false, err
	}
	out, err := h.tmux(ctx, r, nil, "display-message", "-p", "-t", r.Pane, "#{session_attached}")
	if err != nil {
		return false, err
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n > 0, nil
}
