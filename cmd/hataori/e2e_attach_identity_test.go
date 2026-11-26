package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/wire"
)

// Run the actual attach command in a separate process under any link name.
func TestAttachIdentityChild(t *testing.T) {
	root := os.Getenv("HATAORI_ATTACH_IDENTITY_ROOT")
	if root == "" {
		t.Skip("only in the renamed attach process")
	}
	os.Exit(run([]string{"attach", "--root", root, os.Getenv("HATAORI_ATTACH_IDENTITY_KEY")}, os.Stdout, os.Stderr))
}

// CI only: a link of an unrelated name, outside the daemon binary's
// directory, remains protected after a daemon reconnects to its hosts.
func TestDaemonRefusesRenamedAttachPane(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal(err)
		}
		t.Skip("no tmux")
	}
	tmp, err := os.MkdirTemp("/tmp", "hi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", tmp)
	// The default socket lives in this test's own directory, and is inherited
	// by the daemon. Other tests' and people's tmux servers cannot be reached.
	tmux := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("tmux", append([]string{"-f", "/dev/null"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	p := terminalProcRoot(t, "1h")
	setConfig(t, p.root, func(cfg map[string]any) { cfg["host_terminals"], cfg["host_input"] = true, true })
	p.start(t)
	p.in(c1, "!go repo:"+bareRepo(t)+" screen identity-ready")
	th := p.thread(t, 0)
	eventually(t, "the first turn", 15*time.Second, func() bool { return len(p.events(t, "turn_ended", th)) == 1 })
	link := filepath.Join(t.TempDir(), "unrelated-client")
	if err := os.Symlink(os.Args[0], link); err != nil {
		t.Fatal(err)
	}
	tmux("new-session", "-d", "-s", "attach", fmt.Sprintf("exec env HATAORI_ATTACH_IDENTITY_ROOT='%s' HATAORI_ATTACH_IDENTITY_KEY='%s' '%s' -test.run '^TestAttachIdentityChild$'", p.root, numberOf(t, p.root, th), link))
	t.Cleanup(func() { exec.Command("tmux", "kill-server").Run() })
	pane := tmux("display-message", "-p", "-t", "attach:0", "#{pane_id}")
	var id int64
	if err := p.db(t).QueryRow(`SELECT id FROM sessions ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the linked attach", 10*time.Second, func() bool {
		c, err := host.Dial(runner.StateDir(p.root, id))
		if err != nil {
			return false
		}
		defer c.Close()
		n, _, err := c.Attached(context.Background())
		return err == nil && n == 1
	})
	for _, reconnect := range []bool{false, true} {
		if reconnect {
			p.restart(t)
		} // only this test's daemon process
		c, err := wire.Dial(p.root)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		op, err := c.OperateHost(ctx, pane)
		if op != nil {
			op.End()
			<-op.Done()
		}
		if !errors.Is(err, control.ErrForbidden) {
			t.Errorf("reconnected=%v: linked attach accepted: %v", reconnect, err)
		}
		terminals, listErr := c.HostTerminals(ctx)
		if listErr != nil {
			t.Error(listErr)
		}
		for _, term := range terminals {
			if term.Tmux != nil && term.Tmux.Pane == pane {
				t.Error("attached pane is listed")
			}
		}
		cancel()
		c.Close()
	}
}
