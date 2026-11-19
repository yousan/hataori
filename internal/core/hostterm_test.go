package core

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
)

type fakeHostTerms struct{ owned [][]int }

func (h *fakeHostTerms) List(_ context.Context, owned []int) ([]control.HostTerminal, error) {
	h.owned = append(h.owned, owned)
	return []control.HostTerminal{{Device: "/dev/pts/1"}}, nil
}

func (h *fakeHostTerms) Screen(_ context.Context, pane string, owned []int) (string, bool, error) {
	h.owned = append(h.owned, owned)
	return "screen", pane == "%1", nil
}

func hostOps(ctx context.Context) map[string]func(control.API) error {
	return map[string]func(control.API) error{
		"HostTerminals": func(a control.API) error { _, err := a.HostTerminals(ctx); return err },
		"HostScreen":    func(a control.API) error { _, err := a.HostScreen(ctx, "%1"); return err },
	}
}

// With the config's switch off, nobody gets an answer, the owner with
// OpHost included.
func TestHostTerminalsOff(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	a := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s"}, May: all | control.OpHost})
	for name, op := range hostOps(ctx) {
		if err := op(a); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s with the switch off: %v", name, err)
		}
	}
}

// With it on, only the owner's caller with OpHost gets an answer.
func TestHostTerminalsOwnerOnly(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	f.core.HostTerms = &fakeHostTerms{}
	refused := map[string]control.Caller{
		"another principal with OpHost": {Principal: "someone", Scopes: []string{"s"}, May: all | control.OpHost},
		"the owner without OpHost":      {Principal: "owner", Scopes: []string{"s"}, May: all},
		"no principal with OpHost":      {Scopes: []string{"s"}, May: all | control.OpHost},
	}
	for cname, c := range refused {
		for name, op := range hostOps(ctx) {
			if err := op(f.core.As(c)); !errors.Is(err, ErrForbidden) {
				t.Errorf("%s by %s: %v", name, cname, err)
			}
		}
	}
	owner := f.core.As(control.Caller{Principal: "owner", May: control.OpHost})
	for name, op := range hostOps(ctx) {
		if err := op(owner); err != nil {
			t.Errorf("%s by the owner: %v", name, err)
		}
	}
	if _, err := owner.HostScreen(ctx, "%2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("HostScreen of an unlisted pane: %v", err)
	}
}

// The session hosts that may hold a terminal are left out of the list.
func TestHostTerminalsLeaveOutSessionHosts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{})
	h := &fakeHostTerms{}
	f.core.HostTerms = h
	_, live := f.running(t, "t1")
	_, asleep := f.running(t, "t2")
	db := f.core.Store.DB
	db.Exec(`UPDATE sessions SET host_pid = 4321 WHERE id = ?`, live)
	db.Exec(`UPDATE sessions SET host_pid = 4322, state = ? WHERE id = ?`, string(control.Asleep), asleep)
	if _, err := f.core.As(control.Caller{Principal: "owner", May: control.OpHost}).HostTerminals(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.owned[0]; !slices.Equal(got, []int{4321}) {
		t.Errorf("owned pids %v, want [4321]", got)
	}
}

// A tmux server on a socket of its own: its pane is listed with session,
// window, pane, device, command and directory, and HostScreen reads a line
// printed in it.
func TestHostScreenReadsATmuxPane(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("tmux is not installed; CI must run this test")
		}
		t.Skip("no tmux")
	}
	t.Setenv("TMUX", "")
	ctx := context.Background()
	sock := []string{"-L", fmt.Sprintf("hataori-core-test-%d", os.Getpid()), "-f", "/dev/null"}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tmux := func(args ...string) error {
		out, err := exec.Command("tmux", slices.Concat(sock, args)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("tmux %v: %v: %s", args, err, out)
		}
		return nil
	}
	if err := tmux("new-session", "-d", "-s", "hataori-test", "-x", "80", "-y", "24", "-c", dir,
		"sh -c 'echo hataori-screen-line; exec sleep 600'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tmux("kill-server") })

	f := newFixture(t, fakeOwner{})
	f.core.HostTerms = &hostterm.Lister{Tmux: sock}
	a := f.core.As(control.Caller{Principal: "owner", May: control.OpHost})
	var pane *control.HostTerminal
	deadline := time.Now().Add(10 * time.Second)
	for {
		list, err := a.HostTerminals(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i, ht := range list {
			if ht.Tmux != nil && ht.Tmux.Session == "hataori-test" {
				pane = &list[i]
			}
		}
		if pane != nil && pane.Process == "sleep" || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if pane == nil {
		t.Fatal("the test's tmux pane is not listed")
	}
	p := pane.Tmux
	if p.Window != "0" || p.Index != "0" || !strings.HasPrefix(p.Pane, "%") || !strings.HasPrefix(pane.Device, "/dev/") ||
		pane.Process != "sleep" || p.Dir != dir || pane.Activity == nil {
		t.Errorf("pane %+v, tmux %+v", pane, p)
	}
	for {
		screen, err := a.HostScreen(ctx, p.Pane)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(screen, "hataori-screen-line") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the screen does not show the printed line:\n%s", screen)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
