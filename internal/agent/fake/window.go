package fake

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/hostterm"
	"github.com/yousan/hataori/internal/window"
)

// WindowHookMain is `hataori hook --window --agent fake`: the fake's hook in
// a window a person started (the program's --window), as Claude Code's
// plugin hook is for Claude Code. It does what that hook does: in a pane
// taken in, the process taken in hands its report to the thread's session
// host; otherwise it reports the pane, the session and the event to the
// daemon and exits 0 at once.
func WindowHookMain(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	isWindow := fs.Bool("window", false, "")
	root := fs.String("root", "", "")
	wait := fs.Bool("wait", false, "")
	timeout := fs.Duration("timeout", time.Hour, "")
	fs.String("agent", Name, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || !*isWindow || *root == "" {
		return host.HookUsage
	}
	if getenv(host.HookEnvVar) != "" {
		return host.HookOK
	}
	tmux, pane := getenv("TMUX"), getenv("TMUX_PANE")
	i := strings.LastIndexByte(tmux, ',')
	if i <= 0 || !strings.HasPrefix(pane, "%") {
		return host.HookOK
	}
	server := tmux[:i]
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	payload, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return host.HookOK
	}
	pid, device, ok, err := hostterm.TerminalOf(ctx, os.Getppid())
	if err != nil || !ok {
		return host.HookOK
	}
	if rec, taken, err := window.ReadRecord(*root, server, pane); err == nil && taken && rec.PID == pid {
		answer, err := host.Call(rec.Socket, payload, *wait, *timeout)
		switch {
		case errors.Is(err, host.ErrTimeout):
			return host.HookTimeout
		case err != nil:
			return host.HookGone
		}
		stdout.Write(answer)
		return host.HookOK
	}
	var r struct {
		T           string `json:"t"`
		Session     string `json:"session"`
		Cwd         string `json:"cwd"`
		Model       string `json:"model"`
		Interactive bool   `json:"interactive"`
	}
	if json.Unmarshal(payload, &r) != nil || r.Session == "" || r.T == "" {
		return host.HookOK
	}
	window.Send(ctx, *root, window.Report{Holder: window.Tmux, Server: server, Pane: pane, Device: device, PID: pid,
		Agent: Name, Session: r.Session, Dir: r.Cwd, Event: r.T, Model: r.Model, Interactive: r.Interactive})
	return host.HookOK
}
