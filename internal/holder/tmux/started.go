package tmux

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	// PidFile is the process id of the agent, as `hataori exec` had it: a
	// dead pane tells none since tmux 3.8.
	PidFile = "tmux-pid"
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

// startFormat is what Start asks tmux to print of the window it made.
const startFormat = "#{pane_id} #{pid} #{pane_pid} #{version} #{socket_path}"

// Start starts the agent in a tmux window of hataori's own (design, section
// 4.3, "tmux", steps 2 to 4), once the relay for stateDir listens: a session
// named Prefix and the state directory's name, on the server Server names
// (the default server when empty), made with a placeholder command and the
// spec's size; remain-on-exit on its pane, so that its exit status can be
// read after the agent ends; pipe-pane -O to `hataori pipe`, which writes
// the bounded raw log from the agent's first byte; and respawn-pane -k with
// `hataori exec`, which executes the spec with exactly its environment. The
// environment is in ExecFile, on no command line.
//
// A session of that name may be left, with windows a person opened in it
// beside an earlier run's: the agent's window is then made in it, behind
// what the person looks at.
//
// On a server that is too old (ErrTooOld) the window made is removed again,
// and the agent is not started.
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
	os.Remove(filepath.Join(stateDir, PidFile))
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
	r, made, version, err := h.makeWindow(ctx, stateDir, "-x", strconv.Itoa(int(cols)), "-y", strconv.Itoa(int(rows)))
	if err != nil {
		return holder.Window{}, err
	}
	// Only here: a server of another version than the tmux program may run,
	// and one with no session does not tell its version.
	if err := checkVersion(version); err != nil {
		return holder.Window{}, errors.Join(err, h.removePane(ctx, r, pane{pid: made}))
	}
	// In one go, and only while the pane is the one made: on a server started
	// again meanwhile its id is another's, and a person may have given it a
	// program of theirs. A person's hook on one of the commands may make tmux
	// wait there, so the pane is looked at again right before its program is
	// replaced.
	onServer, withProcess := fmt.Sprintf("#{==:#{pid},%d}", r.Server), fmt.Sprintf("#{==:#{pane_pid},%d}", made)
	keep, run := h.setUp(r)
	// If the first condition is cut short, nothing runs, including the
	// respawn. A second check still protects against changes in setup hooks.
	ack, err := newBuffer()
	if err != nil {
		return holder.Window{}, err
	}
	checkedRun := ifPane(r.Pane, run+" ; display-message -p "+ack, onServer, withProcess)
	for i := range checkedRun {
		checkedRun[i] = quote(checkedRun[i])
	}
	out, err := h.tmux(ctx, r, nil, ifPane(r.Pane, keep+" ; "+strings.Join(checkedRun, " "), onServer, withProcess)...)
	if err != nil {
		// The respawn may have given it another process. Gone or a person's,
		// it is left as it is; the next start clears what else is left.
		if p, ferr := h.findStarted(context.WithoutCancel(ctx), r, made); ferr == nil {
			h.removePane(ctx, r, p)
		}
		return holder.Window{}, err
	}
	p, err := h.findStarted(ctx, r, made)
	if err == nil && !slices.Contains(strings.Fields(string(out)), ack) {
		err = errors.New("tmux: the pane setup did not acknowledge starting the agent")
	}
	// A person's hooks on these commands may print lines of their own, also
	// this one: only a pane that is not the agent's was left as it was.
	if err != nil && slices.Contains(strings.Split(string(out), "\n"), notDone) {
		return holder.Window{}, fmt.Errorf("tmux: setup of the pane %s was refused or its condition was incomplete; it was left as it is: %w", r.Pane, err)
	}
	if err != nil {
		res, _ := h.startedStop(context.WithoutCancel(ctx), r, made, time.Second, time.Second)
		if res.Left != "" {
			err = fmt.Errorf("%w; %s", err, res.Left)
		}
		return holder.Window{}, err
	}
	rb, _ := json.Marshal(r)
	// One that ended already has no process on tmux 3.8: the pane is then
	// told by what it runs (findStarted), not by its process.
	return holder.Window{Holder: Name, Ref: string(rb), Owned: true, Process: cmp.Or(p.pid, made)}, nil
}

// makeTries is how often a window is made, or a listing read, whose answer
// tmux cut short.
const makeTries = 3

