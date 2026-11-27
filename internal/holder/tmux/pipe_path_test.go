package tmux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/holder"
	"github.com/yousan/hataori/internal/host"
)

// pipe-pane's time/format expansion is tested directly even on versions
// older than the holder's Start minimum; the exact production command is
// taken from setUp, with no substitute pipe command in the test.
func TestPipeCommandPreservesLiteralPaths(t *testing.T) {
	s := newServer(t)
	dir, bin := literalPipePaths(t, s)
	h := &Holder{Hataori: bin}
	pane, _ := s.pane("exec cat")
	if err := host.ResetPipe(dir); err != nil {
		t.Fatal(err)
	}
	keep, _ := h.setUp(ref{Pane: pane, Dir: dir})
	_, pipe, ok := strings.Cut(keep, "pipe-pane -O -t "+pane+" ")
	if !ok {
		t.Fatal("setup did not include a pipe command")
	}
	s.run("if-shell", "-F", "1", "pipe-pane -O -t "+pane+" "+pipe)
	s.wait("literal-path pipe did not start", func() bool { return host.PipeRuns(dir) })
	s.run("send-keys", "-t", pane, "-l", "literal pipe output")
	s.run("send-keys", "-t", pane, "Enter")
	s.wait("output not recorded at literal path", func() bool { return strings.Contains(string(host.KeptTerminal(dir)), "literal pipe output") })
}

func TestStartRecordsOutputAtLiteralPipePaths(t *testing.T) {
	s := newServer(t)
	dir, bin := literalPipePaths(t, s)
	h := s.starter()
	h.Hataori = bin
	w, err := h.Start(t.Context(), dir, holder.Spec{Terminal: true, Argv: []string{"/bin/sh", "-c", "printf 'literal start output\\n'; exec sleep 600"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Stop(context.Background(), w, 0, 0) })
	s.wait("first output not recorded at literal path", func() bool { return strings.Contains(string(host.KeptTerminal(dir)), "literal start output") })
}

func literalPipePaths(t *testing.T, s *server) (string, string) {
	t.Helper()
	root := filepath.Join(s.dir, "root-#{pid}-#-%Y-#[red]-it's")
	dir := filepath.Join(root, "7")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "binary-#{pid}-#-%Y-#[blue]")
	if err := os.Symlink(self, bin); err != nil {
		t.Fatal(err)
	}
	return dir, bin
}
