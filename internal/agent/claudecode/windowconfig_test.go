package claudecode

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/yousan/hataori/internal/window"
)

// The report names the configuration directory the window's session uses:
// CLAUDE_CONFIG_DIR, or the CLI's default under HOME when it is not set.
func TestWindowHookReportsTheConfigDirInUse(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"CLAUDE_CONFIG_DIR": "/home/alice/.claude-work", "HOME": "/home/alice"}, "/home/alice/.claude-work"},
		{map[string]string{"HOME": "/home/alice"}, "/home/alice/.claude"},
	} {
		env := map[string]string{"TMUX": "/tmp/tmux-1000/default,4242,0", "TMUX_PANE": "%7", "CLAUDE_CODE_ENTRYPOINT": "cli"}
		for k, v := range c.env {
			env[k] = v
		}
		var sent []window.Report
		h := windowHook{getenv: func(k string) string { return env[k] }, ppid: 4243,
			terminal: func(context.Context, int) (int, string, bool, error) { return 4242, "/dev/pts/9", true, nil },
			send:     func(_ context.Context, _ string, r window.Report) error { sent = append(sent, r); return nil }}
		h.main([]string{"--window", "--root", t.TempDir()}, bytes.NewReader(capturedPayload(t, "2.1.328/paste_tabs/hooks/01-SessionStart.json")), io.Discard)
		if len(sent) != 1 || sent[0].ConfigDir != c.want {
			t.Fatalf("%v: reported %+v, want the config dir %s", c.env, sent, c.want)
		}
	}
}
