package workspace

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func selectedMirror(t *testing.T, x *mirrorFixture) string {
	t.Helper()
	dir, unlock := x.m.mirror(context.Background(), "file://"+x.bare)
	unlock()
	return dir
}

// Recovery must not guess whether a lock is stale. It selects a completed
// independent generation and leaves the old pathname and its work intact.
func TestMirrorRecoversWithoutRemovingGitLocks(t *testing.T) {
	for _, lockName := range []string{"packed-refs.lock", "refs/heads/gone.lock", "config.lock"} {
		t.Run(lockName, func(t *testing.T) {
			x := newMirrorFixture(t)
			gone := x.push("main", "refs/heads/gone")
			x.make()
			x.make() // establish the mirror's guards
			old := x.mirror
			x.git(old, "pack-refs", "--all")
			if lockName == "config.lock" {
				x.git(old, "config", "--unset-all", "transfer.hideRefs")
			}
			lock := filepath.Join(old, lockName)
			if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
				t.Fatal(err)
			}
			const contents = "a lock whose ownership is unknown\n"
			if err := os.WriteFile(lock, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(lock)
			if err != nil {
				t.Fatal(err)
			}
			x.deleteUpstream("refs/heads/gone")
			current := selectedMirror(t, x)
			if current == old || current == "file://"+x.bare {
				t.Fatalf("selected %q, want a fresh mirror generation", current)
			}
			after, err := os.Stat(lock)
			data, readErr := os.ReadFile(lock)
			if err != nil || readErr != nil || !os.SameFile(before, after) || string(data) != contents {
				t.Fatalf("old lock changed: %v, %v, %q", err, readErr, data)
			}
			if got := x.git(old, "rev-parse", "refs/heads/gone"); got != gone {
				t.Fatal("old-only work changed:", got)
			}
			if got := x.git(old, "cat-file", "-t", gone); got != "commit" {
				t.Fatal("old-only object lost:", got)
			}
			x.mirror = current
			x.wantSame()
			if next := selectedMirror(t, x); next != current {
				t.Fatalf("generation not reused: %s, want %s", next, current)
			}
			dir := x.make()
			if got := x.git(dir, "remote", "get-url", "origin"); got != "file://"+x.bare {
				t.Fatal("origin:", got)
			}
			if _, err := os.Lstat(filepath.Join(dir, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
				t.Fatal("workspace depends on mirror:", err)
			}
		})
	}
}

// A prepared update-ref transaction has a live owner even after it closed
// its lock's FD. Both killing it and letting it commit must remain safe.
func TestMirrorRecoveryLeavesAPreparedGitTransactionAlone(t *testing.T) {
	for _, killed := range []bool{true, false} {
		t.Run(fmt.Sprint("killed=", killed), func(t *testing.T) {
			x := newMirrorFixture(t)
			x.make()
			x.make()
			old := x.mirror
			id := x.git(old, "rev-parse", "main")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cmd := exec.CommandContext(ctx, "git", "--git-dir", old, "update-ref", "--stdin")
			cmd.Env = x.m.gitEnv()
			in, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill() // only this test's child
					cmd.Wait()
				}
			})
			if _, err := fmt.Fprintf(in, "start\ncreate refs/heads/manual %s\nprepare\n", id); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(out)
			for _, want := range []string{"start: ok\n", "prepare: ok\n"} {
				if got, err := reader.ReadString('\n'); err != nil || got != want {
					t.Fatalf("prepared transaction: %q %v, stderr %s", got, err, &stderr)
				}
			}
			lock := filepath.Join(old, "refs", "heads", "manual.lock")
			before, err := os.Stat(lock)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(lock)
			if err != nil {
				t.Fatal(err)
			}
			if killed {
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				cmd.Wait()
			}
			current := selectedMirror(t, x)
			if current == old || !strings.HasPrefix(current, filepath.Join(x.root, "mirrors")+string(os.PathSeparator)) {
				t.Fatalf("no generation recovery: %q", current)
			}
			after, err := os.Stat(lock)
			got, readErr := os.ReadFile(lock)
			if err != nil || readErr != nil || !os.SameFile(before, after) || !bytes.Equal(got, data) {
				t.Fatalf("transaction lock changed: %v %v", err, readErr)
			}
			if !killed {
				io.WriteString(in, "commit\n")
				in.Close()
				if got, err := reader.ReadString('\n'); err != nil || got != "commit: ok\n" {
					t.Fatalf("original live Git cannot commit: %q %v", got, err)
				}
				if err := cmd.Wait(); err != nil {
					t.Fatalf("live Git: %v %s", err, &stderr)
				}
				if got := x.git(old, "rev-parse", "refs/heads/manual"); got != id {
					t.Fatal("live Git committed to another path:", got)
				}
			}
		})
	}
}
