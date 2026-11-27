package tmux

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// Setup hooks can hand the pane to a person before either sizing command.
func TestStartLeavesAHooksReplacementSizeAlone(t *testing.T) {
	for _, hook := range []string{"after-pipe-pane", "after-resize-window"} {
		t.Run(hook, func(t *testing.T) {
			s := newServer(t)
			s.run("new-session", "-d", "-s", "person", "exec sleep 600")
			base := quote(s.tmux) + " -S " + quote(s.sock)
			body := base + " set-hook -g -u " + hook + "; " + base + " respawn-pane -k -t =hataori-t-7: 'exec sleep 611'; " + base + " resize-window -t =hataori-t-7: -x 91 -y 29"
			s.run("set-hook", "-g", hook, "run-shell "+quote(body))
			h := s.starter()
			dir := filepath.Join(s.dir, "7")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			spec := sleeper
			spec.Cols, spec.Rows = 103, 37
			if _, err := h.Start(t.Context(), dir, spec); !errors.Is(err, errGone) {
				t.Fatalf("Start after replacement: %v", err)
			}
			if got := s.run("display-message", "-p", "-t", "=hataori-t-7:", "#{pane_width}x#{pane_height} #{window-size}"); got != "91x29 manual" {
				t.Errorf("replacement's size and sizing policy changed: %q", got)
			}
		})
	}
}

// The first program sees the requested dimensions, including on 3.2a;
// attaching and resizing a real client still controls the window afterwards.
func TestStartUsesTheSpecSizeAndThenTheClientSize(t *testing.T) {
	s := newServer(t)
	h := s.starter()
	dir := filepath.Join(s.dir, "7")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(s.dir, "first-size")
	spec := sleeper
	spec.Cols, spec.Rows = 103, 37
	spec.Argv = []string{"/bin/sh", "-c", "stty size > " + quote(first) + "; exec sleep 600"}
	w, err := h.Start(t.Context(), dir, spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(waitFile(t, first))); got != "37 103" {
		t.Errorf("size when the agent started: %q, want 37 103", got)
	}
	r, err := parseRef(w)
	if err != nil {
		t.Fatal(err)
	}
	// Exclude the status line so client rows equal pane rows.
	s.run("set-option", "-t", "="+h.Prefix+"7", "status", "off")
	cmd := exec.Command(s.tmux, "-S", s.sock, "attach-session", "-t", "="+h.Prefix+"7")
	cmd.Env = append(env(), "TERM=xterm-256color")
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 121, Rows: 43})
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	go func() { io.Copy(io.Discard, f); close(drained) }()
	t.Cleanup(func() {
		cmd.Process.Kill() // only the client this test started
		cmd.Wait()
		f.Close()
		<-drained
	})
	waitSize := func(cols, rows int) {
		t.Helper()
		want := fmt.Sprintf("%dx%d", cols, rows)
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			got := s.run("display-message", "-p", "-t", r.Pane, "#{pane_width}x#{pane_height}")
			if got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("pane size after client resize: %s, want %s", got, want)
			}
		}
	}
	waitSize(121, 43)
	if err := pty.Setsize(f, &pty.Winsize{Cols: 97, Rows: 31}); err != nil {
		t.Fatal(err)
	}
	waitSize(97, 31)
}
