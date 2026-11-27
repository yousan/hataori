package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/holder"
	"github.com/yousan/hataori/internal/holder/tmux"
)

// This test uses a private server and a gate so the runner records the
// window before its program closes the terminal. Runtime execution is CI-only.
func TestClosedTerminalAgentSurvivesPollingAndCanBeStopped(t *testing.T) {
	prog, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("no tmux")
	}
	root, err := os.MkdirTemp("", "hr-close")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	socket := filepath.Join(root, "s")
	on := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(prog, append([]string{"-S", socket, "-f", "/dev/null"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("private tmux: %v: %s", err, out)
		}
		return out
	}
	t.Cleanup(func() { exec.Command(prog, "-S", socket, "kill-server").Run() })
	dir := filepath.Join(root, "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(root, "close")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	command := "exec sh -c " + quote("until [ -e "+quote(gate)+" ]; do sleep 0.02; done; trap '' HUP; exec </dev/null >/dev/null 2>&1; exec sleep 600") + " # exec " + quote(dir)
	pid, err := strconv.Atoi(strings.TrimSpace(string(on("new-session", "-d", "-s", "held", "-P", "-F", "#{pane_pid}", command))))
	if err != nil {
		t.Fatal(err)
	}
	on("set-option", "-w", "-t", "=held:", "remain-on-exit", "on")
	pane := strings.TrimSpace(string(on("display-message", "-p", "-t", "=held:", "#{pane_id}")))
	env := strings.TrimSpace(string(on("display-message", "-p", "-t", pane, "#{socket_path},#{pid},0")))
	w, err := tmux.Window(env, pane, pid)
	if err != nil {
		t.Fatal(err)
	}
	var ref map[string]any
	if err := json.Unmarshal([]byte(w.Ref), &ref); err != nil {
		t.Fatal(err)
	}
	ref["dir"] = dir
	b, _ := json.Marshal(ref)
	w.Ref, w.Owned = string(b), true
	b, _ = json.Marshal(w)
	if err := os.WriteFile(filepath.Join(dir, HeldFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tmux.PidFile), []byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	h := &tmux.Holder{Tmux: prog, Server: []string{"-S", socket, "-f", "/dev/null"}}
	r := &Runner{Tmux: h, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Give the server a fresh event before each poll, including after the
	// one-second liveness cache expires. It may consider the pane dead.
	var stopping atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	bad := make(chan error, 1)
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			l := r.polled(dir)
			if !stopping.Load() && l.State != holder.Running {
				r.endHeld(context.Background(), dir, w)
				bad <- fmt.Errorf("poll lost closed-terminal agent: %+v", l)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case err := <-bad:
		t.Fatal(err)
	case <-time.After(2*heldPoll + 100*time.Millisecond):
	}
	if _, ok := heldOf(dir); !ok {
		t.Fatal("poll removed the owned window reference")
	}
	stopping.Store(true)
	res, err := r.stop(dir, 0, 0)
	cancel()
	<-done
	if err != nil || res.Already || res.Signals == "" {
		t.Fatalf("Stop: %+v, %v", res, err)
	}
	if _, ok := heldOf(dir); ok {
		t.Fatal("Stop retained the ended window")
	}
	if err := syscall.Kill(pid, 0); err == nil {
		out, _ := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
		if stat := strings.TrimSpace(string(out)); stat != "" && !strings.HasPrefix(stat, "Z") {
			t.Fatalf("agent still runs: %s", stat)
		}
	}
}
