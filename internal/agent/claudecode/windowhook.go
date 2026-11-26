package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/hostterm"
	"github.com/yousan/hataori/internal/window"
)

// windowHookBudget bounds a window hook from its start to its exit: it
// reads the payload, finds the agent's terminal and sends the report within
// it, or gives up and exits all the same.
const windowHookBudget = 600 * time.Millisecond

// maxWindowPayload bounds the payload a window hook reads; a larger one is
// not reported.
const maxWindowPayload = 8 << 20

// WindowHookUsage documents `hataori hook --window`.
const WindowHookUsage = `usage: hataori hook --window --root DIR [--wait] [--timeout DURATION]

The hook of hataori's Claude Code plugin (hataori plugin). In a session that
hataori runs ($` + host.HookEnvVar + ` set) it does nothing. In a session
a person started in a tmux pane, it tells the daemon of the instance at DIR
the pane, the session id, the directory and the event's name, and nothing
of the session's text. It prints nothing and exits 0 within a second,
whether the daemon answers or not.

In a pane that was taken in (hataori take), the hook of the Claude Code
process that was taken in does what hataori hook does, with the thread's
session host: --wait waits for the answer, up to --timeout, and prints it.
`

// windowHook is `hataori hook --window`, with what it reads from the host.
type windowHook struct {
	getenv   func(string) string
	ppid     int
	terminal func(ctx context.Context, pid int) (proc int, device string, ok bool, err error)
	send     func(ctx context.Context, root string, r window.Report) error
	// stdout gets the answer of a call relayed for a window taken in;
	// nothing when nil.
	stdout io.Writer
}

// WindowHookMain is `hataori hook --window`. args are the arguments after
// "hook".
func WindowHookMain(args []string, stdin io.Reader, stderr io.Writer, getenv func(string) string) int {
	return windowHook{getenv: getenv, ppid: os.Getppid(), terminal: hostterm.TerminalOf, send: window.Send,
		stdout: os.Stdout}.main(args, stdin, stderr)
}

// The fields of a hook payload that a report takes. Nothing else of the
// payload is read: not the prompt, a message or a tool's input or result.
type windowPayload struct {
	Session string `json:"session_id"`
	Dir     string `json:"cwd"`
	Event   string `json:"hook_event_name"`
	Model   any    `json:"model"` // a string; some versions may send an object
}

func (h windowHook) main(args []string, stdin io.Reader, stderr io.Writer) int {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	isWindow := fs.Bool("window", false, "")
	root := fs.String("root", "", "")
	wait := fs.Bool("wait", false, "")
	timeout := fs.Duration("timeout", time.Hour, "")
	fs.String("agent", Name, "") // the adapter whose hook this is; hataori names it
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || !*isWindow || *root == "" || *timeout <= 0 {
		fmt.Fprint(stderr, WindowHookUsage)
		return host.HookUsage
	}
	// hataori's own hooks report a session that hataori runs.
	if h.getenv(host.HookEnvVar) != "" {
		return host.HookOK
	}
	server, pane, ok := tmuxWindow(h.getenv("TMUX"), h.getenv("TMUX_PANE"))
	if !ok {
		return host.HookOK // no window this hook can name
	}
	ctx, cancel := context.WithTimeout(context.Background(), windowHookBudget)
	defer cancel()
	payload, ok := readWithin(ctx, stdin, maxWindowPayload)
	if !ok {
		return host.HookOK
	}
	pid, device, ok, err := h.terminal(ctx, h.ppid)
	if err != nil || !ok {
		return host.HookOK
	}
	// A window taken in: the process that was taken in reports to its
	// thread's session host, as hataori's own sessions do.
	// Not one under -p, which an environment inherited could place up the
	// same chain.
	if rec, taken, err := window.ReadRecord(*root, server, pane); err == nil && taken && rec.PID == pid &&
		h.getenv("CLAUDE_CODE_ENTRYPOINT") == "cli" {
		cancel() // the call has its own time: --timeout
		return relay(rec.Socket, payload, *wait, *timeout, h.stdout)
	}
	var p windowPayload
	if json.Unmarshal(payload, &p) != nil || p.Session == "" || p.Event == "" {
		return host.HookOK
	}
	model, _ := p.Model.(string)
	h.send(ctx, *root, window.Report{
		Holder: window.Tmux, Server: server, Pane: pane, Device: device, PID: pid,
		Agent: Name, Session: p.Session, Dir: p.Dir, ConfigDir: windowConfigDir(h.getenv),
		Event: p.Event, Model: model, ConfigDirDefault: h.getenv("CLAUDE_CONFIG_DIR") == "",
		// Observed, not documented: "cli" in an interactive session,
		// "sdk-cli" under -p (design, Appendix B).
		Interactive: h.getenv("CLAUDE_CODE_ENTRYPOINT") == "cli",
	})
	return host.HookOK // whatever happened: any other status shows the person an error
}

// windowConfigDir is the configuration directory the session in the window
// uses: CLAUDE_CONFIG_DIR, or the CLI's default under HOME when it is not
// set. Its resume after the window closes runs with the same one, not the
// daemon's.
func windowConfigDir(getenv func(string) string) string {
	if d := getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	if home := getenv("HOME"); filepath.IsAbs(home) {
		return filepath.Join(home, ".claude")
	}
	return ""
}

// relay hands payload to the session host on socket, as `hataori hook`
// does, and prints the answer of a waiting call to stdout.
func relay(socket string, payload []byte, wait bool, timeout time.Duration, stdout io.Writer) int {
	answer, err := host.Call(socket, payload, wait, timeout)
	switch {
	case errors.Is(err, host.ErrTimeout):
		return host.HookTimeout
	case err != nil:
		return host.HookGone
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if n, err := stdout.Write(answer); err != nil || n != len(answer) {
		return host.HookOutput
	}
	return host.HookOK
}

// tmuxWindow names the tmux pane from TMUX ("<socket>,<server pid>,<session
// index>") and TMUX_PANE ("%3"): the server, without the session's index,
// and the pane.
func tmuxWindow(tmux, pane string) (server, paneID string, ok bool) {
	i := strings.LastIndexByte(tmux, ',')
	if i <= 0 || !strings.HasPrefix(pane, "%") {
		return "", "", false
	}
	server = tmux[:i]
	if !strings.Contains(server, ",") {
		return "", "", false
	}
	return server, pane, true
}

// readWithin reads r to its end, at most limit bytes, before ctx ends.
func readWithin(ctx context.Context, r io.Reader, limit int64) ([]byte, bool) {
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(r, limit+1))
		done <- result{b, err}
	}()
	select {
	case res := <-done:
		return res.b, res.err == nil && int64(len(res.b)) <= limit
	case <-ctx.Done():
		return nil, false
	}
}
