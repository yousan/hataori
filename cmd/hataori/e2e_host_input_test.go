package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/hostterm"
	"github.com/yousan/hataori/internal/wire"
)

// The daemon with host_input on, against a tmux server of the test's own:
// at its start it deletes the tmux buffers an earlier run of the same
// instance left, and no other buffer; the owner's connection types into a
// pane; the start and the end are logged, and what was typed is not.
// With host_input off the same call is refused and logged.
func TestDaemonHostInput(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("no tmux")
		}
		t.Skip("no tmux")
	}
	tmpdir, err := os.MkdirTemp("/tmp", "hd") // short on macOS too: a socket path is limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpdir) })
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", tmpdir) // the server's socket is in a directory of the test's own
	sock := []string{"-L", fmt.Sprintf("hataori-daemon-input-test-%d", os.Getpid()), "-f", "/dev/null"}
	tmux := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("tmux", slices.Concat(sock, args)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	const marker = "hataori-daemon-typed-marker-0b7e"
	dir := t.TempDir()
	got, ready := filepath.Join(dir, "got"), filepath.Join(dir, "ready")
	// Typed only once the pane's terminal is in raw mode: before stty,
	// Enter's \r would be read as \n (#514).
	tmux("new-session", "-d", "-s", "work", "sh", "-c", fmt.Sprintf("stty raw -echo; : > %s; head -c %d > %s; exec sleep 600", ready, len(marker)+1, got))
	t.Cleanup(func() { exec.Command("tmux", slices.Concat(sock, []string{"kill-server"})...).Run() })
	eventually(t, "the pane's terminal in raw mode", 10*time.Second, func() bool { _, err := os.Stat(ready); return err == nil })
	pane := tmux("display-message", "-p", "-t", "work:0", "#{pane_id}")

	hostTermsHook = func(l *hostterm.Lister) { l.Tmux = sock }
	t.Cleanup(func() { hostTermsHook = nil })
	root := newRoot(t)
	abs, _ := filepath.Abs(root)
	leftover := "hataori-input-" + instanceTag(abs) + "-0123456789abcdef"
	others := []string{"hataori-input-" + instanceTag(abs+"x") + "-0123456789abcdef", "mine"}
	for _, b := range append([]string{leftover}, others...) {
		tmux("set-buffer", "-b", b, marker)
	}
	setConfig(t, root, func(cfg map[string]any) {
		cfg["host_terminals"], cfg["host_input"], cfg["host_input_idle"] = true, true, "30s"
	})
	stop, logged := runDaemonLog(t, root)
	buffers := strings.Fields(tmux("list-buffers", "-F", "#{buffer_name}"))
	slices.Sort(buffers)
	if want := slices.Sorted(slices.Values(others)); !slices.Equal(buffers, want) {
		t.Errorf("buffers after the start %q, want %q", buffers, want)
	}
	if !strings.Contains(logged(), "deleted tmux buffers an earlier run left") {
		t.Errorf("the deletion is not logged:\n%s", logged())
	}
	tmux("delete-buffer", "-b", "mine")
	tmux("delete-buffer", "-b", others[0])

	c, err := wire.Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	op, err := c.OperateHost(ctx, pane)
	if err != nil {
		t.Fatal(err)
	}
	if op.Idle() != 30*time.Second {
		t.Errorf("idle %v", op.Idle())
	}
	if err := op.Send(ctx, []control.HostKey{{Text: marker}, {Key: "Enter"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the pane to read the text", 10*time.Second, func() bool {
		b, _ := os.ReadFile(got)
		return string(b) == marker+"\r"
	})
	op.End()
	<-op.Done()
	if op.Reason() != control.HostEnded {
		t.Errorf("reason %q", op.Reason())
	}
	if out, _ := exec.Command("tmux", slices.Concat(sock, []string{"list-buffers"})...).CombinedOutput(); strings.Contains(string(out), "hataori-input-") {
		t.Errorf("a buffer is left: %s", out)
	}
	stop()
	log := logged()
	if !strings.Contains(log, "host input started") || !strings.Contains(log, `host input ended`) || !strings.Contains(log, "reason=ended") {
		t.Errorf("start and end are not logged:\n%s", log)
	}
	if strings.Contains(log, marker) {
		t.Error("the log holds what was typed")
	}

	// host_input off: refused and logged.
	off := newRoot(t)
	setConfig(t, off, func(cfg map[string]any) { cfg["host_terminals"] = true })
	_, loggedOff := runDaemonLog(t, off)
	c2, err := wire.Dial(off)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if _, err := c2.OperateHost(ctx, pane); !errors.Is(err, control.ErrForbidden) {
		t.Errorf("with host_input off: %v", err)
	}
	if l := loggedOff(); !strings.Contains(l, "control call refused") || !strings.Contains(l, "op=OperateHost") {
		t.Errorf("the refusal is not logged:\n%s", l)
	}
}
