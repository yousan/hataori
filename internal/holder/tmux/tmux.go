// Package tmux is the terminal holder for tmux windows (design, sections
// 4.3 and 4.8). For a window a person started, it hands the window's agent
// pastes and named keys, shows snapshots of the pane, tells whether that
// agent still runs, and makes the check that comes before a take-in; it
// never starts, stops, respawns or resizes such a window, and sets no
// option on it. A window hataori starts (Start) is a tmux session of
// hataori's own, which it makes, sets up, stops and removes itself.
//
// Text goes only on load-buffer's standard input, into a buffer of the
// holder's own, which paste-buffer deletes; it is never in an argument.
package tmux

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/holder"
)

// Name is the holder's name.
const Name = "tmux"

// Holder holds windows of tmux servers. The zero value runs "tmux" from
// PATH.
type Holder struct {
	Tmux string        // the tmux program; "tmux" when empty
	Poll time.Duration // how often Screen takes a snapshot; 500ms when zero

	// For the windows hataori starts (Start): the hataori binary, whose
	// `exec` and `pipe` the window runs; the start of the name of each of
	// their tmux sessions; and the server they are made on, as the
	// arguments before each tmux command, such as "-L", "name"; empty for
	// the default server. Without Hataori, Start refuses.
	Hataori string
	Prefix  string
	Server  []string

	// noRawPaste holds, by socket, the servers that said they do not know
	// paste-buffer's -S: before 3.7, which paste a buffer as it is.
	noRawPaste sync.Map

	// notPasted holds, by Ref, the windows whose last input with a paste
	// failed (Input).
	notPasted sync.Map
}

var _ holder.Holder = (*Holder)(nil)

// ref is what a Window's Ref holds: the server, by its socket and process
// id, since pane ids start again at %0 on a new server, and the pane.
type ref struct {
	Socket string `json:"socket"`
	Server int    `json:"server"`
	Pane   string `json:"pane"`
	// Dir is the session's state directory, for a window hataori started
	// (Start): how it ended is recorded there once the pane is gone.
	Dir string `json:"dir,omitempty"`
}

var paneID = regexp.MustCompile(`^%[0-9]+$`)

// Window returns the window that a hook's environment names: tmuxEnv is
// its TMUX ("<socket path>,<server pid>,<session index>") and pane its
// TMUX_PANE. agentPID is the agent's process that reported, which the
// window's Process holds.
func Window(tmuxEnv, pane string, agentPID int) (holder.Window, error) {
	// The socket path may hold commas: the last two fields are numbers.
	i := strings.LastIndexByte(tmuxEnv, ',')
	if i < 0 {
		return holder.Window{}, errors.New("tmux: TMUX is not <socket>,<pid>,<session>")
	}
	j := strings.LastIndexByte(tmuxEnv[:i], ',')
	if j <= 0 {
		return holder.Window{}, errors.New("tmux: TMUX is not <socket>,<pid>,<session>")
	}
	pid, err := strconv.Atoi(tmuxEnv[j+1 : i])
	if err != nil || pid <= 0 {
		return holder.Window{}, errors.New("tmux: TMUX names no server process")
	}
	if !paneID.MatchString(pane) {
		return holder.Window{}, errors.New("tmux: TMUX_PANE is not a pane id")
	}
	b, _ := json.Marshal(ref{Socket: tmuxEnv[:j], Server: pid, Pane: pane})
	return holder.Window{Holder: Name, Ref: string(b), Process: agentPID}, nil
}

func parseRef(w holder.Window) (ref, error) {
	var r ref
	if w.Holder != Name {
		return r, fmt.Errorf("tmux: a window of the holder %q", w.Holder)
	}
	if err := json.Unmarshal([]byte(w.Ref), &r); err != nil || r.Socket == "" || r.Server <= 0 || !paneID.MatchString(r.Pane) {
		return ref{}, errors.New("tmux: not a tmux window reference")
	}
	return r, nil
}

// Caps: snapshots of the windows people started; with Hataori set, it
// starts windows too, whose raw output it keeps from the first byte.
func (h *Holder) Caps() holder.Caps {
	if h.Hataori != "" {
		return holder.Caps{CanStart: true, RawScreen: true}
	}
	return holder.Caps{}
}

