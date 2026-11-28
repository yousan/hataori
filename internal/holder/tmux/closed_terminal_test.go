package tmux

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/yousan/hataori/internal/holder"
)

func TestAClosedTerminalDoesNotMeanTheAgentExited(t *testing.T) {
	s := newServer(t)
	h := &Holder{Prefix: "hataori-t-"}
	w, pane, dir := s.started(h, strings.Replace(closes, "kept", filepath.Join(s.dir, "kept"), 1))
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
	if s.seesClose() {
		s.wait("the terminal did not close", func() bool { return s.closed(pane) })
	}
	if !s.child(w, w.Process) {
		t.Fatal("agent ended before liveness check")
	}
	l, err := h.Alive(t.Context(), w)
	if err != nil || l.State != holder.Running {
		t.Fatalf("closed-terminal agent: %+v, %v", l, err)
	}
	res, err := h.Stop(context.Background(), w, 0, 0)
	if err != nil || res.Already || !groupGone(w.Process) {
		t.Fatalf("Stop lost the agent: %+v, %v", res, err)
	}
}
