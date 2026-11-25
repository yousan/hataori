package tmux

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/holder"
)

// captured is what a tmux of an older version printed.
func captured(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// script is a program of the test's, in place of tmux.
func (s *server) script(body string) string {
	s.t.Helper()
	p := filepath.Join(s.dir, "old-tmux")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		s.t.Fatal(err)
	}
	return p
}

// A tmux that does not know paste-buffer's -S says so in its version's words;
// it is asked again without it, and from then on only without it.
func TestPasteToATmuxWithoutTheRawFlag(t *testing.T) {
	for _, v := range []string{"3.2a", "3.3a"} {
		t.Run(v, func(t *testing.T) {
			s := newServer(t)
			w, out := s.reader()
			refusals := filepath.Join(s.dir, "refusals")
			h := &Holder{Tmux: s.script(`case "$*" in *" -S ; "*) echo x >> ` + refusals + `; printf '%s\n' ` +
				quote(captured(t, "paste-buffer-S-"+v+".txt")) + ` >&2; exit 1;; esac
exec ` + s.tmux + ` "$@"`)}
			for range 2 {
				if err := h.Input(t.Context(), w, []agent.Part{{Paste: "a paste"}, {Key: agent.Enter}}); err != nil {
					t.Fatal(err)
				}
			}
			want := strings.Repeat("\x1b[200~a paste\x1b[201~\r", 2)
			if got := waitBytes(t, out, len(want)); got != want {
				t.Errorf("the pane read %q, want %q", got, want)
			}
			if b, _ := os.ReadFile(refusals); string(b) != "x\n" {
				t.Errorf("-S was refused %d times, want once", len(b)/2)
			}
		})
	}
}

// A server older than the oldest whose windows are held: a start and a
// take-in are refused, naming its version, and the start leaves nothing.
func TestAnOldServerIsRefused(t *testing.T) {
	for v, refused := range map[string]bool{"3.0a": true, "3.1c": true, "3.2": false, "3.2a": false} {
		t.Run(v, func(t *testing.T) {
			s := newServer(t)
			// This tmux, as of version v.
			old := s.script(`for a; do
	shift
	set -- "$@" "$(printf '%s' "$a" | sed 's/#{version}/` + captured(t, "version-"+v+".txt") + `/g')"
done
exec ` + s.tmux + ` "$@"`)
			w, _ := s.reader()
			check := func(what string, err error) {
				t.Helper()
				if !refused && err != nil || refused && (!errors.Is(err, ErrTooOld) || !strings.HasSuffix(err.Error(), v)) {
					t.Errorf("%s: %v", what, err)
				}
			}
			check("a take-in", (&Holder{Tmux: old}).CheckTakeIn(t.Context(), w))

			before := s.panes()
			dir := filepath.Join(s.dir, "7")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			h := &Holder{Tmux: old, Hataori: "/bin/true", Prefix: "hataori-t-", Server: []string{"-S", s.sock}}
			_, err := h.Start(t.Context(), dir, holder.Spec{Terminal: true, Argv: []string{"sleep", "600"}, Dir: s.dir})
			check("a start", err)
			if after := s.panes(); refused && len(after) != len(before) {
				t.Errorf("the panes after a refused start: %v, before it %v", after, before)
			}
		})
	}
}

func TestCheckVersion(t *testing.T) {
	for v, ok := range map[string]bool{"": false, "2.9a": false, "3.1c": false, "3.2": true, "3.2a": true, "3.8": true, "3.10": true,
		"4.0": true, "next-3.1": false, "next-3.9": true, "3.2-rc": true, "master": true, "openbsd-7.4": true} {
		if err := checkVersion(v); (err == nil) != ok {
			t.Errorf("%q: %v", v, err)
		}
	}
}
