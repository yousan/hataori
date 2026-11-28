package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/holder"
)

func TestStartKeepsAnAgentThatExitsBetweenItsTwoLooks(t *testing.T) {
	s := newServer(t)
	v, err := exec.Command(s.tmux, "-V").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(v), "tmux 3.8") {
		t.Skip("this regression needs tmux 3.8's cleared dead-pane PID")
	}
	h := s.starter()
	dir := filepath.Join(s.dir, "7")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(s.dir, "exit")
	on := quote(s.tmux) + " -S " + quote(s.sock)
	// Only the ownership read follows the first liveness read. Release the
	// real agent there and let tmux read its end before returning that read.
	h.Tmux = s.script(`case "$*" in *" list-panes -t "*"#{pane_start_command}"*)
  if [ ! -e ` + quote(gate) + ` ]; then
    touch ` + quote(gate) + `
    i=0
    while [ "$(` + on + ` display-message -p -t =hataori-t-7: '#{pane_dead},#{pane_pid}')" != '1,' ]; do
      i=$((i+1)); [ "$i" -lt 200 ] || exit 1
      ` + on + ` run-shell -b true
      sleep 0.02
    done
  fi;;
esac
exec ` + quote(s.tmux) + ` "$@"`)
	w, err := h.Start(t.Context(), dir, holder.Spec{Terminal: true, Argv: []string{"/bin/sh", "-c", "until [ -e " + quote(gate) + " ]; do sleep 0.02; done; exit 3"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Stop(context.Background(), w, 0, 0) })
	if _, err := os.Stat(gate); err != nil {
		t.Fatal("agent never exited between reads")
	}
	l, err := h.Alive(t.Context(), w)
	if err != nil || l.State != holder.Exited || l.Status == nil || l.Status.Code != 3 {
		t.Fatalf("ended agent: %+v, %v", l, err)
	}
}
