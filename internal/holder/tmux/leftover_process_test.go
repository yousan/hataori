package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/yousan/hataori/internal/holder"
)

func TestAStartEndsALeftoverWithAClosedTerminal(t *testing.T) {
	s := newServer(t)
	if !s.seesClose() {
		t.Skip("this tmux keeps a closed terminal alive")
	}
	h := s.starter()
	program := strings.Replace(closes, "kept", filepath.Join(s.dir, "kept"), 1)
	w, pane, dir := s.started(h, program)
	if err := os.WriteFile(filepath.Join(dir, PidFile), []byte(strconv.Itoa(w.Process)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s.child(w, w.Process) {
			syscall.Kill(-w.Process, syscall.SIGKILL)
		}
	})
	if err := os.WriteFile(filepath.Join(s.dir, "kept"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.wait("the terminal did not close", func() bool { return s.closed(pane) })
	if groupGone(w.Process) {
		t.Fatal("leftover ended before the next Start")
	}
	// The new agent records whether the old program still runs when it starts.
	out := filepath.Join(s.dir, "next")
	spec := sleeper
	spec.Argv = []string{"/bin/sh", "-c", "ps -p " + strconv.Itoa(w.Process) + " -o stat= | awk '$1 !~ /^Z/ {print \"overlap\"}' > " + quote(out) + "; echo started >> " + quote(out) + "; exec sleep 600"}
	next, err := h.Start(t.Context(), dir, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Stop(context.Background(), next, 0, 0) })
	b := waitFile(t, out)
	if strings.Contains(string(b), "overlap") || !groupGone(w.Process) {
		t.Fatalf("old agent overlapped the next Start: %s", b)
	}
}

// Simulate an unreaped leftover's process number now belonging to an
// unrelated group. Uncertainty must refuse Start before signalling or
// replacing its state files, and must remain a refusal on another attempt.
func TestAStartDoesNotSignalAnUnprovenLeftoverGroup(t *testing.T) {
	s := newServer(t)
	h := s.starter()
	w, pane, dir := s.started(h, "exec sleep 600")
	other := exec.Command("sleep", "600")
	other.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Process.Kill(); other.Wait() })
	s.renumber(h, &w, dir, other.Process.Pid)
	old := []byte("unchanged old execution specification")
	if err := os.WriteFile(filepath.Join(dir, ExecFile), old, 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := h.Start(t.Context(), dir, sleeper); err == nil {
			t.Error("Start accepted an unproven leftover group")
		}
		if syscall.Kill(other.Process.Pid, 0) != nil || zombie(t.Context(), other.Process.Pid) {
			t.Error("unrelated process was signalled")
		}
		if s.panes()[pane] == "" {
			t.Error("unproven leftover pane was removed")
		}
		if b, err := os.ReadFile(filepath.Join(dir, ExecFile)); err != nil || string(b) != string(old) {
			t.Errorf("old execution state overwritten: %q, %v", b, err)
		}
	}
}

// Startup can stop after remain-on-exit was set but before respawning the
// agent. A placeholder kept as a dead pane must not block the next Start.
func TestALeftoverPlaceholderKeptOnExitIsCleared(t *testing.T) {
	s := newServer(t)
	h := s.starter()
	_, _, dir := s.started(h, placeholder)
	w, err := h.Start(t.Context(), dir, sleeper)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Stop(context.Background(), w, 0, 0) })
	if l, err := h.Alive(t.Context(), w); err != nil || l.State != holder.Running {
		t.Fatalf("new agent: %+v, %v", l, err)
	}
}