// Stop ends the agent of a window hataori started (startedStop). For a
// window a person started it refuses and does nothing: hataori never ends a
// window it did not start.
func (h *Holder) Stop(ctx context.Context, w holder.Window, interrupt, terminate time.Duration) (holder.StopResult, error) {
	if !w.Owned {
		return holder.StopResult{}, holder.ErrNotOwned
	}
	r, err := parseRef(w)
	if err != nil {
		return holder.StopResult{}, err
	}
	return h.startedStop(ctx, r, w.Process, interrupt, terminate)
}

// cmdTimeout bounds each tmux and ps command.
const cmdTimeout = 10 * time.Second

// tmux runs one tmux command on the server of r, with stdin on its
// standard input.
func (h *Holder) tmux(ctx context.Context, r ref, stdin []byte, args ...string) ([]byte, error) {
	prog := h.Tmux
	if prog == "" {
		prog = "tmux"
	}
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	return runTmux(ctx, prog, []string{"-S", r.Socket}, args, stdin)
}

// runTmux runs prog with the server's arguments, then a command's args, and
// stdin on its standard input. It returns what prog printed, with an error
// too. With -u the client prints UTF-8 whatever the locale: else, under one
// that is not UTF-8, it prints each character outside ASCII as "_" (#483).
func runTmux(ctx context.Context, prog string, server, args []string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, prog, slices.Concat([]string{"-u"}, server, args)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// The arguments are a fixed command, a buffer name, a pane id and
		// a key name: never text.
		return out, &tmuxError{op: args[0], err: err, msg: strings.TrimSpace(stderr.String())}
	}
	return out, nil
}

type tmuxError struct {
	op  string
	err error
	msg string
}

func (e *tmuxError) Error() string { return fmt.Sprintf("tmux: %s: %v: %s", e.op, e.err, e.msg) }
func (e *tmuxError) Unwrap() error { return e.err }

// noServer reports whether err says that no server listens on the socket.
func noServer(err error) bool {
	var te *tmuxError
	if !errors.As(err, &te) {
		return false
	}
	return strings.Contains(te.msg, "no server running") ||
		strings.HasPrefix(te.msg, "error connecting to") && strings.Contains(te.msg, "No such file or directory")
}

// serverGone is what no server on the socket means: the window is gone
// when the reference's server process is too. A socket file that was
// removed, as by a cleaner of temporary files, leaves a server that runs
// on and cannot be reached: that is not known to be gone.
func serverGone(r ref) error {
	if errors.Is(syscall.Kill(r.Server, 0), syscall.ESRCH) {
		return fmt.Errorf("%w: no server answers on the socket, and process %d has ended", errGone, r.Server)
	}
	// A process of that id that is not tmux, as after a reboot that gave the
	// id to another, is not the server.
	if out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(r.Server)).Output(); err == nil &&
		!strings.Contains(filepath.Base(strings.TrimSpace(string(out))), "tmux") {
		return fmt.Errorf("%w: no server answers on the socket, and process %d is %q", errGone, r.Server, strings.TrimSpace(string(out)))
	}
	return errors.New("tmux: the server's socket does not answer, and its process runs")
}

// noPanes reports whether a list-panes error says that the server has no
// pane to list, as a server with exit-empty off and no session left.
func noPanes(err error) bool {
	var te *tmuxError
	return errors.As(err, &te) && (strings.Contains(te.msg, "no current target") || strings.Contains(te.msg, "no sessions"))
}

// pane is what list-panes says of the window's pane.
type pane struct {
	tty           string
	width, height string
	pid           int    // the pane's process, which leads its process group
	dead          bool   // remain-on-exit kept the pane after its process ended
	status        string // how it ended, when dead: "x" and its exit status, or "-"
	signal        string // the signal that ended it, when dead and known, or "-"
	session       string // the name of the session it is in, each U+0020 as "_"
}

// paneFormat is what find asks list-panes for. Spaces between the fields:
// without -u, tmux may print a tab in a format as "_" (#483). Only the
// session's name, the last, may hold anything a person chose: without the
// U+0020, as tmux up to 3.1 prints a newline of it as it is, and what follows
// must not read as another pane's line. Every other field prints something,
// "-" for nothing: a dead pane may have no terminal any more.
const paneFormat = "#{pid} #{pane_id} #{?pane_tty,#{pane_tty},-} #{pane_width} #{pane_height} #{?pane_pid,#{pane_pid},-} #{pane_dead} " +
	"#{?pane_dead,x#{pane_dead_status},-} #{?pane_dead_signal,#{pane_dead_signal},-} #{s/ /_/:session_name}"

