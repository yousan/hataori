package workspace

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
)

func mirrorGit(t *testing.T, m Maker, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, m.gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestMirrorKey(t *testing.T) {
	want := MirrorKey("git@github.com:o/r.git")
	for _, repo := range []string{"ssh://git@GitHub.com:22/o/r", "https://github.com:443/o/r.git", "https://user:secret@github.com/o/r/", "http://github.com:80/o/r", "git://github.com:9418/o/r"} {
		if got := MirrorKey(repo); got != want {
			t.Errorf("%s: %s != %s", repo, got, want)
		}
	}
	local := filepath.Join(t.TempDir(), "repo.git")
	if MirrorKey(local) != MirrorKey("file://"+local) {
		t.Fatal("local spellings differ")
	}
	if MirrorKey("ssh://host:2222/o/r") == MirrorKey("ssh://host/o/r") {
		t.Fatal("nondefault port lost")
	}
}

func TestMirrorCloneSurvivesDeletion(t *testing.T) {
	repo := bareRepo(t)
	m := Maker{Root: t.TempDir()}
	dir, err := m.Clone(context.Background(), "chat", "scope", "one", repo)
	if err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(m.Root, "mirrors", MirrorKey(repo)+".git")
	if info, err := os.Stat(mirror); err != nil || !info.IsDir() {
		t.Fatalf("mirror not created: %v", err)
	}
	mirrorGit(t, m, mirror, "fsck", "--full")
	if _, err := m.Clone(context.Background(), "chat", "scope", "two", repo+"/"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(m.Root, "mirrors"))
	if err != nil || len(entries) != 1 || entries[0].Name() != MirrorKey(repo)+".git" {
		t.Fatalf("mirror not reused: %v, %v", entries, err)
	}
	if got := mirrorGit(t, m, dir, "remote", "get-url", "origin"); got != repo {
		t.Fatalf("origin: %s", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatalf("alternates: %v", err)
	}
	if err := os.RemoveAll(mirror); err != nil {
		t.Fatal(err)
	}
	mirrorGit(t, m, dir, "fsck", "--full")
}

func TestConcurrentMirrors(t *testing.T) {
	repo := bareRepo(t)
	m := Maker{Root: t.TempDir()}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := m.Make(context.Background(), control.ThreadID{Scope: "scope", Key: fmt.Sprint(i)}, repo)
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	mirrorGit(t, m, filepath.Join(m.Root, "mirrors", MirrorKey(repo)+".git"), "fsck", "--full")
}

func TestMirrorTimeout(t *testing.T) {
	m := Maker{Root: t.TempDir(), FetchTimeout: 300 * time.Millisecond}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	repo := srv.URL + "/o/r.git"
	mirror := filepath.Join(m.Root, "mirrors", MirrorKey(repo)+".git")
	if err := os.MkdirAll(filepath.Dir(mirror), 0o700); err != nil {
		t.Fatal(err)
	}
	// Simulate a mirror built earlier using a different address for the repository.
	mirrorGit(t, m, m.Root, "clone", "--bare", "--", bareRepo(t), mirror)
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	pidfile := filepath.Join(bin, "fetch-pid")
	barePID := filepath.Join(bin, "bare-pid")
	script := "#!/bin/sh\ncase \" $* \" in *' fetch --no-tags -- '*) echo $$ > '" + pidfile + "';; *' clone --bare '*) echo $$ > '" + barePID + "';; esac\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir, err := m.Clone(ctx, "chat", "scope", "stale", repo)
	if err != nil {
		t.Fatal(err)
	}
	mirrorGit(t, m, dir, "fsck", "--full")
	if got := mirrorGit(t, m, dir, "remote", "get-url", "origin"); got != repo {
		t.Fatalf("origin: %s", got)
	}
	checkGroupGone := func(pidfile string) {
		t.Helper()
		b, err := os.ReadFile(pidfile)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Second)
		for {
			err = syscall.Kill(-pid, 0)
			if err == syscall.ESRCH {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("git process group %d remains: %v", pid, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	checkGroupGone(pidfile)
	// With no usable mirror the caller's deadline also bounds the direct clone.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel2()
	missing := srv.URL + "/missing.git"
	if _, err := m.Clone(ctx2, "chat", "scope", "missing", missing); err == nil || ctx2.Err() == nil {
		t.Fatalf("direct fallback: %v, context: %v", err, ctx2.Err())
	}
	if _, err := os.Lstat(filepath.Join(m.Root, "mirrors", MirrorKey(missing)+".git")); !os.IsNotExist(err) {
		t.Fatalf("failed mirror appeared: %v", err)
	}
	checkGroupGone(barePID)
}

func TestMirrorPublishedOnlyWhenComplete(t *testing.T) {
	repo := bareRepo(t)
	m := Maker{Root: t.TempDir()}
	mirror := filepath.Join(m.Root, "mirrors", MirrorKey(repo)+".git")
	state := filepath.Join(m.Root, "state", "mirrors-tmp")
	leftover := filepath.Join(state, "leftover")
	os.MkdirAll(leftover, 0o700)
	os.WriteFile(filepath.Join(leftover, "mine"), []byte("untouched"), 0o600)
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \" $* \" in *' clone --bare '*) [ ! -e '" + mirror + "' ] || exit 91\n '" + real + "' \"$@\" || exit $?\n [ ! -e '" + mirror + "' ] || exit 92\n exit 0;; esac\nexec '" + real + "' \"$@\"\n"
	os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := m.Clone(context.Background(), "chat", "scope", "one", repo); err != nil {
		t.Fatal(err)
	}
	mirrorGit(t, m, mirror, "fsck", "--full")
	if b, err := os.ReadFile(filepath.Join(leftover, "mine")); err != nil || string(b) != "untouched" {
		t.Fatalf("leftover changed: %q %v", b, err)
	}
	entries, err := os.ReadDir(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("temporary mirror left behind: %v", entries)
	}
}
