package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/wire"
	"github.com/yousan/hataori/internal/workspace"
)

// lastText is the last text thread id said.
func lastText(t *testing.T, owner *wire.Client, id control.ThreadID) string {
	t.Helper()
	texts := threadEvents(t, owner, id, "text")
	if len(texts) == 0 {
		return ""
	}
	s, _ := texts[len(texts)-1]["text"].(string)
	return s
}

// With files set, the supervisor's launch finds each file's current
// content in docs/, and so does each wake: a file changed between two
// wakes is new at the second.
func TestSupervisorDocsAtEachLaunchAndWake(t *testing.T) {
	host := t.TempDir()
	design := filepath.Join(host, "design.md")
	os.WriteFile(design, []byte("v1\n"), 0o644)
	wt := newWakeTest(t, func(cfg map[string]any) {
		sv := cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)
		sv["instructions"] = "wake-script"
		sv["files"] = []string{design}
	})
	stop, owner, sv := wt.start(t)
	defer stop()
	if a := sendAndRead(t, owner, sv.ID, "r0", "read docs/design.md"); a != "read: v1" {
		t.Fatalf("after its launch the supervisor read %q", a)
	}
	for i, content := range []string{"v2", "v3, longer"} {
		os.WriteFile(design, []byte(content+"\n"), 0o644)
		key := []string{"w1", "w2"}[i]
		_, pos := wt.worker(t, owner, "c1", key, "reply read docs/design.md")
		wt.read(t, pos)
		wt.quiet(t)
		wt.woken(t, owner, sv.ID, i+1)
		if a := lastText(t, owner, sv.ID); a != "read: "+content {
			t.Fatalf("at wake %d the supervisor read %q; want %q", i+1, a, "read: "+content)
		}
	}
	st, err := os.Stat(filepath.Join(workspace.ScopePath(wt.r.root, "c1"), supervisor.DocsDir, "design.md"))
	if err != nil || st.Mode().Perm()&0o222 != 0 {
		t.Fatalf("the copy: %v, %v; want it not writable", st, err)
	}
}

// A listed path that is missing, a directory or a file larger than 1 MB is
// skipped with one log line each, and the supervisor's launch goes on.
// Without files, its directory gets no docs/.
func TestSupervisorDocsSkipWhatIsNoFile(t *testing.T) {
	host := t.TempDir()
	missing, dir, big := filepath.Join(host, "missing.md"), filepath.Join(host, "adir"), filepath.Join(host, "big.md")
	os.Mkdir(dir, 0o700)
	os.WriteFile(big, []byte(strings.Repeat("x", supervisor.DocMax+1)), 0o644)
	r, _ := svRoot(t, true, func(cfg map[string]any) {
		cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)["files"] = []string{missing, dir, big}
	})
	stop, logged := runDaemonLog(t, r.root)
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	supervisorThread(t, owner, "c1", control.Idle)
	owner.Close()
	stop()
	for _, l := range []struct{ path, why string }{{missing, "missing"}, {dir, "not a regular file"}, {big, "larger than 1 MB"}} {
		n := 0
		for _, line := range strings.Split(logged(), "\n") {
			if strings.Contains(line, "supervisor docs") && strings.Contains(line, l.path) && strings.Contains(line, l.why) {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%d log lines for %s, want one saying %q:\n%s", n, l.path, l.why, logged())
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(workspace.ScopePath(r.root, "c1"), supervisor.DocsDir)); len(entries) != 0 {
		t.Fatalf("docs/ holds %v", entries)
	}

	// Without the key: no docs/ at all.
	plain, _ := svRoot(t, true)
	stop = runDaemon(t, plain.root)
	defer stop()
	owner, err = wire.Dial(plain.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	supervisorThread(t, owner, "c1", control.Idle)
	if _, err := os.Lstat(filepath.Join(workspace.ScopePath(plain.root, "c1"), supervisor.DocsDir)); !os.IsNotExist(err) {
		t.Fatalf("docs/ without files: %v", err)
	}
}
