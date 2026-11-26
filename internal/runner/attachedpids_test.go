package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/proctest"
)

func TestAttachedPIDsReconnect(t *testing.T) {
	root := t.TempDir()
	dir := StateDir(root, 1)
	_, err := host.Start(host.Command{Path: os.Args[0], Args: []string{"role", "host"}}, dir, host.Spec{Terminal: true, Argv: []string{"cat"}, Env: []string{"PATH=" + os.Getenv("PATH")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		proctest.Kill(filepath.Join(dir, host.PIDFile))
		proctest.Kill(filepath.Join(dir, host.HostPIDFile))
	})
	a, err := host.Attach(host.SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// No live runner state: the original daemon could have exited.
	r := &Runner{Root: root}
	got, err := r.AttachedPIDs(context.Background(), 1)
	if err != nil || !slices.Equal(got, []int{os.Getpid()}) {
		t.Fatalf("reconnected: %v, %v", got, err)
	}
}

func TestAttachedPIDsUnknownLaunchRefused(t *testing.T) {
	r := &Runner{Root: t.TempDir()}
	if _, err := r.AttachedPIDs(context.Background(), 1); err == nil {
		t.Fatal("missing launch accepted")
	}
	dir := StateDir(r.Root, 1)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	save := func(spec host.Spec) {
		b, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, host.SpecFile), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	save(host.Spec{Terminal: true})
	if _, err := r.AttachedPIDs(context.Background(), 1); err == nil {
		t.Fatal("unreachable terminal host accepted")
	}
	save(host.Spec{})
	if pids, err := r.AttachedPIDs(context.Background(), 1); err != nil || len(pids) != 0 {
		t.Fatalf("pipes: %v %v", pids, err)
	}
}
