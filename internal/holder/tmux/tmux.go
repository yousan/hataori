// Package tmux is the terminal holder for a tmux window that a person
// started (design, sections 4.3 and 4.8): it hands the window's agent
// pastes and named keys, shows snapshots of the pane, tells whether that
// agent still runs, and makes the check that comes before a take-in.
// It never starts, stops, respawns or resizes a window, and sets no option.
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
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
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

	// noRawPaste is set once tmux has said it does not know paste-buffer's
	// -S: a server before 3.7, which pastes a buffer as it is.
	noRawPaste atomic.Bool
}

var _ holder.Holder = (*Holder)(nil)

// ref is what a Window's Ref holds: the server, by its socket and process
// id, since pane ids start again at %0 on a new server, and the pane.
type ref struct {
	Socket string `json:"socket"`
	Server int    `json:"server"`
	Pane   string `json:"pane"`
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

// Caps: no start, and snapshots only.
func (h *Holder) Caps() holder.Caps { return holder.Caps{} }

// Start refuses: this holder holds only windows a person started.
func (h *Holder) Start(ctx context.Context, stateDir string, spec holder.Spec) (holder.Window, error) {
	return holder.Window{}, holder.ErrCannotStart
}

// Stop refuses and does nothing: hataori never ends a window it did not
// start.
func (h *Holder) Stop(ctx context.Context, w holder.Window, interrupt, terminate time.Duration) (holder.StopResult, error) {
	return holder.StopResult{}, holder.ErrNotOwned
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
	cmd := exec.CommandContext(ctx, prog, append([]string{"-S", r.Socket}, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// The arguments are a fixed command, a buffer name, a pane id and
		// a key name: never text.
		return nil, &tmuxError{op: args[0], err: err, msg: strings.TrimSpace(stderr.String())}
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

// noServer reports whether err says that no server listens on the socket:
// the server, and so every window of it, is gone.
func noServer(err error) bool {
	var te *tmuxError
	if !errors.As(err, &te) {
		return false
	}
	return strings.Contains(te.msg, "no server running") ||
		strings.HasPrefix(te.msg, "error connecting to") && strings.Contains(te.msg, "No such file or directory")
}

// pane is what list-panes says of the window's pane.
type pane struct {
	tty           string
	width, height string
}

// errGone is the reason behind holder.Gone: the window is not there.
var errGone = errors.New("tmux: the window is gone")

// find returns the window's pane, errGone when the server is another or
// does not list the pane, and another error when tmux cannot tell.
func (h *Holder) find(ctx context.Context, r ref) (pane, error) {
	out, err := h.tmux(ctx, r, nil, "display-message", "-p", "#{pid}")
	if noServer(err) {
		return pane{}, errGone
	}
	if err != nil {
		return pane{}, err
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(out))); err != nil || pid != r.Server {
		return pane{}, errGone // another server on the same socket: its %N is another pane
	}
	// display-message on a pane that has gone exits 0 with empty fields, so
	// whether it exists is read from list-panes.
	// Spaces between the fields, none of which holds one: tmux 3.5 prints
	// a tab in a format as "_".
	out, err = h.tmux(ctx, r, nil, "list-panes", "-a", "-F", "#{pane_id} #{pane_tty} #{pane_width} #{pane_height}")
	if noServer(err) {
		return pane{}, errGone
	}
	if err != nil {
		return pane{}, err
	}
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) == 4 && f[0] == r.Pane {
			return pane{tty: f[1], width: f[2], height: f[3]}, nil
		}
	}
	return pane{}, errGone
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

// Input hands the parts to the window's pane in order: a paste by
// load-buffer from standard input into a buffer of the holder's own, then
// paste-buffer -p -r -d, which pastes it unchanged, bracketed when the
// program asked for bracketed paste, and deletes it; a key by send-keys
// with tmux's name for it. A part it cannot encode refuses the whole input
// before anything runs, and so does a window that is gone. When a paste
// fails, its buffer is deleted. A failed part ends the input.
func (h *Holder) Input(ctx context.Context, w holder.Window, parts []agent.Part) error {
	r, err := parseRef(w)
	if err != nil {
		return err
	}
	for i, p := range parts {
		if p.Key == "" {
			continue
		}
		if p.Paste != "" {
			return fmt.Errorf("tmux: part %d is both a paste and a key", i+1)
		}
		if _, ok := keyNames[p.Key]; !ok {
			return fmt.Errorf("tmux: part %d: no key %q", i+1, p.Key)
		}
	}
	if _, err := h.find(ctx, r); err != nil {
		return err
	}
	for _, p := range parts {
		if p.Key != "" {
			if _, err := h.tmux(ctx, r, nil, "send-keys", "-t", r.Pane, keyNames[p.Key]); err != nil {
				return err
			}
			continue
		}
		if err := h.paste(ctx, r, p.Paste); err != nil {
			return err
		}
	}
	return nil
}

// paste pastes text into the pane through a buffer of its own, which is
// gone afterwards whatever happened.
func (h *Holder) paste(ctx context.Context, r ref, text string) error {
	buf, err := newBuffer()
	if err != nil {
		return err
	}
	// Not cut short by ctx: a tmux client killed halfway does not undo
	// what it asked of the server, and the buffer must still be deleted.
	cctx := context.WithoutCancel(ctx)
	_, err = h.tmux(cctx, r, []byte(text), "load-buffer", "-b", buf, "-")
	if err == nil {
		args := []string{"paste-buffer", "-p", "-r", "-d", "-b", buf, "-t", r.Pane}
		// Since 3.7 tmux pastes a buffer's control characters as ^X unless
		// given -S. An older server refuses -S before it does anything, and
		// pastes the buffer as it is.
		if !h.noRawPaste.Load() {
			_, err = h.tmux(cctx, r, nil, append(args, "-S")...)
			var te *tmuxError
			if errors.As(err, &te) && strings.Contains(te.msg, "unknown flag -S") {
				h.noRawPaste.Store(true)
				_, err = h.tmux(cctx, r, nil, args...)
			}
		} else {
			_, err = h.tmux(cctx, r, nil, args...)
		}
	}
	if err != nil {
		if _, derr := h.tmux(cctx, r, nil, "delete-buffer", "-b", buf); derr != nil && !noBuffer(derr) && !noServer(derr) {
			return errors.Join(err, derr)
		}
	}
	return err
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
	out, err := cmd.Output()
	f := strings.Fields(string(out))
	if err != nil && len(f) == 0 {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return proc{}, errNoProcess // ps lists nothing for a pid that has none
		}
		return proc{}, fmt.Errorf("tmux: ps: %w", err)
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

// CheckTakeIn is the check before a take-in (design, section 4.8): the
// window exists, the agent's process that reported (w.Process) has the
// pane's terminal as its terminal, and leads that terminal's foreground
// process group. An environment is inherited, so a process the session's
// own shell started names the same pane. nil only when every answer is
// yes; an error when any is no or cannot be told.
func (h *Holder) CheckTakeIn(ctx context.Context, w holder.Window) error {
	r, err := parseRef(w)
	if err != nil {
		return err
	}
	if w.Process <= 0 {
		return errors.New("tmux: the window names no process that reported")
	}
	p, err := h.find(ctx, r)
	if err != nil {
		return err
	}
	pr, err := process(ctx, w.Process)
	if err != nil {
		return err
	}
	if pr.tty == "" || pr.tty != p.tty {
		return ErrNotItsTerminal
	}
	if pr.pgid != w.Process || pr.tpgid != pr.pgid {
		return ErrNotForeground
	}
	return nil
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
	if _, err := h.find(ctx, r); errors.Is(err, errGone) {
		return holder.Liveness{State: holder.Gone}, nil
	} else if err != nil {
		return holder.Liveness{State: holder.Unknown}, nil
	}
	if w.Process <= 0 {
		return holder.Liveness{State: holder.Unknown}, nil
	}
	switch err := h.CheckTakeIn(ctx, w); {
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
			p, err := h.find(ctx, r)
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
