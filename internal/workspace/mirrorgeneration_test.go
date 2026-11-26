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
	"time"

	"golang.org/x/sys/unix"
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
	for _, lockName := range []string{"packed-refs.lock", "refs/heads/gone.lock", "refs/heads/develop.lock", "config.lock"} {
		t.Run(lockName, func(t *testing.T) {
			x := newMirrorFixture(t)
			gone := x.push("main", "refs/heads/gone")
			x.make()
			x.make() // establish the mirror's guards
			old := x.mirror
			x.push("main", "refs/heads/develop")
			x.git(x.bare, "symbolic-ref", "HEAD", "refs/heads/develop")
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
			if got := x.git(current, "symbolic-ref", "HEAD"); got != "refs/heads/develop" {
				t.Fatal("recovered mirror did not follow upstream HEAD:", got)
			}
			if got := x.git(old, "symbolic-ref", "HEAD"); got != "refs/heads/main" {
				t.Fatal("recovery changed old HEAD:", got)
			}
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

func TestMirrorRejectsInvalidGenerationPointers(t *testing.T) {
	for i, value := range []string{"../elsewhere\n", "foreign-generation-123.git\n", "missing", "", "too large", "symlink generation"} {
		t.Run(fmt.Sprint("case-", i), func(t *testing.T) {
			x := newMirrorFixture(t)
			x.make()
			key := MirrorKey(x.bare)
			if value == "missing" {
				value = key + "-generation-123.git\n"
			}
			if value == "too large" {
				value = strings.Repeat("x", 257)
			}
			if value == "symlink generation" {
				value = key + "-generation-123.git\n"
				if err := os.Symlink(x.bare, filepath.Join(x.root, "mirrors", strings.TrimSpace(value))); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(x.root, "state", "mirrors-tmp", key+".current")
			if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := selectedMirror(t, x); got != "file://"+x.bare {
				t.Fatalf("invalid pointer selected %q", got)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != value {
				t.Fatalf("invalid pointer was changed: %q %v", got, err)
			}
		})
	}
}

func TestMirrorRejectsSpecialGenerationPointers(t *testing.T) {
	for _, kind := range []string{"fifo", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			x := newMirrorFixture(t)
			x.make()
			pointer := filepath.Join(x.root, "state", "mirrors-tmp", MirrorKey(x.bare)+".current")
			var err error
			switch kind {
			case "fifo":
				err = unix.Mkfifo(pointer, 0o600)
			case "symlink":
				err = os.Symlink(filepath.Join(x.mirror, "config"), pointer)
			case "directory":
				err = os.Mkdir(pointer, 0o700)
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := selectedMirrorPath(filepath.Dir(pointer), filepath.Join(x.root, "mirrors"), MirrorKey(x.bare))
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("special pointer accepted")
				}
			case <-time.After(2 * time.Second):
				// Release only this test's FIFO reader if the fix is reverted.
				if kind == "fifo" {
					f, err := os.OpenFile(pointer, os.O_WRONLY|unix.O_NONBLOCK, 0)
					if err == nil {
						f.Close()
					}
				}
				t.Fatal("special pointer blocked selection")
			}
		})
	}
}

// Publication can fail after a whole generation was placed. Such an orphan
// stays, and another attempt neither adopts nor cleans it up.
func TestMirrorRetainsAnUnselectedCompletedGeneration(t *testing.T) {
	x := newMirrorFixture(t)
	x.make()
	state := filepath.Join(x.root, "state", "mirrors-tmp")
	mirrors := filepath.Join(x.root, "mirrors")
	key := MirrorKey(x.bare)
	pointer := filepath.Join(state, key+".current")
	if err := os.Mkdir(pointer, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(state, key+".lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := x.m.recoverMirror(context.Background(), "file://"+x.bare, state, mirrors, key, lock); err == nil {
		t.Fatal("publication over a directory succeeded")
	}
	paths, err := filepath.Glob(filepath.Join(mirrors, key+"-generation-*.git"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("completed generation not retained: %v %v", paths, err)
	}
	orphan := paths[0]
	if got := x.git(orphan, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Fatal("orphan is not complete:", got)
	}
	if err := os.Remove(pointer); err != nil { // this test's empty obstruction
		t.Fatal(err)
	}
	if got := selectedMirror(t, x); got != x.mirror {
		t.Fatal("unselected orphan was adopted:", got)
	}
	current, err := x.m.recoverMirror(context.Background(), "file://"+x.bare, state, mirrors, key, lock)
	if err != nil || current == orphan {
		t.Fatalf("next publication: %q %v", current, err)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatal("completed orphan removed:", err)
	}
	if got := selectedMirror(t, x); got != current {
		t.Fatalf("published pointer selected %q, want %q", got, current)
	}
}

func TestMirrorRecoversSuccessiveGenerationsAndAFailedAttempt(t *testing.T) {
	x := newMirrorFixture(t)
	x.make()
	old := x.mirror
	lock := filepath.Join(old, "config.lock")
	if err := os.WriteFile(lock, []byte("old lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := selectedMirror(t, x)
	if first == old {
		t.Fatal("first recovery did not select a new generation")
	}
	locked := filepath.Join(first, "HEAD.lock")
	if err := os.WriteFile(locked, []byte("new lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The remote is this test's repository. An unavailable upstream must
	// leave the current generation and its lock usable and selected.
	offline := x.bare + ".offline"
	if err := os.Rename(x.bare, offline); err != nil {
		t.Fatal(err)
	}
	if got := selectedMirror(t, x); got != first {
		t.Fatal("failed recovery changed the selected generation:", got)
	}
	if err := os.Rename(offline, x.bare); err != nil {
		t.Fatal(err)
	}
	second := selectedMirror(t, x)
	if second == first || second == old || second == "file://"+x.bare {
		t.Fatalf("second recovery selected %q", second)
	}
	for path, want := range map[string]string{lock: "old lock", locked: "new lock"} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("retained lock %q: %q %v", path, got, err)
		}
	}
	if got := selectedMirror(t, x); got != second {
		t.Fatal("second generation was not reused:", got)
	}
}