// parsePane reads one line of list-panes in paneFormat: the server's
// process id, the pane's id and the pane.
func parsePane(l string) (server int, id string, p pane, ok bool) {
	f := strings.SplitN(l, " ", 10)
	if len(f) != 10 || !paneID.MatchString(f[1]) {
		return 0, "", pane{}, false
	}
	server, err := strconv.Atoi(f[0])
	if err != nil {
		return 0, "", pane{}, false
	}
	p = pane{tty: f[2], width: f[3], height: f[4], dead: f[6] == "1", status: f[7], signal: f[8], session: f[9]}
	if p.tty == "-" {
		p.tty = ""
	}
	p.pid, _ = strconv.Atoi(f[5])
	return server, f[1], p, true
}

// errGone is the reason behind holder.Gone: the window is not there.
var errGone = errors.New("tmux: the window is gone")

// find returns the window's pane, errGone when the server is another or
// does not have the pane, and another error when tmux cannot tell. Gone is
// a definite answer: output that names no pane says nothing about this
// one, and a pane that one listing leaves out is looked up by its id before
// it counts as gone (#497).
func (h *Holder) find(ctx context.Context, r ref) (pane, error) {
	out, err := h.tmux(ctx, r, nil, "list-panes", "-a", "-F", paneFormat)
	if noServer(err) {
		return pane{}, serverGone(r)
	}
	if noPanes(err) {
		return pane{}, fmt.Errorf("%w: the server has no pane: %v", errGone, err)
	}
	if err != nil {
		return pane{}, err
	}
	lines := 0
	for _, l := range strings.Split(string(out), "\n") {
		pid, id, p, ok := parsePane(l)
		if !ok {
			continue
		}
		if pid != r.Server {
			// another server on the same socket: its %N is another pane
			return pane{}, fmt.Errorf("%w: the server on the socket is process %d, not %d", errGone, pid, r.Server)
		}
		lines++
		if id == r.Pane {
			return p, nil
		}
	}
	if lines == 0 {
		return pane{}, fmt.Errorf("tmux: list-panes listed no pane: %.200q", out)
	}
	// display-message on a pane that has gone exits 0 with its fields
	// empty; on one that is there it prints the pane's id.
	out, err = h.tmux(ctx, r, nil, "display-message", "-p", "-t", r.Pane, "#{pane_id}")
	if err != nil {
		return pane{}, err
	}
	if got := strings.TrimSpace(string(out)); got != "" {
		return pane{}, fmt.Errorf("tmux: list-panes left out the pane %s, which display-message finds as %q", r.Pane, got)
	}
	return pane{}, fmt.Errorf("%w: the server does not have the pane %s, among its %d", errGone, r.Pane, lines)
}

// Encodings of the named keys: tmux's name for each, which send-keys
// encodes for the pane's current mode.
var keyNames = map[agent.Key]string{
	agent.Enter:  "Enter",
	agent.Escape: "Escape",
	agent.Up:     "Up",
	agent.Down:   "Down",
}

// bufferPrefix starts the name of every buffer the holder makes.
const bufferPrefix = "hataori-holder-"

func newBuffer() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("tmux: %w", err)
	}
	return bufferPrefix + hex.EncodeToString(b[:]), nil
}

// ErrTakesNoInput refuses input to a pane where it would not reach the
// agent's program as it should, or not only that program:
//   - the pane is in one of tmux's own modes, as copy mode while a person
//     has scrolled back in it: a paste would lose its marks, and a key
//     would go to the mode;
//   - its program has ended: tmux would drop a key, and up to 3.3a a paste
//     ends the whole server, with every window a person has on it;
//   - its input is off (select-pane -d): tmux would drop the input;
//   - the panes of its window are synchronized: tmux may type each key into
//     the other panes too, which are a person's;
//   - its foreground process changed as the input was handed over, in a
//     window hataori did not start: a shell would run the text as a command.
var ErrTakesNoInput = errors.New("tmux: the window's pane takes no input as it is: its program has ended, it is in a mode such as copy mode, its input is off, the window's panes are synchronized, or its foreground process changed; nothing was sent")

// errNotPasted refuses an Enter whose paste was refused.
var errNotPasted = errors.New("tmux: the paste before this Enter was not handed over; nothing was sent")

// errNoName refuses input to an agent whose name has nothing that tells it
// from any other program (unsure).
var errNoName = errors.New("tmux: the name of the window's foreground process cannot be compared; nothing was sent")