// makeWindow makes the window Start runs the agent in, with the placeholder
// in it: its pane, the process tmux made there, and the server's version.
// size is new-session's options for it.
//
// Since tmux 3.7 a format that takes longer than 100 ms, as when the server
// does not get to run, is printed up to where it was then, without an error.
// The window is removed then and made again: its process is only known as
// the one made from what tmux printed as it made it, and a socket that is
// empty names no server.
func (h *Holder) makeWindow(ctx context.Context, stateDir string, size ...string) (ref, int, string, error) {
	name := h.Prefix + filepath.Base(stateDir)
	print := []string{"-P", "-F", startFormat, placeholder}
	var cut error
	for range makeTries {
		out, err := h.serverCmd(ctx, slices.Concat([]string{"new-session", "-d", "-s", name}, size, print)...)
		if err != nil && strings.Contains(err.Error(), "duplicate session") {
			// Not beside a run that could not be ended: they would share the
			// state directory.
			if left, lerr := h.leftover(ctx, stateDir); lerr != nil || len(left) > 0 {
				return ref{}, 0, "", fmt.Errorf("%w, with %d panes of an earlier run (%v)", err, len(left), lerr)
			}
			out, err = h.serverCmd(ctx, slices.Concat([]string{"new-window", "-d", "-t", "=" + name + ":"}, print)...)
		}
		if err != nil {
			return ref{}, 0, "", err
		}
		// The socket is printed last: with it, the rest is whole.
		f := strings.SplitN(strings.TrimSuffix(string(out), "\n"), " ", 5)
		if len(f) == 5 && f[4] != "" && paneID.MatchString(f[0]) {
			server, _ := strconv.Atoi(f[1])
			made, _ := strconv.Atoi(f[2])
			if server > 0 && made > 0 {
				return ref{Socket: f[4], Server: server, Pane: f[0], Dir: stateDir}, made, f[3], nil
			}
		}
		h.clearLeftover(ctx, stateDir)
		cut = fmt.Errorf("tmux: new-session printed %q", out)
	}
	return ref{}, 0, "", cut
}

// setUp is the tmux commands that make the pane Start made the agent's: those
// that keep what becomes of it, and those that run the agent in it.
func (h *Holder) setUp(r ref) (keep, run string) {
	// remain-on-exit is a pane option since tmux 3.3, and a window option
	// before; it is set as both, and again after the respawn, which can set
	// a pane's options back. The window's own goes then, so that a split a
	// person opens in it ends with its program; where there is no pane
	// option, it is set again.
	onPane := "set-option -p -t " + r.Pane + " remain-on-exit on"
	keep = strings.Join([]string{
		"set-option -w -t " + r.Pane + " remain-on-exit on",
		onPane,
		"pipe-pane -O -t " + r.Pane + " " + quote(pipeCommand(h.Hataori, r.Dir)),
	}, " ; ")
	return keep, strings.Join([]string{
		"respawn-pane -k -t " + r.Pane + " " + quote("exec "+quote(h.Hataori)+" exec "+quote(r.Dir)),
		onPane,
		"set-option -w -u -t " + r.Pane + " remain-on-exit",
		onPane,
	}, " ; ")
}

