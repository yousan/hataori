package tmux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/holder"
)

// Replay the real truncated output captured from tmux 3.8 with a 150 ms
// pause in pane_pid. It must never prove a change of ownership or an end.
func TestCutFormatsLeaveOwnershipUnknown(t *testing.T) {
	for _, where := range []string{"pane", "start", "name", "end", "attached", "version", "missing"} {
		t.Run(where, func(t *testing.T) {
			s := newServer(t)
			h := s.starter()
			w, _, _ := s.started(h, "exec sleep 600")
			r, _ := parseRef(w)
			cut := capture(t, "list-panes-cut-3.8.txt")
			match := map[string]string{
				"pane": "*\" list-panes -a \"*", "start": "*pane_start_command*",
				"name": "*pane_current_command*", "end": "*pane_dead_time*",
				"attached": "*session_attached*", "version": "*\" display-message \"*",
				"missing": "*\" display-message \"*",
			}[where]
			prefix := ""
			if where == "missing" {
				// Omit the target from an otherwise complete real listing, then cut
				// the independent display-message check too.
				s.pane("exec sleep 600")
				prefix = "case \"$*\" in *\" list-panes -a \"*) " + s.tmux + " \"$@\" | grep -v ' " + r.Pane + " '; exit;; esac\n"
			}
			h.Tmux = s.script(prefix + "case \"$*\" in " + match + ") exec cat " + quote(cut) + ";; esac\nexec " + s.tmux + " \"$@\"")
			var err error
			switch where {
			case "pane", "missing":
				_, err = h.find(t.Context(), r)
			case "start":
				_, err = h.findStarted(t.Context(), r, w.Process+1)
			case "name":
				w.Owned = false
				_, err = h.takes(t.Context(), r, w)
			case "end":
				_, err = h.endOf(t.Context(), r, w.Process)
			case "attached":
				_, err = h.Attached(t.Context(), w)
			case "version":
				err = h.CheckTakeIn(t.Context(), w)
			}
			if err == nil || errors.Is(err, errGone) || !strings.Contains(err.Error(), "incomplete") {
				t.Errorf("%s read: %v, want unknown incomplete format", where, err)
			}
			if got := s.run("display-message", "-p", "-t", r.Pane, "#{pane_dead}"); got != "0" {
				t.Errorf("pane changed: %q", got)
			}
		})
	}
}

// A false format condition, including one cut short, must not skip setup
// and still respawn the agent, or report its placeholder as a running agent.
func TestStartDoesNotRunAfterARefusedSetup(t *testing.T) {
	s := newServer(t)
	h := s.starter()
	dir := filepath.Join(s.dir, "7")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	once := filepath.Join(s.dir, "once")
	h.Tmux = s.script(`case "$*" in *" if-shell "*)
 if [ ! -e ` + quote(once) + ` ]; then
  touch ` + quote(once) + `
  # Replace only the first if-shell's condition, leaving its false branch
  # and any subsequent commands as tmux itself would execute them.
  for arg do
   if [ "$arg" = if-shell ]; then shift; break; fi
   shift
  done
  shift 4
  exec ` + s.tmux + ` -S ` + quote(s.sock) + ` if-shell -F -t %0 0 "$@"
 fi;; esac
exec ` + s.tmux + ` "$@"`)
	ran := filepath.Join(s.dir, "ran")
	w, err := h.Start(t.Context(), dir, holder.Spec{Terminal: true, Argv: []string{"/bin/sh", "-c", "echo ran > " + quote(ran)}})
	if err == nil || errors.Is(err, errGone) {
		t.Errorf("Start: %+v %v, want uncertain refusal", w, err)
	}
	if b, _ := os.ReadFile(ran); len(b) > 0 {
		t.Errorf("agent ran without setup: %q", b)
	}
}

// Every false input guard is safe to treat as a refusal, including a timed
// out condition: no paste or key is handed over, so a retry cannot duplicate.
func TestARefusedGuardHandsOverNoInput(t *testing.T) {
	s := newServer(t)
	w, out := s.reader()
	h := &Holder{}
	h.Tmux = s.script(fmt.Sprintf(`case "$*" in *" if-shell "*)
 for arg do
  if [ "$arg" = if-shell ];then shift;break;fi
  shift
 done
 shift 4
 exec %s -S %s if-shell -F -t %%0 0 "$@";; esac
exec %s "$@"`, s.tmux, quote(s.sock), s.tmux))
	sent, err := h.input(t.Context(), w, []agent.Part{{Paste: "must not arrive"}, {Key: agent.Enter}})
	if sent || !errors.Is(err, ErrTakesNoInput) {
		t.Errorf("Send: %v %v", sent, err)
	}
	if b, _ := os.ReadFile(out); len(b) > 0 {
		t.Errorf("refused input arrived: %q", b)
	}
}