// What the command of Input prints: that every part was handed over, or
// that none was.
const (
	saidHanded  = "hataori-handed"
	saidRefused = "hataori-refused"
)

// Input hands the parts to the window's pane in order: a paste by
// load-buffer from standard input into a buffer of the holder's own, then
// paste-buffer -p -r -d, which pastes it unchanged, bracketed when the
// program asked for bracketed paste, and deletes it; a key by send-keys
// with tmux's name for it. A part it cannot encode refuses the whole input
// before anything runs, and so do an empty paste, a window that is gone and
// a context that ended. A pane that takes no input as it is refuses it too
// (ErrTakesNoInput): tmux looks at the pane and takes all the parts in one
// command, so that a person who scrolls back meanwhile comes before all of
// them or after. When the input fails, its buffers are deleted.
//
// Input to a window hataori did not start goes only to the agent that
// reported (w.Process), while it is the window's foreground process (as
// CheckTakeIn): suspended or ended, it left the foreground to someone's
// shell. That the pane's process and the name of its foreground process are
// those seen with the agent in the foreground is part of the one command too.
// tmux tells the name only: another program of the same name, started right
// after the look, is not told from the agent, and neither is one that reads
// what tmux has written before the agent did.
//
// After an input with a paste that failed, or of which it is not known that
// it was handed over, an Enter is refused until a paste is handed over: it
// was meant for that paste, and would submit what a person has typed in the
// window.
func (h *Holder) Input(ctx context.Context, w holder.Window, parts []agent.Part) error {
	r, err := parseRef(w)
	if err != nil {
		return err
	}
	_, notPasted := h.notPasted.Load(w.Ref)
	pastes := slices.ContainsFunc(parts, func(p agent.Part) bool { return p.Paste != "" })
	if pastes {
		h.notPasted.Store(w.Ref, true)
	}
	enterFirst := false // before any paste of this input
	for i, p := range parts {
		if p.Key == "" {
			if p.Paste == "" {
				return fmt.Errorf("tmux: part %d is empty", i+1)
			}
			continue
		}
		if p.Paste != "" {
			return fmt.Errorf("tmux: part %d is both a paste and a key", i+1)
		}
		if _, ok := keyNames[p.Key]; !ok {
			return fmt.Errorf("tmux: part %d: no key %q", i+1, p.Key)
		}
		if p.Key == agent.Enter && !slices.ContainsFunc(parts[:i], func(p agent.Part) bool { return p.Paste != "" }) {
			enterFirst = true
		}
	}
	if notPasted && enterFirst {
		return errNotPasted
	}
	if err := h.hand(ctx, r, w, parts); err != nil {
		return err
	}
	if pastes {
		h.notPasted.Delete(w.Ref)
	}
	return nil
}

// hand hands the parts over, all of them or, with an error, none, unless
// tmux failed halfway.
func (h *Holder) hand(ctx context.Context, r ref, w holder.Window, parts []agent.Part) (err error) {
	conds, err := h.takes(ctx, r, w)
	if err != nil {
		return err
	}
	// Not cut short by ctx: a tmux client killed halfway does not undo
	// what it asked of the server, and the buffers must still be deleted.
	cctx := context.WithoutCancel(ctx)
	var bufs []string
	defer func() {
		if err != nil {
			err = errors.Join(err, h.deleteBuffers(cctx, r, bufs))
		}
	}()
	for _, p := range parts {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tmux: the input was stopped: %w", err)
		}
		if p.Key != "" {
			continue
		}
		buf, err := newBuffer()
		if err != nil {
			return err
		}
		bufs = append(bufs, buf)
		if _, err := h.tmux(cctx, r, []byte(p.Paste), "load-buffer", "-b", buf, "-"); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("tmux: the input was stopped: %w", err)
	}
	// Since 3.7 tmux pastes a buffer's control characters as ^X unless
	// given -S. An older server refuses -S as it reads the commands, before
	// it runs any of them, and pastes the buffer as it is (noRawFlag).
	_, old := h.noRawPaste.Load(r.Socket)
	raw := !old && len(bufs) > 0
	out, err := h.tmux(cctx, r, nil, handOver(r, conds, parts, bufs, raw)...)
	if raw && noRawFlag(err) {
		h.noRawPaste.Store(r.Socket, true)
		out, err = h.tmux(cctx, r, nil, handOver(r, conds, parts, bufs, false)...)
	}
	// A hook a person set on these commands may print lines of its own, and
	// may fail after a part was handed over.
	said := strings.Fields(string(out))
	switch {
	case slices.Contains(said, saidHanded):
		return nil
	case slices.Contains(said, saidRefused):
		if _, err := h.takes(ctx, r, w); err != nil {
			return err // gone, or not the agent's
		}
		return ErrTakesNoInput
	case err != nil:
		return err
	default:
		return fmt.Errorf("tmux: the server answered an input with %.200q", out)
	}
}