// pipeCommand is shell-quoted first, then escaped for pipe-pane's extra
// format_expand_time pass. Shell quoting alone protects neither # nor %.
func pipeCommand(program, stateDir string) string {
	word := func(s string) string {
		// tmux preserves runs of # before [ as style markers. Adjacent
		// empty shell quotes separate them for tmux and rejoin the path
		// for the shell, so the normal ## escape works on every version.
		return strings.NewReplacer("#", "##", "%", "%%", "[", "''[").Replace(quote(s))
	}
	return "exec " + word(program) + " pipe " + word(stateDir)
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

// quote quotes s for the shell tmux runs a command in, and for tmux's own
// commands, which read single quotes as it does.
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

// startedProcess is the process of the pane of a window hataori started, 0
// when not known and for a window it did not start.
func startedProcess(w holder.Window) int {
	if !w.Owned {
		return 0
	}
	return w.Process
}

// findStarted is find for a window hataori started, whose pane's process is
// process, when known. Once a person gave the pane a program of their own,
// with respawn-pane, the pane is theirs, and the agent's window is gone;
// hataori's log of the pane ends. A respawn without a command runs the agent
// again, which is still hataori's.
func (h *Holder) findStarted(ctx context.Context, r ref, process int) (pane, error) {
	p, err := h.find(ctx, r)
	if err != nil || p.pid > 0 && (process <= 0 || p.pid == process) || process <= 0 && r.Dir == "" {
		return p, err
	}
	out, err := h.formatted(ctx, r, "list-panes", "-t", r.Pane, "-F", "#{pane_id} #{pane_pid} #{pane_start_command}")
	if err != nil {
		return pane{}, err
	}
	pid := "" // a dead pane's, since tmux 3.8
	if p.pid > 0 {
		pid = strconv.Itoa(p.pid)
	}
	for _, l := range strings.Split(string(out), "\n") {
		started, ok := strings.CutPrefix(l, r.Pane+" "+pid+" ")
		if !ok {
			continue
		}
		if runsExec(started, r.Dir) {
			p.start = started
			return p, nil
		}
		// The pane's pipe is hataori's while one runs: a person's own takes
		// its place. Left there, it would keep the next start waiting.
		if r.Dir != "" && host.PipeRuns(r.Dir) {
			h.tmux(context.WithoutCancel(ctx), r, nil, ifPane(r.Pane, "pipe-pane -t "+r.Pane,
				fmt.Sprintf("#{==:#{pid},%d}", r.Server), hasProcess(p.pid, started))...)
		}
		return pane{}, fmt.Errorf("%w: the pane %s runs process %s, not the agent's %d", errGone, r.Pane, pid, process)
	}
	return pane{}, fmt.Errorf("tmux: the pane %s changed as it was looked at: %.200q", r.Pane, out)
}

// reapWait bounds the wait for tmux to reap a process that ended.
const reapWait = 2 * time.Second

// findEnded is findStarted, and has tmux see how the process of a pane ended
// that does not tell it. Now and then tmux misses that, and the process stays
// a zombie until tmux looks at its children again, as after the end of the
// next one. Since tmux 3.8 the pane is not dead until then, and tells no
// process.
func (h *Holder) findEnded(ctx context.Context, r ref, process int) (pane, error) {
	p, err := h.findStarted(ctx, r, process)
	for deadline := time.Now().Add(reapWait); err == nil && (p.dead && p.status == "x" && p.signal == "-" || !p.dead && p.pid == 0); {
		missed := zombie(ctx, cmp.Or(p.pid, execPid(r), process))
		if missed {
			if time.Now().After(deadline) {
				break
			}
			h.tmux(ctx, r, nil, ifPane(r.Pane, "run-shell -b -t "+r.Pane+" true", fmt.Sprintf("#{==:#{pid},%d}", r.Server))...)
			time.Sleep(50 * time.Millisecond)
		}
		p, err = h.findStarted(ctx, r, process) // also when tmux saw it since
		if !missed {
			break
		}
	}
	return p, err
}

// runsExec reports whether a pane's start command, as tmux prints it, is
// `hataori exec <stateDir>`. tmux escapes some characters of it, depending on
// its version: quotes and backslashes are not compared.
func runsExec(started, stateDir string) bool {
	plain := strings.NewReplacer("'", "", `"`, "", `\`, "")
	started = plain.Replace(started)
	return stateDir != "" && strings.HasPrefix(started, "exec ") && strings.HasSuffix(started, plain.Replace(" exec "+stateDir))
}

// startedAlive is Alive for a window hataori started: Running while the
// pane's process runs; Exited, with how it ended, once the pane is dead or
// its end was recorded; Gone when the pane or its server went with nothing
// recorded, as when the server was killed; Unknown when tmux cannot tell.
func (h *Holder) startedAlive(ctx context.Context, r ref, process int) holder.Liveness {
	p, err := h.findEnded(ctx, r, process)
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
	p, err := h.findEnded(ctx, r, w.Process)
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
	return h.removePane(ctx, r, p)
}

// removePane removes the window's pane p, as found, and nothing else: tmux
// ends the window, and the session, with the last pane. What a person opened
// beside it stays. It fails when the pane is still there, and still
// hataori's, as when its agent was run again meanwhile.
func (h *Holder) removePane(ctx context.Context, r ref, p pane) error {
	ctx = context.WithoutCancel(ctx)
	out, err := h.tmux(ctx, r, nil, killPane(r.Pane, p, fmt.Sprintf("#{==:#{pid},%d}", r.Server))...)
	if err != nil && !noServer(err) && !strings.Contains(err.Error(), "can't find") {
		return err
	}
	if strings.TrimSpace(string(out)) != notDone {
		return nil
	}
	// 1 is no pane's: without a process, the pane is told by what it runs.
	q, err := h.findStarted(ctx, r, cmp.Or(p.pid, 1))
	switch {
	case errors.Is(err, errGone):
		return nil
	case err != nil:
		return err
	case p.pid > 0 && q.pid == 0: // it ended since
		return h.removePane(ctx, r, q)
	}
	return fmt.Errorf("tmux: the pane %s is still there", r.Pane)
}

// killPane is the tmux command that removes the pane id when it is as p
// (hasProcess) and conds hold. It does nothing otherwise, as when the pane
// went, or got another program, after it was looked at.
func killPane(id string, p pane, conds ...string) []string {
	return ifPane(id, "kill-pane -t "+id, append(conds, hasProcess(p.pid, p.start))...)
}

// hasProcess is the tmux condition that the pane's process is process, or
// ended with none in its place, where the pane's start command is start, as
// list-panes printed it; with no start, only the first. A dead pane still has
// its process up to tmux 3.7, and none since: a person may have run a program
// of theirs there, which ended too.
func hasProcess(process int, start string) string {
	has := fmt.Sprintf("#{==:#{pane_pid},%d}", process)
	if start == "" {
		return has
	}
	return "#{||:" + has + ",#{&&:#{&&:#{==:#{pane_dead},1},#{?pane_pid,0,1}},#{m:" + startPattern(start) + ",#{pane_start_command}}}}"
}

// startPattern is the pattern a pane's start command is compared with. A
// character that means something in a tmux format or in a pattern, or that is
// not ASCII, matches any one.
func startPattern(start string) string { return special.ReplaceAllString(start, "?") }

var special = regexp.MustCompile(`[^a-zA-Z0-9_./:+@= -]`)

// ifPane is the tmux command that runs do when conds hold for the pane id,
// in one go. It prints notDone when they do not.
func ifPane(id, do string, conds ...string) []string {
	return []string{"if-shell", "-F", "-t", id, all(append(conds, "#{==:#{pane_id},"+id+"}")...), do,
		"display-message -p -t " + id + " " + notDone}
}

// notDone is what ifPane prints when its command was not run.
const notDone = "hataori-not-done"

// killWait bounds the wait for the group to vanish after SIGKILL.
const killWait = 10 * time.Second

// startedStop ends the agent of a window hataori started, as hataori's own
// host does: SIGINT to the pane's process group, then SIGTERM after
// interrupt, then SIGKILL after terminate, stopping as soon as no process of
// the group is left. It records how the agent ended, from
// #{pane_dead_status}, and removes the pane. process is the pane's process
// as Start found it, 0 when not known. The group is signalled only when it
// is told to be the agent's (agentsGroup); where that cannot be told while
// the agent may run, Stop fails and leaves everything as it is.
func (h *Holder) startedStop(ctx context.Context, r ref, process int, interrupt, terminate time.Duration) (holder.StopResult, error) {
	var res holder.StopResult
	p, err := h.findStarted(ctx, r, process)
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
	pgid := cmp.Or(p.pid, execPid(r), process)
	started := cmp.Or(pgid, process)
	var sent []string
	// The group, whenever a process of it is left: also after the pane's
	// own process ended, leaving children.
	left := pgid > 1 && !groupGone(pgid)
	if left {
		left, err = h.agentsGroup(ctx, r, pgid)
		if errors.Is(err, errLeft) {
			res.Left, err = err.Error(), nil
		}
		if err != nil {
			return res, err
		}
	}
	if left {
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
		q, err := h.findEnded(ctx, r, started)
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
	q, err := h.findStarted(ctx, r, started)
	if errors.Is(err, errGone) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	return res, h.removePane(ctx, r, q)
}

// errLeft is why a group of which some may be the agent's is left running:
// the agent itself has ended.
var errLeft = errors.New("processes are left that cannot be told to be the agent's")

// agentsGroup reports whether the group pgid, the number of the process the
// pane has or had, is the agent's. The number is another's once no process
// has it, as its own, its group's or its session's. The agent has it while
// it is the pane's process, or a child of the server whose end the server has
// not read; when another process has it, the agent's group is gone. A process
// of the agent's group is in the session the agent led, and one that ran
// before the agent ended has kept the number since. An error when it cannot
// be told, and the group is left alone then: errLeft once the agent itself
// has ended.
func (h *Holder) agentsGroup(ctx context.Context, r ref, pgid int) (bool, error) {
	cannot := func(ended bool, why string) (bool, error) {
		if !ended {
			return false, fmt.Errorf("tmux: the group %d cannot be told to be the agent's, which may run: %s", pgid, why)
		}
		return false, fmt.Errorf("tmux: group %d: %w: %s", pgid, errLeft, why)
	}
	for deadline := time.Now().Add(deadWait); ; time.Sleep(20 * time.Millisecond) {
		end, err := h.endOf(ctx, r, pgid)
		if err != nil {
			return false, err
		}
		pane := !end.dead && !end.read && end.pid == pgid // the pane's process, as tmux has just told
		ps, err := processes(ctx)
		if err != nil && pane && end.tells {
			return true, nil
		}
		if err != nil {
			return cannot(end.read, err.Error())
		}
		if i := slices.IndexFunc(ps, func(p member) bool { return p.pid == pgid }); i >= 0 {
			child := ps[i].parent == r.Server && !end.read
			switch {
			case child && pane:
				return true, nil
			case child && !end.others && end.tells && (end.pid == pgid || execPid(r) == pgid):
				return true, nil
			case child && !end.others:
				// The server may have read the end of a program that a
				// signal ended, and the number be another child's, such as a
				// popup's: not of one that ran when the agent noted it.
				noted, ok := execNoted(r, pgid)
				if ok && !ps[i].started.After(noted.Add(seconds)) {
					return true, nil
				}
				return cannot(false, "the process of that number may be another")
			case !end.dead && !end.read:
				return cannot(false, "the process of that number is not the pane's")
			}
			return false, nil // it has the agent's number
		}
		own := false
		for _, p := range ps {
			if p.group != pgid {
				continue
			}
			sid, err := unix.Getsid(p.pid)
			if err != nil {
				continue // it ended
			}
			if sid != pgid {
				return false, nil
			}
			own = true
			if !end.at.IsZero() && !p.started.After(end.at.Add(seconds)) {
				return true, nil
			}
		}
		late := time.Now().After(deadline)
		switch {
		case !own && groupGone(pgid):
			return false, nil
		case late && syscall.Kill(pgid, 0) != syscall.ESRCH: // and ps does not list it
			return cannot(end.read, "a process has that number, and ps does not tell of it")
		case late && !own:
			return cannot(true, "ps does not tell of them")
		case own && !end.at.IsZero():
			return cannot(true, "they were started after it ended")
		case own && end.dead && !end.tells:
			return cannot(true, "tmux does not tell when it ended, as up to 3.2")
		case late:
			return cannot(true, "tmux has not told that it ended")
		}
	}
}

// deadWait is how long tmux may take to tell that a program it has read the
// end of has ended: it first writes the pane's output to its pipe.
const deadWait = 2 * time.Second

// seconds is how much later than it was a process's start seems beside
// another time: both are in whole seconds.
const seconds = 2 * time.Second

// paneEnd is what tmux tells of how the program of a pane ended.
type paneEnd struct {
	pid    int
	dead   bool
	read   bool      // tmux has read the process's end: its number may be another's
	at     time.Time // when, in whole seconds; zero when not told
	others bool      // another pane runs a process of the number
	tells  bool      // the server tells of every end it has read, as from 3.3 on; before, not of one by a signal
}

// tellsEnds is the version of a server that tells of every end it has read.
var tellsEnds = regexp.MustCompile(`^(3\.([3-9]|[1-9][0-9])|[4-9])`)

// endOf is the paneEnd of r's pane, whose process had the number pid.
func (h *Holder) endOf(ctx context.Context, r ref, pid int) (paneEnd, error) {
	out, err := h.formatted(ctx, r, "list-panes", "-a", "-F", "#{pid},#{pane_id},#{pane_pid},#{pane_dead},#{pane_dead_time},#{pane_dead_status}#{pane_dead_signal},#{version}")
	if err != nil {
		return paneEnd{}, err
	}
	var e paneEnd
	found := false
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Split(l, ",")
		if len(f) != 7 || f[0] != strconv.Itoa(r.Server) {
			continue
		}
		n, _ := strconv.Atoi(f[2])
		dead, read, tells := f[3] == "1", f[4] != "" || f[5] != "", tellsEnds.MatchString(f[6])
		if f[1] != r.Pane {
			e.others = e.others || n == pid && !read && (!dead || tells)
			continue
		}
		found, e.pid, e.dead, e.read, e.tells = true, n, dead, read, tells
		if at, err := strconv.ParseInt(f[4], 10, 64); err == nil {
			e.at = time.Unix(at, 0)
		}
	}
	if !found {
		// The listing may have left it out: find looks it up by its id.
		if _, err := h.find(ctx, r); !errors.Is(err, errGone) {
			return paneEnd{}, fmt.Errorf("tmux: the pane %s changed as it was looked at: %v", r.Pane, err)
		}
		return paneEnd{dead: true}, nil // and tells nothing more
	}
	return e, nil
}

// execPid is the process id PidFile holds, 0 when there is none.
func execPid(r ref) int {
	if r.Dir == "" {
		return 0
	}
	b, _ := os.ReadFile(filepath.Join(r.Dir, PidFile))
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// execNoted is when `hataori exec` noted pid as the agent's process.
func execNoted(r ref, pid int) (time.Time, bool) {
	if execPid(r) != pid {
		return time.Time{}, false
	}
	fi, err := os.Stat(filepath.Join(r.Dir, PidFile))
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// groupGone reports whether no process of the group runs. One that ended and
// was not reaped does not; signals still go to it.
func groupGone(pgid int) bool { return noGroup(pgid) || onlyZombies(pgid) }

func noGroup(pgid int) bool { return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) }

// onlyZombies looks twice: ps reads one process after the other, and may miss
// one started meanwhile by one that it then reads as ended.
func onlyZombies(pgid int) bool {
	for range 2 {
		ps, err := group(context.Background(), pgid)
		if err != nil || slices.ContainsFunc(ps, func(p member) bool { return !p.zombie }) {
			return false
		}
		if len(ps) == 0 {
			return noGroup(pgid) // reaped since
		}
	}
	return true
}

func waitGone(pgid int, d time.Duration) bool {
	look := time.Now().Add(100 * time.Millisecond) // mostly it is reaped by then
	for deadline := time.Now().Add(d); ; time.Sleep(5 * time.Millisecond) {
		if noGroup(pgid) {
			return true
		}
		if now := time.Now(); now.After(look) {
			if onlyZombies(pgid) {
				return true
			}
			look = time.Now().Add(100 * time.Millisecond)
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
	p, err := h.findStarted(ctx, r, startedProcess(w))
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
	if err := writePrivate(filepath.Join(stateDir, PidFile), []byte(strconv.Itoa(os.Getpid()))); err != nil {
		return err
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

// leftover is the panes of the session of stateDir's name that an earlier
// run left, by their ids: only those hataori started for that state
// directory.
func (h *Holder) leftover(ctx context.Context, stateDir string) (map[string]pane, error) {
	name := h.Prefix + filepath.Base(stateDir)
	// The pane Start made runs the placeholder until the respawn, and
	// `hataori exec <stateDir>` after it. A line that does not end with the
	// pane was cut (makeWindow): tmux escapes a line break of a command. What
	// it was of is not known, so that no listing without it is taken for one
	// of all panes.
	ps := map[string]pane{}
	var cut error
	for range makeTries {
		out, err := h.serverCmd(ctx, "list-panes", "-s", "-t", "="+name+":", "-F", "#{pane_id} #{pane_pid} #{pane_dead} #{pane_start_command} #{pane_id}")
		if err != nil {
			return nil, err
		}
		clear(ps)
		cut = nil
		for _, l := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
			f := strings.SplitN(l, " ", 4)
			start, ended := strings.CutSuffix(f[len(f)-1], " "+f[0])
			if len(f) != 4 || !ended {
				cut = fmt.Errorf("tmux: list-panes printed %.200q", l)
				continue
			}
			if !runsExec(start, stateDir) && strings.Trim(start, `"`) != placeholder {
				continue
			}
			pid, _ := strconv.Atoi(f[1])
			ps[f[0]] = pane{pid: pid, dead: f[2] == "1", start: start}
		}
		if cut == nil {
			break
		}
	}
	return ps, cut
}

// clearLeftover ends the agent an earlier run left and removes its panes.
// tmux ends the session with its last pane.
func (h *Holder) clearLeftover(ctx context.Context, stateDir string) {
	ps, _ := h.leftover(ctx, stateDir) // an error is mostly that there is no such session
	for id, p := range ps {
		if p.pid > 1 && !p.dead {
			syscall.Kill(-p.pid, syscall.SIGKILL)
			waitGone(p.pid, killWait)
		}
		h.serverCmd(context.WithoutCancel(ctx), killPane(id, p)...)
	}
}

// Attached reports whether a tmux client shows the window's session now, as
// when a person looks at it or types in it.
func (h *Holder) Attached(ctx context.Context, w holder.Window) (bool, error) {
	r, err := parseRef(w)
	if err != nil {
		return false, err
	}
	out, err := h.formatted(ctx, r, "display-message", "-p", "-t", r.Pane, "#{session_attached}")
	if err != nil {
		return false, err
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n > 0, nil
}
