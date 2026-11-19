package claudecode

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
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
whether the daemon answers or not; --wait and --timeout are accepted, and
it waits for nothing.
`

// windowHook is `hataori hook --window`, with what it reads from the host.
type windowHook struct {
	getenv   func(string) string
	ppid     int
	terminal func(ctx context.Context, pid int) (proc int, device string, ok bool, err error)
	send     func(ctx context.Context, root string, r window.Report) error
}

// WindowHookMain is `hataori hook --window`. args are the arguments after
// "hook".
func WindowHookMain(args []string, stdin io.Reader, stderr io.Writer, getenv func(string) string) int {
	return windowHook{getenv: getenv, ppid: os.Getppid(), terminal: hostterm.TerminalOf, send: window.Send}.main(args, stdin, stderr)
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
	fs.Bool("wait", false, "")
	fs.Duration("timeout", time.Hour, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || !*isWindow || *root == "" {
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
	var p windowPayload
	if json.Unmarshal(payload, &p) != nil || p.Session == "" || p.Event == "" {
		return host.HookOK
	}
	pid, device, ok, err := h.terminal(ctx, h.ppid)
	if err != nil || !ok {
		return host.HookOK
	}
	model, _ := p.Model.(string)
	h.send(ctx, *root, window.Report{
		Holder: window.Tmux, Server: server, Pane: pane, Device: device, PID: pid,
		Agent: Name, Session: p.Session, Dir: p.Dir, ConfigDir: h.getenv("CLAUDE_CONFIG_DIR"),
		Event: p.Event, Model: model,
		// Observed, not documented: "cli" in an interactive session,
		// "sdk-cli" under -p (design, Appendix B).
		Interactive: h.getenv("CLAUDE_CODE_ENTRYPOINT") == "cli",
	})
	return host.HookOK // whatever happened: any other status shows the person an error
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