// noRawFlag reports whether a tmux error says that paste-buffer does not
// know -S. tmux names the flag since 3.3; before, it answers with the usage
// of paste-buffer, whose flags have no S, and nothing more inside if-shell.
func noRawFlag(err error) bool {
	var te *tmuxError
	return errors.As(err, &te) && (strings.Contains(te.msg, "unknown flag -S") || strings.HasPrefix(te.msg, "usage: paste-buffer [-dpr] "))
}

// takes is the tmux condition that what the window's pane runs is what it
// runs now, the window's agent; an error when it does not.
func (h *Holder) takes(ctx context.Context, r ref, w holder.Window) ([]string, error) {
	if w.Owned {
		p, err := h.findStarted(ctx, r, w.Process)
		if err != nil || w.Process <= 0 {
			return nil, err
		}
		return []string{fmt.Sprintf("#{==:#{pane_pid},%d}", p.pid)}, nil
	}
	look := func() (pane, error) {
		p, err := h.foreground(ctx, r, w.Process)
		switch {
		case p.dead:
			return p, ErrTakesNoInput
		case err != nil:
			return p, fmt.Errorf("%w; nothing was sent", err)
		}
		return p, nil
	}
	// The name between two looks at what has it.
	if _, err := look(); err != nil {
		return nil, err
	}
	out, nameErr := h.tmux(ctx, r, nil, "list-panes", "-t", r.Pane, "-F", "#{pane_id} #{pane_pid} #{pane_current_command}")
	p, err := look()
	if err != nil {
		return nil, err
	}
	if nameErr != nil {
		return nil, nameErr
	}
	for _, l := range strings.Split(string(out), "\n") {
		if name, ok := strings.CutPrefix(l, fmt.Sprintf("%s %d ", r.Pane, p.pid)); ok {
			if name = unsure.ReplaceAllString(name, "*"); name == "*" {
				return nil, errNoName
			}
			return []string{fmt.Sprintf("#{==:#{pane_pid},%d}", p.pid), "#{m:" + name + ",#{pane_current_command}}"}, nil
		}
	}
	return nil, fmt.Errorf("tmux: the pane %s changed as it was looked at: %.200q", r.Pane, out)
}

// unsure is what of a program's name is left out of a comparison: what means
// something in a tmux format or a pattern, and what tmux prints in another
// form than it has it: bytes that are not UTF-8 and control characters as
// escapes and, to a client without UTF-8, all that is not ASCII as "_".
var unsure = regexp.MustCompile(`(\\[0-7]{3}|\\[a-z]|[^a-zA-Z0-9./:+@=-])+`)

// handOver is the tmux command that hands the parts over, the pastes from
// bufs, when the pane takes input (ErrTakesNoInput), conds hold and it is on
// the window's own server, and that hands over nothing otherwise. tmux runs
// the commands of an if-shell -F one right after the other and after its
// condition: no key of a person and no command of another client comes
// between them. For a pane that is gone, tmux reads the condition on another
// pane or on none: the pane's id is part of it.
func handOver(r ref, conds []string, parts []agent.Part, bufs []string, raw bool) []string {
	var cmds []string
	for _, p := range parts {
		if p.Key != "" {
			cmds = append(cmds, "send-keys -t "+r.Pane+" "+keyNames[p.Key])
			continue
		}
		c := "paste-buffer -p -r -d -b " + bufs[0] + " -t " + r.Pane
		if raw {
			c += " -S"
		}
		cmds, bufs = append(cmds, c), bufs[1:]
	}
	cmds = append(cmds, "display-message -p "+saidHanded)
	conds = append([]string{fmt.Sprintf("#{==:#{pid},%d}", r.Server), "#{==:#{pane_id}," + r.Pane + "}", "#{==:#{pane_dead},0}", "#{==:#{pane_in_mode},0}", "#{==:#{pane_input_off},0}",
		"#{||:#{==:#{pane_synchronized},0},#{==:#{window_panes},1}}"}, conds...)
	return []string{"if-shell", "-F", "-t", r.Pane, all(conds...), strings.Join(cmds, " ; "), "display-message -p " + saidRefused}
}

// all is the tmux condition that holds when each of conds does.
func all(conds ...string) string {
	c := conds[0]
	for _, d := range conds[1:] {
		c = "#{&&:" + c + "," + d + "}"
	}
	return c
}

// deleteBuffers deletes those of bufs that are still there.
func (h *Holder) deleteBuffers(ctx context.Context, r ref, bufs []string) error {
	var errs []error
	for _, buf := range bufs {
		if _, err := h.tmux(ctx, r, nil, "delete-buffer", "-b", buf); err != nil && !noBuffer(err) && !noServer(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// noBuffer reports whether a tmux error says that the buffer is not there.
func noBuffer(err error) bool {
	var te *tmuxError
	return errors.As(err, &te) && (strings.Contains(te.msg, "unknown buffer") || strings.Contains(te.msg, "no buffer"))
}

// proc is what ps says of one process.
type proc struct {
	pgid, tpgid int
	tty         string // "/dev/..." or "" without a terminal
}

// errNoProcess says that no process has the pid.
var errNoProcess = errors.New("tmux: no such process")

// process reads pid's process group, its terminal's foreground group and
// its terminal.
func process(ctx context.Context, pid int) (proc, error) {
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-o", "pgid=,tpgid=,tty=", "-p", strconv.Itoa(pid))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	f := strings.Fields(string(out))
	if err != nil && len(f) == 0 {
		var ee *exec.ExitError
		// ps lists nothing, says nothing and exits 1 for a pid that has
		// none. One killed at the deadline says nothing either: that is not
		// known to be gone.
		if errors.As(err, &ee) && ee.ExitCode() == 1 && stderr.Len() == 0 && ctx.Err() == nil {
			return proc{}, errNoProcess
		}
		return proc{}, fmt.Errorf("tmux: ps: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if len(f) != 3 {
		return proc{}, fmt.Errorf("tmux: ps: %q", out)
	}
	var p proc
	var err1, err2 error
	p.pgid, err1 = strconv.Atoi(f[0])
	p.tpgid, err2 = strconv.Atoi(f[1])
	if err1 != nil || err2 != nil {
		return proc{}, fmt.Errorf("tmux: ps: %q", out)
	}
	if f[2] != "?" && f[2] != "??" && f[2] != "-" {
		p.tty = "/dev/" + strings.TrimPrefix(f[2], "/dev/")
	}
	return p, nil
}

// The reasons the take-in check refuses a window.
var (
	ErrNotItsTerminal = errors.New("tmux: the process that reported does not run on the window's terminal")
	ErrNotForeground  = errors.New("tmux: the process that reported is not the window's foreground process")
)

// The oldest tmux whose windows are held. A pane that is gone, for one,
// reads as an error on an older one, not as gone.
const oldestMajor, oldestMinor = 3, 2

// ErrTooOld refuses a window on a server that is older.
var ErrTooOld = fmt.Errorf("tmux: the server is older than tmux %d.%d, the oldest hataori holds windows of", oldestMajor, oldestMinor)

// "3.2a", "next-3.5", "3.4-rc"; a build with a name of its own, as "master",
// has no number.
var versionNumber = regexp.MustCompile(`^(?:next-)?([0-9]+)\.([0-9]+)`)

// checkVersion refuses a version, as the server prints it for #{version},
// that is older than the oldest whose windows are held. A server so old that
// it does not know #{version} prints nothing.
func checkVersion(version string) error {
	if version == "" {
		return fmt.Errorf("%w: it prints no version", ErrTooOld)
	}
	m := versionNumber.FindStringSubmatch(version)
	if m == nil {
		return nil
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < oldestMajor || major == oldestMajor && minor < oldestMinor {
		return fmt.Errorf("%w: it is %s", ErrTooOld, version)
	}
	return nil
}

// CheckTakeIn is the check before a take-in (design, section 4.8): the
// window exists, the agent's process that reported (w.Process) has the
// pane's terminal as its terminal, and leads that terminal's foreground
// process group, and the server is not too old (ErrTooOld). An environment
// is inherited, so a process the session's own shell started names the same
// pane. nil only when every answer is yes; an error when any is no or cannot
// be told.
func (h *Holder) CheckTakeIn(ctx context.Context, w holder.Window) error {
	r, err := parseRef(w)
	if err != nil {
		return err
	}
	if _, err := h.foreground(ctx, r, w.Process); err != nil {
		return err
	}
	out, err := h.tmux(ctx, r, nil, "display-message", "-p", "-t", r.Pane, "#{version}")
	if err != nil {
		return err
	}
	return checkVersion(strings.TrimSpace(string(out)))
}

// foreground returns the window's pane, with an error when pid does not lead
// the foreground process group of its terminal, or that cannot be told.
func (h *Holder) foreground(ctx context.Context, r ref, pid int) (pane, error) {
	if pid <= 0 {
		return pane{}, errors.New("tmux: the window names no process that reported")
	}
	p, err := h.find(ctx, r)
	if err != nil {
		return pane{}, err
	}
	pr, err := process(ctx, pid)
	if err != nil {
		return p, err
	}
	if pr.tty == "" || pr.tty != p.tty {
		return p, ErrNotItsTerminal
	}
	if pr.pgid != pid || pr.tpgid != pr.pgid {
		return p, ErrNotForeground
	}
	return p, nil
}

// Alive says Gone for a pane that the server no longer lists, and for a
// reference whose server is not the one running on its socket; Running
// while the agent's process the hooks named (w.Process) leads the pane
// terminal's foreground process group; Exited, with no status, once that
// process is gone while the pane stays; and Unknown otherwise, as when tmux
// cannot be reached.
func (h *Holder) Alive(ctx context.Context, w holder.Window) (holder.Liveness, error) {
	r, err := parseRef(w)
	if err != nil {
		return holder.Liveness{}, err
	}
	if w.Owned {
		return h.startedAlive(ctx, r, w.Process), nil
	}
	if _, err := h.find(ctx, r); errors.Is(err, errGone) {
		return holder.Liveness{State: holder.Gone}, nil
	} else if err != nil {
		return holder.Liveness{State: holder.Unknown}, nil
	}
	if w.Process <= 0 {
		return holder.Liveness{State: holder.Unknown}, nil
	}
	switch _, err := h.foreground(ctx, r, w.Process); {
	case err == nil:
		return holder.Liveness{State: holder.Running}, nil
	case errors.Is(err, errNoProcess):
		return holder.Liveness{State: holder.Exited}, nil
	case errors.Is(err, errGone):
		return holder.Liveness{State: holder.Gone}, nil
	default:
		return holder.Liveness{State: holder.Unknown}, nil
	}
}

// snapshotRun is the Run of every snapshot: it counts snapshots, not bytes.
const snapshotRun = "tmux-snapshots"

func (h *Holder) poll() time.Duration {
	if h.Poll > 0 {
		return h.Poll
	}
	return 500 * time.Millisecond
}

// Screen sends a snapshot of the pane, marked Reset, at once and then each
// time it changes, from capture-pane, which changes nothing. run and after
// are not used: a snapshot has no position. The channel is closed when ctx
// ends or the window is gone.
func (h *Holder) Screen(ctx context.Context, w holder.Window, run string, after uint64) (<-chan control.TerminalChunk, error) {
	r, err := parseRef(w)
	if err != nil {
		return nil, err
	}
	out := make(chan control.TerminalChunk)
	go func() {
		defer close(out)
		var last []byte
		var n uint64
		for ctx.Err() == nil {
			p, err := h.findStarted(ctx, r, startedProcess(w))
			if errors.Is(err, errGone) {
				return
			}
			if err == nil {
				snap, err := h.tmux(ctx, r, nil, "capture-pane", "-p", "-e", "-t", r.Pane)
				if err == nil && (n == 0 || !bytes.Equal(snap, last)) {
					last, n = snap, n+1
					cols, _ := strconv.Atoi(p.width)
					rows, _ := strconv.Atoi(p.height)
					data := bytes.ReplaceAll(bytes.TrimSuffix(snap, []byte("\n")), []byte("\n"), []byte("\r\n"))
					select {
					case out <- control.TerminalChunk{Run: snapshotRun, Pos: n, Reset: true, Cols: cols, Rows: rows, Data: data}:
					case <-ctx.Done():
						return
					}
				}
			}
			select {
			case <-ctx.Done():
			case <-time.After(h.poll()):
			}
		}
	}()
	return out, nil
}
