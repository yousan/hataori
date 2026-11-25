package supervisor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

// The threads of a filesFixture: of scope c1 the supervisor's own one and a
// conversation, and c2's own one.
const (
	filesOwn, filesConv, filesOther = 5, 8, 20
)

// filesFixture is the supervisors' directories of two scopes, and what a
// session of each of three threads calls.
type filesFixture struct {
	files            *ScopeFiles
	log              *lockedBuffer
	root             string // of the store
	dir, dir2        string // of c1 and c2
	outside          string // a directory that is neither's
	own, conv, other control.API
}

// lockedBuffer is a log that several sessions write.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// take returns what was logged since the last take.
func (l *lockedBuffer) take() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	defer l.b.Reset()
	return l.b.String()
}

func newFilesFixture(t *testing.T) *filesFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := openFilesFixture(t, root)
	for _, dir := range []string{f.dir, f.dir2, f.outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.sql(t, fmt.Sprintf(`INSERT INTO supervisor_dirs (scope, dir) VALUES ('c1', '%s'), ('c2', '%s')`, f.dir, f.dir2))
	for id, at := range map[int][3]string{filesOwn: {"c1", "supervisor", f.dir}, filesConv: {"c1", "conversation", f.dir}, filesOther: {"c2", "supervisor", f.dir2}} {
		f.sql(t, fmt.Sprintf(`INSERT INTO threads (id, scope, key, started_by, state, confined, kind, workspace, created_at, updated_at)
			VALUES (%d, '%s', 'k%d', 'p', 'running', 1, '%s', '%s', 0, 0)`, id, at[0], id, at[1], at[2]))
	}
	return f
}

// openFilesFixture is the fixture that a newFilesFixture made at root.
func openFilesFixture(t *testing.T, root string) *filesFixture {
	t.Helper()
	st, err := store.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &filesFixture{log: &lockedBuffer{}, root: root, dir: filepath.Join(root, "confined", "scope-c1"),
		dir2: filepath.Join(root, "confined", "scope-c2"), outside: filepath.Join(root, "outside")}
	f.files = &ScopeFiles{Store: st, Log: slog.New(slog.NewTextHandler(f.log, nil))}
	f.own, f.conv, f.other = f.files.API(nil, "c1", filesOwn), f.files.API(nil, "c1", filesConv), f.files.API(nil, "c2", filesOther)
	return f
}

func (f *filesFixture) sql(t *testing.T, q string) {
	t.Helper()
	if _, err := f.files.Store.DB.Exec(q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// put makes the file name in dir, with the directories on its way.
func put(t *testing.T, dir, name, text string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// holds is the text of the file name in dir.
func holds(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// tree is all that a write could change under dirs: what stands there, what
// it holds, and which file it is.
func tree(t *testing.T, dirs ...string) string {
	t.Helper()
	var b strings.Builder
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			st, err := os.Lstat(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, "%s %v %d", p, st.Mode(), st.Sys().(*syscall.Stat_t).Ino)
			switch {
			case st.Mode().IsRegular():
				text, err := os.ReadFile(p)
				if err != nil && !errors.Is(err, fs.ErrPermission) {
					return err
				}
				fmt.Fprintf(&b, " %d %v %x", st.Size(), st.ModTime(), sha256.Sum256(text))
			case st.Mode()&fs.ModeSymlink != 0:
				to, _ := os.Readlink(p)
				fmt.Fprintf(&b, " to %s", to)
			}
			b.WriteString("\n")
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

func (f *filesFixture) tree(t *testing.T) string {
	t.Helper()
	return tree(t, f.dir, f.dir2, f.outside)
}

// recorded is how many writes are recorded, done or not.
func (f *filesFixture) recorded(t *testing.T) (n int) {
	t.Helper()
	if err := f.files.Store.DB.QueryRow(`SELECT count(*) FROM supervisor_writes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ownFiles are the files under dir with one of the daemon's own names.
func ownFiles(t *testing.T, dir string) (own []string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), tmpPrefix) {
			own = append(own, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return own
}

func writeOf(path, text, version string) control.FilesRequest {
	return control.FilesRequest{Op: control.FilesWrite, Path: path, Text: &text, Version: version}
}

func editOf(path, passage, by string) control.FilesRequest {
	return control.FilesRequest{Op: control.FilesEdit, Path: path, Old: passage, New: &by}
}

func readOf(path string) control.FilesRequest {
	return control.FilesRequest{Op: control.FilesRead, Path: path}
}

// do is a call that must be answered.
func do(t *testing.T, api control.API, r control.FilesRequest) control.FilesAnswer {
	t.Helper()
	ans, err := api.Files(context.Background(), r)
	if err != nil {
		t.Fatalf("%s %s: %v", r.Op, r.Path, err)
	}
	return ans
}

// made is a write that must be made; it returns the new version.
func made(t *testing.T, api control.API, r control.FilesRequest) string {
	t.Helper()
	ans := do(t, api, r)
	if ans.Refused != "" || ans.File == nil || ans.File.Text != "" || ans.File.Path != r.Path {
		t.Fatalf("%s %s: answered %+v, %+v, want it written", r.Op, r.Path, ans, ans.File)
	}
	return ans.File.Version
}

// refused is a call that must be refused for why, with nothing changed or
// recorded, and with one log line that says so and quotes nothing of it.
func (f *filesFixture) refused(t *testing.T, api control.API, r control.FilesRequest, why string) {
	t.Helper()
	before, records := f.tree(t), f.recorded(t)
	f.log.take()
	ans, err := api.Files(context.Background(), r)
	if err == nil || !strings.HasSuffix(err.Error(), why) {
		t.Errorf("%s %.60q: %+v, %v, want it refused: %s", r.Op, r.Path, ans, err, why)
	}
	if after := f.tree(t); after != before {
		t.Fatalf("%s %.60q, refused, changed something:\n%s\nbefore:\n%s", r.Op, r.Path, after, before)
	}
	if n := f.recorded(t); n != records {
		t.Errorf("%s %.60q, refused: %d writes are recorded, were %d", r.Op, r.Path, n, records)
	}
	log := f.log.take()
	if strings.Count(log, "\n") != 1 || !strings.Contains(log, why) || !strings.Contains(log, "level=WARN") {
		t.Errorf("%s %.60q, refused, is logged as %q", r.Op, r.Path, log)
	}
}

// misfit is a write that does not fit the file there, for why: it answers
// with that file and changes and records nothing.
func (f *filesFixture) misfit(t *testing.T, api control.API, r control.FilesRequest, why, there string) {
	t.Helper()
	before, records := f.tree(t), f.recorded(t)
	ans := do(t, api, r)
	if ans.Refused != why || ans.File == nil || ans.File.Text != there || ans.File.Version != versionOf(there) || ans.File.Size != int64(len(there)) {
		t.Errorf("%s %s: answered %q with %+v, want %q with %q", r.Op, r.Path, ans.Refused, ans.File, why, there)
	}
	if after := f.tree(t); after != before {
		t.Fatalf("%s %s, which did not fit, changed something:\n%s\nbefore:\n%s", r.Op, r.Path, after, before)
	}
	if n := f.recorded(t); n != records {
		t.Errorf("%s %s, which did not fit: %d writes are recorded, were %d", r.Op, r.Path, n, records)
	}
}

// A file is made, with the directories on its way, read, listed, edited and
// replaced, and keeps its permissions.
func TestFilesWriteAndRead(t *testing.T) {
	f := newFilesFixture(t)
	if ans := do(t, f.own, control.FilesRequest{Op: control.FilesList}); len(ans.Files) != 0 {
		t.Fatalf("an empty directory lists %+v", ans.Files)
	}
	v := made(t, f.own, writeOf("notes/2026/plan.md", "one\ntwo\n", ""))
	if v != versionOf("one\ntwo\n") || holds(t, f.dir, "notes/2026/plan.md") != "one\ntwo\n" {
		t.Fatalf("version %s; the file holds %q", v, holds(t, f.dir, "notes/2026/plan.md"))
	}
	for path, perm := range map[string]fs.FileMode{"notes": 0o700, "notes/2026": 0o700, "notes/2026/plan.md": 0o600} {
		if st, err := os.Stat(filepath.Join(f.dir, path)); err != nil || st.Mode().Perm() != perm {
			t.Fatalf("%s: %v, %v, want %v", path, st, err, perm)
		}
	}
	if err := os.Chmod(filepath.Join(f.dir, "notes/2026/plan.md"), 0o640); err != nil {
		t.Fatal(err)
	}
	// The other threads of the scope have the same directory.
	ans := do(t, f.conv, readOf("notes/2026/plan.md"))
	if want := (control.ScopeFile{Path: "notes/2026/plan.md", Size: 8, Version: v, Text: "one\ntwo\n"}); ans.File == nil || *ans.File != want {
		t.Fatalf("read %+v, want %+v", ans.File, want)
	}
	v2 := made(t, f.conv, editOf("notes/2026/plan.md", "two", "2"))
	if v2 != versionOf("one\n2\n") || holds(t, f.dir, "notes/2026/plan.md") != "one\n2\n" {
		t.Fatalf("version %s; the file holds %q", v2, holds(t, f.dir, "notes/2026/plan.md"))
	}
	made(t, f.own, writeOf("notes/2026/plan.md", "", v2)) // an empty text is one
	if holds(t, f.dir, "notes/2026/plan.md") != "" {
		t.Fatalf("the file holds %q", holds(t, f.dir, "notes/2026/plan.md"))
	}
	if st, err := os.Stat(filepath.Join(f.dir, "notes/2026/plan.md")); err != nil || st.Mode().Perm() != 0o640 {
		t.Fatalf("replaced, its permissions are %v, %v", st, err)
	}
	// What is there already is not written again.
	before := f.tree(t)
	made(t, f.own, writeOf("notes/2026/plan.md", "", versionOf("")))
	if f.tree(t) != before {
		t.Fatal("a write of what is there changed something")
	}
	if n := f.recorded(t); n != 0 || len(ownFiles(t, f.root)) != 0 {
		t.Fatalf("%d writes are recorded; own files %v", n, ownFiles(t, f.root))
	}
	if log := f.log.take(); strings.Count(log, "a supervisor's file is written") != 3 {
		t.Fatalf("logged %s", log)
	}
}

// A file that is written keeps its group too. One whose owner or group the
// daemon cannot keep, or which runs as another, is not written, and nothing
// is left of the try.
func TestFilesKeepsAccess(t *testing.T) {
	f := newFilesFixture(t)
	put(t, f.dir, "plan.md", "old")
	group := func() int {
		t.Helper()
		st, err := os.Stat(filepath.Join(f.dir, "plan.md"))
		if err != nil || st.Mode().Perm() != 0o640 {
			t.Fatalf("%v, %v", st, err)
		}
		return int(st.Sys().(*syscall.Stat_t).Gid)
	}
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, gid := range groups {
		if gid == os.Getegid() {
			continue
		}
		if err := os.Chown(filepath.Join(f.dir, "plan.md"), -1, gid); err != nil {
			continue
		}
		if err := os.Chmod(filepath.Join(f.dir, "plan.md"), 0o640); err != nil {
			t.Fatal(err)
		}
		made(t, f.own, writeOf("plan.md", "new", versionOf("old")))
		if group() != gid {
			t.Fatalf("written, the file has group %d, had %d", group(), gid)
		}
		made(t, f.own, editOf("plan.md", "new", "newer"))
		if group() != gid {
			t.Fatalf("edited, the file has group %d, had %d", group(), gid)
		}
		kept = true
		break
	}
	if !kept {
		t.Log("in no group but one")
	}

	const why = "a file whose owner, group, permissions or access list the daemon cannot keep"
	for name, bit := range map[string]fs.FileMode{"set-user-ID": fs.ModeSetuid, "set-group-ID": fs.ModeSetgid, "sticky": fs.ModeSticky} {
		at := filepath.Join(f.dir, "run")
		put(t, f.dir, "run", "old")
		if err := os.Chmod(at, 0o750|bit); err != nil {
			t.Logf("%s: %v", name, err) // not on every system
			continue
		}
		st, err := os.Stat(at)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode()&bit == 0 {
			t.Logf("%s: not kept here", name)
			continue
		}
		f.refused(t, f.own, writeOf("run", "new", versionOf("old")), why)
		f.refused(t, f.own, editOf("run", "old", "new"), why)
		if now, err := os.Stat(at); err != nil || now.Mode() != st.Mode() {
			t.Errorf("%s: %v, %v", name, now, err)
		}
	}
	if os.Geteuid() == 0 {
		return
	}
	for name, as := range map[string]access{"owner": {uid: os.Geteuid() + 1, gid: os.Getegid(), perm: 0o640}, "group": {uid: os.Geteuid(), gid: 1 << 30, perm: 0o640}} {
		root, err := openDir(f.dir)
		if err != nil {
			t.Fatal(err)
		}
		before := f.tree(t)
		w := placing{path: "plan.md", tmp: tmpName(), was: versionOf(holds(t, f.dir, "plan.md")), version: versionOf("other")}
		if left, err := f.files.place(context.Background(), 0, f.dir, root, &w, "other", &as); err == nil || !strings.HasSuffix(err.Error(), why) || left != nil {
			t.Errorf("another %s: %v, left %v", name, err, left)
		}
		if f.tree(t) != before {
			t.Errorf("another %s: something changed", name)
		}
		root.Close()
	}

	// What cannot be given all of it is open to nobody in between.
	for name, as := range map[string]access{
		"owner":       {uid: os.Geteuid() + 1, gid: os.Getegid(), perm: 0o640},
		"group":       {uid: os.Geteuid(), gid: 1 << 30, perm: 0o640},
		"access list": {uid: os.Geteuid(), gid: os.Getegid(), perm: 0o640, acl: "none"},
	} {
		fh, err := os.OpenFile(filepath.Join(t.TempDir(), "own"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0)
		if err != nil {
			t.Fatal(err)
		}
		err = as.give(fh)
		if st, serr := fh.Stat(); err == nil || err.Error() != why || serr != nil || st.Mode().Perm() != 0 {
			t.Errorf("another %s: %v; the file has %v, %v", name, err, st.Mode(), serr)
		}
		fh.Close()
	}
}

// The list has every file that a path names, with the version of each that
// can be read, and nothing else; docs/ can be read.
func TestFilesList(t *testing.T) {
	f := newFilesFixture(t)
	put(t, f.dir, "AGENTS.md", "rules")
	put(t, f.dir, "docs/design.md", "design")
	put(t, f.dir, "notes/b.md", "b")
	put(t, f.dir, "notes/.hidden/c.md", "c")
	put(t, f.dir, ".claude/settings.json", "{}")
	put(t, f.dir, "bin", "\xff")
	put(t, f.dir, "large.md", strings.Repeat("x", FileMax+1))
	put(t, f.dir, "not\u200bshown.md", "d")
	put(t, f.dir, strings.Repeat("d/", pathParts-1)+"deep.md", "deep")
	put(t, f.dir, strings.Repeat("e/", pathParts)+"deeper.md", "deeper")
	put(t, f.dir2, "other.md", "other")
	os.WriteFile(filepath.Join(f.dir, "bad\xff.md"), []byte("e"), 0o600) // not on every file system
	for _, l := range [][2]string{{"notes/b.md", "link.md"}, {"notes", "link"}, {f.outside, "out"}} {
		if err := os.Symlink(l[0], filepath.Join(f.dir, l[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(f.dir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range do(t, f.own, control.FilesRequest{Op: control.FilesList}).Files {
		if l.Text != "" {
			t.Errorf("the list gives the text of %s", l.Path)
		}
		got = append(got, fmt.Sprintf("%s %d %t", l.Path, l.Size, l.Version != ""))
		if l.Version != "" && l.Version != versionOf(holds(t, f.dir, l.Path)) {
			t.Errorf("the list gives another version of %s", l.Path)
		}
	}
	want := []string{"AGENTS.md 5 true", "bin 1 false", strings.Repeat("d/", pathParts-1) + "deep.md 4 true", "docs/design.md 6 true",
		fmt.Sprintf("large.md %d false", FileMax+1), "notes/b.md 1 true"}
	if !slices.Equal(got, want) {
		t.Fatalf("listed\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if ans := do(t, f.own, readOf("docs/design.md")); ans.File == nil || ans.File.Text != "design" {
		t.Fatalf("read of docs/: %+v", ans.File)
	}
	if os.Geteuid() != 0 { // a file that cannot be read is in the list all the same
		os.Chmod(filepath.Join(f.dir, "AGENTS.md"), 0)
		if ans := do(t, f.own, control.FilesRequest{Op: control.FilesList}); len(ans.Files) != len(want) || ans.Files[0].Path != "AGENTS.md" || ans.Files[0].Version != "" || ans.Files[0].Size != 5 {
			t.Fatalf("listed %+v", ans.Files)
		}
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if ans, err := f.files.list(gone, f.dir); !errors.Is(err, context.Canceled) {
		t.Errorf("a list for a caller that is gone: %+v, %v", ans, err)
	}
	for i := range listMax - len(want) {
		put(t, f.dir, fmt.Sprintf("many/%d", i), "")
	}
	if ans := do(t, f.own, control.FilesRequest{Op: control.FilesList}); len(ans.Files) != listMax {
		t.Errorf("listed %d files", len(ans.Files))
	}
	put(t, f.dir, "many/more", "")
	f.refused(t, f.own, control.FilesRequest{Op: control.FilesList}, "the directory has more than 2000 files")
}

// Every path that Files does not take, and every file that is not one of
// its own directory, is refused to each operation and changes nothing.
func TestFilesPathsRefused(t *testing.T) {
	const (
		dot     = "a path with a part that starts with a dot"
		hidden  = "a path with a character that does not show"
		link    = "a part of the path is a link, or no directory"
		regular = "not a regular file"
		docs    = "no write in docs/"
	)
	f := newFilesFixture(t)
	secret := "outside"
	put(t, f.outside, "secret.md", secret)
	put(t, f.outside, "sub/secret.md", secret)
	put(t, f.dir2, "secret.md", secret)
	put(t, f.dir, "plan.md", secret)
	put(t, f.dir, "docs/secret.md", secret)
	put(t, f.dir, "notes/secret.md", secret)
	put(t, f.dir, ".secret.md", secret)
	put(t, f.dir, ".dir/secret.md", secret)
	for _, l := range [][2]string{{"plan.md", "file-link"}, {f.outside + "/secret.md", "out-file-link"}, {"nowhere", "gone-link"}, {"notes", "dir-link"},
		{f.outside, "out-link"}, {"../scope-c2", "other-link"}, {"docs", "docs-link"}, {".", "notes/self"}} {
		if err := os.Symlink(l[0], filepath.Join(f.dir, l[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(f.dir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(f.dir) // a socket's path is short on every system
	l, err := net.Listen("unix", "sock")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, c := range []struct{ path, why string }{
		{"", "no path"},
		{"a\xff.md", "a path that is not UTF-8"},
		{"/plan.md", "an absolute path"},
		{filepath.Join(f.dir, "plan.md"), "an absolute path"},
		{filepath.Join(f.outside, "secret.md"), "an absolute path"},
		{strings.Repeat("a/", pathParts) + "secret.md", "a path with more than 16 parts"},
		{"notes//secret.md", "a path with an empty part"}, {"notes/", "a path with an empty part"},
		{"..", dot}, {".", dot}, {"../outside/secret.md", dot}, {"../scope-c2/secret.md", dot}, {"notes/../plan.md", dot}, {"notes/../../outside/secret.md", dot},
		{"./plan.md", dot}, {"notes/./secret.md", dot}, {"notes/..", dot}, {".secret.md", dot}, {".dir/secret.md", dot}, {"notes/.secret.md", dot},
		{".claude/settings.json", dot}, {"notes/.new/secret.md", dot}, {tmpPrefix + "0", dot},
		{"a\x00b", hidden}, {"a\nb", hidden}, {"a\tb", hidden}, {"a\x7fb", hidden}, {"a\u200bb", hidden}, {"a\u00adb", hidden}, {"a\u202eb", hidden},
		{"a\u00a0b", hidden}, {"a\u3000b", hidden}, {"a\ufe0fb", hidden}, {"a\U000e0100b", hidden}, {"a\u3164b", hidden}, {"a\u034fb", hidden}, {"a\u2800b", hidden},
		{"a\U0001d159b", hidden}, {"a\U00013441b", hidden}, {"a\U00013442b", hidden}, {"a\U00016fe4b", hidden},
		{" plan.md", hidden}, {"plan.md ", hidden}, {"notes /secret.md", hidden}, {"notes/ secret.md", hidden}, {"\u200b/secret.md", hidden},
		{"notes", regular}, {"pipe", regular}, {"sock", regular}, {"file-link", regular}, {"out-file-link", regular}, {"gone-link", regular}, {"dir-link", regular}, {"out-link", regular}, {"notes/self", regular},
		{"dir-link/secret.md", link}, {"out-link/secret.md", link}, {"out-link/sub/secret.md", link}, {"out-link/new/secret.md", link}, {"other-link/secret.md", link},
		{"docs-link/secret.md", link}, {"notes/self/secret.md", link}, {"gone-link/secret.md", link}, {"plan.md/secret.md", link}, {"plan.md/new/secret.md", link}, {"pipe/secret.md", link},
	} {
		f.refused(t, f.own, readOf(c.path), c.why)
		f.refused(t, f.own, writeOf(c.path, "changed", ""), c.why)
		f.refused(t, f.own, writeOf(c.path, "changed", versionOf(secret)), c.why)
		f.refused(t, f.own, editOf(c.path, secret, "changed"), c.why)
	}
	// docs/ and what is named as it where case does not count, as a file, and
	// in a directory that would have to be made. ſ is another case of s.
	for _, path := range []string{"docs/secret.md", "docs/new.md", "docs/new/new.md", "docs", "DOCS/secret.md", "Docs/new.md", "dOcS/new/new.md", "DOCS", "doc\u017f/new.md"} {
		f.refused(t, f.own, writeOf(path, "changed", ""), docs)
		f.refused(t, f.own, writeOf(path, "changed", versionOf(secret)), docs)
		f.refused(t, f.own, editOf(path, secret, "changed"), docs)
	}
	// Another name that the disk knows it by, here of a file.
	other := newFilesFixture(t)
	put(t, other.dir, "docs", secret)
	if err := os.Link(filepath.Join(other.dir, "docs"), filepath.Join(other.dir, "other")); err != nil {
		t.Fatal(err)
	}
	other.refused(t, other.own, writeOf("other", "changed", versionOf(secret)), docs)
	other.refused(t, other.own, editOf("other", secret, "changed"), docs)
	// They are no names of it, and the longest path is one.
	for _, path := range []string{"notes/docs/new.md", "docs.md", "adocs/new.md", "a b/c d.md", "日本語/メモ.md", "e\u0301.md", "a...b", strings.Repeat("a/", pathParts-1) + "secret.md"} {
		made(t, f.own, writeOf(path, "text", ""))
	}
}

// A call that is none, and one of a session that does not work in the
// scope's directory, are refused; and whatever is there at the directory's
// path but a directory is neither read nor written.
func TestFilesCallsRefused(t *testing.T) {
	f := newFilesFixture(t)
	put(t, f.dir, "plan.md", "text")
	f.refused(t, f.own, control.FilesRequest{Op: control.FilesWrite, Path: "new.md"}, "a write without a text")
	f.refused(t, f.own, editOf("plan.md", "", "x"), "an edit without the passage or what replaces it")
	f.refused(t, f.own, control.FilesRequest{Op: control.FilesEdit, Path: "plan.md", Old: "text"}, "an edit without the passage or what replaces it")
	text := "x"
	for _, op := range []string{"", "delete", "List", "append"} {
		f.refused(t, f.own, control.FilesRequest{Op: op, Path: "plan.md", Text: &text, Version: versionOf("text"), Old: "text", New: &text}, "no such operation on files")
	}
	f.log.take()
	if _, err := f.own.Files(context.Background(), control.FilesRequest{Op: "from the model", Path: "plan.md"}); err == nil || strings.Contains(f.log.take(), "from the model") {
		t.Errorf("answered %v, or logged the operation's name", err)
	}
	for _, path := range []string{"gone.md", "gone/gone.md"} {
		f.refused(t, f.own, readOf(path), "there is no such file")
	}
	put(t, f.dir, "large.md", strings.Repeat("x", FileMax+1))
	put(t, f.dir, "bin.md", "a\xffb")
	f.refused(t, f.own, readOf("large.md"), "the file is over 256 KB, or not UTF-8")
	f.refused(t, f.own, readOf("bin.md"), "the file is over 256 KB, or not UTF-8")

	all := func(api control.API, why string) {
		t.Helper()
		f.refused(t, api, control.FilesRequest{Op: control.FilesList}, why)
		f.refused(t, api, readOf("plan.md"), why)
		f.refused(t, api, writeOf("plan.md", "changed", versionOf("text")), why)
		f.refused(t, api, writeOf("new.md", "new", ""), why)
		f.refused(t, api, editOf("plan.md", "text", "changed"), why)
	}
	const not = "this session does not work in the channel's supervisor's directory"
	f.sql(t, `INSERT INTO threads (id, scope, key, started_by, state, confined, kind, workspace, created_at, updated_at) VALUES
		(30, 'c1', 'k30', 'p', 'running', 1, 'conversation', '/elsewhere', 0, 0), (31, 'c1', 'k31', 'p', 'running', 1, 'conversation', '', 0, 0),
		(32, 'c2', 'k32', 'p', 'running', 1, 'conversation', '`+f.dir+`', 0, 0)`)
	all(f.files.API(nil, "c1", 30), not)         // it works elsewhere
	all(f.files.API(nil, "c1", 31), not)         // nowhere yet
	all(f.files.API(nil, "c1", filesOther), not) // a thread of another scope
	all(f.files.API(nil, "c1", 32), not)         // one that works here
	all(f.files.API(nil, "c1", 99), not)         // no thread
	all(f.files.API(nil, "c3", filesOwn), "the channel's supervisor has no directory yet")

	// A thread that is closed writes nothing, and one that is closed while
	// its call waits for another's write does not either.
	const ended = "the thread of this session is closed"
	state := func(s control.State) {
		t.Helper()
		f.sql(t, fmt.Sprintf(`UPDATE threads SET state = '%s' WHERE id = %d`, s, filesConv))
	}
	for _, s := range []control.State{control.Closed, control.Removed} {
		state(s)
		f.refused(t, f.conv, writeOf("plan.md", "changed", versionOf("text")), ended)
		f.refused(t, f.conv, editOf("plan.md", "text", "changed"), ended)
		f.refused(t, f.conv, writeOf("new.md", "new", ""), ended)
		f.refused(t, f.conv, writeOf("new/new.md", "new", ""), ended)
	}
	for _, s := range []control.State{control.Creating, control.Idle, control.Asleep, control.Failed} {
		state(s)
		v := made(t, f.conv, writeOf("plan.md", "by one that is "+string(s), versionOf("text")))
		made(t, f.conv, writeOf("plan.md", "text", v))
	}
	state(control.Running)
	before := f.tree(t)
	release := f.files.locks.lock(f.dir)
	waited := make(chan error, 1)
	go func() {
		_, err := f.conv.Files(context.Background(), writeOf("plan.md", "changed", versionOf("text")))
		waited <- err
	}()
	time.Sleep(200 * time.Millisecond) // for it to wait
	state(control.Closed)
	release()
	if err := <-waited; err == nil || !strings.HasSuffix(err.Error(), ended) || f.tree(t) != before || f.recorded(t) != 0 {
		t.Errorf("closed while it waited: %v; %d recorded", err, f.recorded(t))
	}

	const cannot = "the channel's supervisor's directory cannot be opened"
	closed := func(what string, put func(at string) error) {
		t.Helper()
		if err := os.Rename(f.dir, f.dir+"-moved"); err != nil {
			t.Fatal(err)
		}
		if err := put(f.dir); err != nil {
			t.Fatal(err)
		}
		for _, r := range []control.FilesRequest{{Op: control.FilesList}, readOf("plan.md"), writeOf("plan.md", "changed", versionOf("text")), writeOf("new.md", "new", "")} {
			before := tree(t, f.root)
			f.log.take()
			if ans, err := f.own.Files(context.Background(), r); err == nil || !strings.HasSuffix(err.Error(), cannot) || strings.Contains(err.Error(), f.dir) {
				t.Errorf("%s there: %s answers %+v, %v", what, r.Op, ans, err)
			}
			if withoutStore(tree(t, f.root)) != withoutStore(before) || f.recorded(t) != 0 {
				t.Errorf("%s there: %s changed something", what, r.Op)
			}
			if !strings.Contains(f.log.take(), "level=ERROR") {
				t.Errorf("%s there: no error line", what)
			}
		}
		if err := os.RemoveAll(f.dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(f.dir+"-moved", f.dir); err != nil {
			t.Fatal(err)
		}
	}
	closed("nothing", func(string) error { return nil })
	closed("a file", func(at string) error { return os.WriteFile(at, nil, 0o600) })
	closed("a link", func(at string) error { return os.Symlink(f.dir+"-moved", at) })
	closed("a link to another scope's", func(at string) error { return os.Symlink(f.dir2, at) })
}

// withoutStore is a tree without the store's files.
func withoutStore(tree string) string {
	var keep []string
	for _, l := range strings.Split(tree, "\n") {
		if !strings.Contains(l, "/state/") && !strings.Contains(l, "/state ") {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

// A session reaches its own scope's directory only, where another has a
// file of the same name.
func TestFilesOfTwoScopes(t *testing.T) {
	f := newFilesFixture(t)
	put(t, f.dir, "plan.md", "c1's")
	put(t, f.dir2, "plan.md", "c2's")
	for _, c := range []struct {
		api  control.API
		text string
		dir  string
	}{{f.own, "c1's", f.dir}, {f.conv, "c1's", f.dir}, {f.other, "c2's", f.dir2}} {
		ans := do(t, c.api, readOf("plan.md"))
		if ans.File == nil || ans.File.Text != c.text {
			t.Fatalf("read %+v, want %q", ans.File, c.text)
		}
		if l := do(t, c.api, control.FilesRequest{Op: control.FilesList}).Files; len(l) != 1 || l[0].Version != versionOf(c.text) {
			t.Fatalf("listed %+v", l)
		}
		f.misfit(t, c.api, writeOf("plan.md", "changed", versionOf(map[bool]string{true: "c2's", false: "c1's"}[c.text == "c1's"])), "the file has changed since that version", c.text)
	}
	made(t, f.other, writeOf("plan.md", "new", versionOf("c2's")))
	made(t, f.other, writeOf("new.md", "new", ""))
	if holds(t, f.dir, "plan.md") != "c1's" || holds(t, f.dir2, "plan.md") != "new" {
		t.Fatalf("plan.md of c1 %q, of c2 %q", holds(t, f.dir, "plan.md"), holds(t, f.dir2, "plan.md"))
	}
	if _, err := os.Stat(filepath.Join(f.dir, "new.md")); err == nil {
		t.Fatal("c2's new file is in c1's directory")
	}
}

// A write that does not fit what is there changes nothing, and answers with
// what is there.
func TestFilesWritesThatDoNotFit(t *testing.T) {
	f := newFilesFixture(t)
	const text = "abab\nline\nline\n"
	put(t, f.dir, "plan.md", text)
	f.misfit(t, f.own, writeOf("plan.md", "new", ""), "the file is there already", text)
	f.misfit(t, f.own, writeOf("plan.md", "new", versionOf("other")), "the file has changed since that version", text)
	f.misfit(t, f.own, writeOf("plan.md", "new", versionOf(text)[:20]), "the file has changed since that version", text)
	f.misfit(t, f.own, writeOf("plan.md", "new", strings.ToUpper(versionOf(text))), "the file has changed since that version", text)
	f.misfit(t, f.own, writeOf("plan.md", text, versionOf("other")), "the file has changed since that version", text)
	f.misfit(t, f.own, editOf("plan.md", "Line", "x"), "the passage is not in the file", text)
	f.misfit(t, f.own, editOf("plan.md", "line\n", "x"), "the passage is in the file more than once", text)
	put(t, f.dir, "aaa.md", "aaa")
	f.misfit(t, f.own, editOf("aaa.md", "aa", "b"), "the passage is in the file more than once", "aaa") // the second overlaps the first
	before := f.tree(t)
	for _, c := range []struct {
		r   control.FilesRequest
		why string
	}{
		{writeOf("gone.md", "new", versionOf(text)), "there is no such file any more"},
		{writeOf("gone/gone.md", "new", versionOf(text)), "there is no such file any more"},
		{editOf("gone.md", "a", "b"), "there is no such file"},
		{editOf("gone/gone.md", "a", "b"), "there is no such file"},
	} {
		if ans := do(t, f.own, c.r); ans.Refused != c.why || ans.File != nil {
			t.Errorf("%s %s: answered %q with %+v, want %q", c.r.Op, c.r.Path, ans.Refused, ans.File, c.why)
		}
	}
	// What these tools do not read they do not write over.
	put(t, f.dir, "large.md", strings.Repeat("x", FileMax+1))
	put(t, f.dir, "bin.md", "a\xffb")
	before = f.tree(t)
	for _, path := range []string{"large.md", "bin.md"} {
		for _, r := range []control.FilesRequest{writeOf(path, "new", ""), writeOf(path, "new", versionOf("a\xffb")), editOf(path, "x", "y"), editOf(path, "a", "y")} {
			ans := do(t, f.own, r)
			if ans.Refused != "the file is over 256 KB, or not UTF-8" || ans.File == nil || ans.File.Text != "" || ans.File.Version != "" {
				t.Errorf("%s %s: answered %q with %+v", r.Op, r.Path, ans.Refused, ans.File)
			}
		}
	}
	if f.tree(t) != before || f.recorded(t) != 0 {
		t.Fatal("a write that does not fit changed something")
	}
	if log := f.log.take(); !strings.Contains(log, "a supervisor's write does not fit the file") || strings.Contains(log, "level=WARN") || strings.Contains(log, "is written") {
		t.Fatalf("logged %s", log)
	}
	// A passage that stands there once is replaced, at the beginning, at the
	// end, and by nothing.
	for _, c := range []struct{ passage, by, want string }{{"abab", "a", "a\nline\nline\n"}, {"line\nline\n", "two lines", "a\ntwo lines"}, {"a\n", "", "two lines"}} {
		made(t, f.own, editOf("plan.md", c.passage, c.by))
		if holds(t, f.dir, "plan.md") != c.want {
			t.Fatalf("the file holds %q, want %q", holds(t, f.dir, "plan.md"), c.want)
		}
	}
}

// Each limit holds at its value and is refused past it, to a write and to an
// edit.
func TestFilesLimits(t *testing.T) {
	f := newFilesFixture(t)
	v := made(t, f.own, writeOf("large.md", strings.Repeat("x", FileMax-1)+"y", ""))
	f.refused(t, f.own, writeOf("large.md", strings.Repeat("x", FileMax+1), v), "a file over 256 KB")
	f.refused(t, f.own, editOf("large.md", "y", "yz"), "a file over 256 KB")
	f.refused(t, f.own, writeOf("new.md", strings.Repeat("x", FileMax+1), ""), "a file over 256 KB")
	f.refused(t, f.own, writeOf("new/new.md", strings.Repeat("x", FileMax+1), ""), "a file over 256 KB")
	f.refused(t, f.own, writeOf("new.md", "a\xff", ""), "a text that is not UTF-8")
	made(t, f.own, writeOf("ja.md", "あい", ""))
	f.refused(t, f.own, editOf("ja.md", "あ", "\xe3"), "a text that is not UTF-8")
	f.refused(t, f.own, editOf("ja.md", "\x81\x82", ""), "a text that is not UTF-8") // half a character

	// The user's files, whatever the case.
	large := strings.Repeat("x", UserFileMax+1)
	for _, name := range UserFiles {
		for _, name := range []string{name, strings.ToLower(name), strings.ToUpper(name), strings.ToLower(name[:1]) + name[1:]} {
			f.refused(t, f.own, writeOf(name, large, ""), "one of the user's files over 32 KB")
			v := made(t, f.own, writeOf(name, large[1:], ""))
			f.refused(t, f.own, writeOf(name, large, v), "one of the user's files over 32 KB")
			f.refused(t, f.own, editOf(name, large[1:], large), "one of the user's files over 32 KB")
			if err := os.Remove(filepath.Join(f.dir, name)); err != nil {
				t.Fatal(err)
			}
		}
		made(t, f.own, writeOf("notes/"+name, large, "")) // not the user's
	}
	made(t, f.own, writeOf(UserFiles[0]+"/large.md", large, "")) // nor is one in a directory named so

	if os.Geteuid() == 0 {
		return
	}
	// What the daemon may not write it does not put another file over.
	put(t, f.dir, "kept.md", "kept")
	if err := os.Chmod(filepath.Join(f.dir, "kept.md"), 0o400); err != nil {
		t.Fatal(err)
	}
	f.refused(t, f.own, writeOf("kept.md", "changed", versionOf("kept")), "a file that the daemon may not write")
	f.refused(t, f.own, editOf("kept.md", "kept", "changed"), "a file that the daemon may not write")
	put(t, f.dir, "closed/kept.md", "kept")
	if err := os.Chmod(filepath.Join(f.dir, "closed"), 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(f.dir, "closed"), 0o700)
	f.refused(t, f.own, writeOf("closed/kept.md", "changed", versionOf("kept")), "writing the file: permission denied")
	f.refused(t, f.own, writeOf("closed/new.md", "new", ""), "writing the file: permission denied")
	f.refused(t, f.own, writeOf("closed/new/new.md", "new", ""), "making a directory on the way: permission denied")
}

// sized makes the file name in dir, size bytes long.
func sized(t *testing.T, dir, name string, size int64) {
	t.Helper()
	put(t, dir, name, "")
	if err := os.Truncate(filepath.Join(dir, name), size); err != nil {
		t.Fatal(err)
	}
}

// The directory holds 20 MB besides docs/, hidden files too; a write that
// makes it no larger is made all the same.
func TestFilesBytesOfTheDirectory(t *testing.T) {
	const why = "more than 20 MB in the directory"
	f := newFilesFixture(t)
	sized(t, f.dir, "docs/large", dirBytes)
	sized(t, f.dir, "DOCS/larger", dirBytes) // where case counts, a second one
	sized(t, f.dir, "notes/.large", dirBytes-100)
	made(t, f.own, writeOf("a.md", strings.Repeat("a", 100), ""))
	f.refused(t, f.own, writeOf("b.md", "b", ""), why)
	f.refused(t, f.own, writeOf("a.md", strings.Repeat("a", 101), versionOf(strings.Repeat("a", 100))), why)
	f.refused(t, f.own, editOf("a.md", strings.Repeat("a", 100), strings.Repeat("b", 101)), why)
	made(t, f.own, writeOf("empty.md", "", "")) // it holds nothing more
	made(t, f.own, writeOf("a.md", strings.Repeat("b", 100), versionOf(strings.Repeat("a", 100))))
	v := made(t, f.own, writeOf("a.md", strings.Repeat("b", 99), versionOf(strings.Repeat("b", 100))))
	made(t, f.own, writeOf("b.md", "b", ""))
	f.refused(t, f.own, writeOf("a.md", strings.Repeat("b", 100), v), why)
	sized(t, f.dir, "notes/.large", dirBytes+100) // a person's
	made(t, f.own, writeOf("a.md", "b", v))
	f.refused(t, f.own, writeOf("a.md", "bb", versionOf("b")), why)
}

// The directory holds 500 files besides docs/, hidden ones and those that
// are no regular files too.
func TestFilesNumberInTheDirectory(t *testing.T) {
	const why = "more than 500 files in the directory"
	f := newFilesFixture(t)
	for i := range 600 {
		put(t, f.dir, fmt.Sprintf("docs/%d", i), "")
	}
	for i := range 248 {
		put(t, f.dir, fmt.Sprintf("notes/docs/%d/x", i), "")
		put(t, f.dir, fmt.Sprintf(".hidden/%d", i), "")
	}
	if err := os.Symlink(f.outside, filepath.Join(f.dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(f.dir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	made(t, f.own, writeOf("499.md", "", "")) // after 498
	v := made(t, f.own, writeOf("500.md", "500", ""))
	f.refused(t, f.own, writeOf("501.md", "", ""), why)
	f.refused(t, f.own, writeOf("new/501.md", "", ""), why)
	made(t, f.own, writeOf("500.md", "another", v))
	if err := os.Remove(filepath.Join(f.dir, "499.md")); err != nil {
		t.Fatal(err)
	}
	made(t, f.own, writeOf("new/500.md", "", ""))
	f.refused(t, f.own, writeOf("501.md", "", ""), why)

	root, err := openDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if err := room(gone, root, false, 1); !errors.Is(err, context.Canceled) {
		t.Errorf("counted for a caller that is gone: %v", err)
	}
	if os.Geteuid() == 0 {
		return
	}
	// What the daemon may not read counts as nothing, and keeps no one from
	// writing.
	closed := filepath.Join(f.dir, ".hidden")
	if err := os.Chmod(closed, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(closed, 0o700) })
	if ans, err := f.own.Files(context.Background(), writeOf("501.md", "x", "")); err != nil || ans.Refused != "" {
		t.Errorf("beside a directory that the daemon may not read: %+v, %v", ans, err)
	}
}

// atOnce runs the calls at the same moment, in as many sessions, and returns
// how each was answered.
func atOnce(apis []control.API, rs []control.FilesRequest) ([]control.FilesAnswer, []error) {
	answers, errs := make([]control.FilesAnswer, len(rs)), make([]error, len(rs))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, r := range rs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			answers[i], errs[i] = apis[i%len(apis)].Files(context.Background(), r)
		}()
	}
	close(start)
	wg.Wait()
	return answers, errs
}

// Two sessions read a file and both write it, at once: the second write
// changes nothing and is answered with what the first wrote; after it writes
// again both changes are in the file.
func TestFilesTwoWriteAtOnce(t *testing.T) {
	for i := range 40 {
		f := newFilesFixture(t)
		put(t, f.dir, "plan.md", "base\n")
		base := do(t, f.own, readOf("plan.md")).File
		if other := do(t, f.conv, readOf("plan.md")).File; *other != *base {
			t.Fatalf("read %+v and %+v", base, other)
		}
		if i%2 == 0 { // the two meet in every step
			f.files.Hook = func(string) { time.Sleep(time.Millisecond) }
		}
		apis, lines := []control.API{f.own, f.conv}, []string{"of one\n", "of two\n"}
		answers, errs := atOnce(apis, []control.FilesRequest{writeOf("plan.md", base.Text+lines[0], base.Version), writeOf("plan.md", base.Text+lines[1], base.Version)})
		if errs[0] != nil || errs[1] != nil {
			t.Fatal(errs)
		}
		first, second := 0, 1
		if answers[0].Refused != "" {
			first, second = 1, 0
		}
		won, lost := answers[first], answers[second]
		if won.Refused != "" || won.File.Version != versionOf("base\n"+lines[first]) || lost.Refused != "the file has changed since that version" ||
			lost.File == nil || lost.File.Text != "base\n"+lines[first] || lost.File.Version != won.File.Version {
			t.Fatalf("answered %+v %+v and %+v %+v", answers[0], answers[0].File, answers[1], answers[1].File)
		}
		if got := holds(t, f.dir, "plan.md"); got != "base\n"+lines[first] {
			t.Fatalf("the file holds %q", got)
		}
		made(t, apis[second], writeOf("plan.md", lost.File.Text+lines[second], lost.File.Version))
		if got := holds(t, f.dir, "plan.md"); got != "base\n"+lines[first]+lines[second] {
			t.Fatalf("the file holds %q", got)
		}
		if own := ownFiles(t, f.root); len(own) != 0 || f.recorded(t) != 0 {
			t.Fatalf("left: %v, %d recorded", own, f.recorded(t))
		}
	}
}

// Edits of two passages of one file at once are both in it, and of new files
// of one name at once one is made.
func TestFilesManyAtOnce(t *testing.T) {
	f := newFilesFixture(t)
	var text, want strings.Builder
	var edits []control.FilesRequest
	for i := range 12 {
		fmt.Fprintf(&text, "line %d;\n", i)
		fmt.Fprintf(&want, "changed %d;\n", i)
		edits = append(edits, editOf("plan.md", fmt.Sprintf("line %d;", i), fmt.Sprintf("changed %d;", i)))
	}
	put(t, f.dir, "plan.md", text.String())
	apis := []control.API{f.own, f.conv}
	answers, errs := atOnce(apis, edits)
	for i := range answers {
		if errs[i] != nil || answers[i].Refused != "" {
			t.Errorf("edit %d: %+v, %v", i, answers[i], errs[i])
		}
	}
	if got := holds(t, f.dir, "plan.md"); got != want.String() {
		t.Fatalf("the file holds %q", got)
	}
	var news []control.FilesRequest
	for i := range 12 {
		news = append(news, writeOf("new/new.md", fmt.Sprint(i), ""))
	}
	answers, errs = atOnce(apis, news)
	var wrote []string
	for i := range answers {
		switch {
		case errs[i] != nil:
			t.Error(errs[i])
		case answers[i].Refused == "":
			wrote = append(wrote, fmt.Sprint(i))
		case answers[i].Refused != "the file is there already" || answers[i].File.Text != holds(t, f.dir, "new/new.md"):
			t.Errorf("answered %+v %+v", answers[i], answers[i].File)
		}
	}
	if len(wrote) != 1 || holds(t, f.dir, "new/new.md") != wrote[0] {
		t.Fatalf("written by %v; the file holds %q", wrote, holds(t, f.dir, "new/new.md"))
	}
}

// While one write is being made, at any of its steps, another of the same
// directory waits, and one of another directory does not.
func TestFilesOneWriteAtATime(t *testing.T) {
	for _, step := range []string{"written", "placing", "placed"} {
		f := newFilesFixture(t)
		put(t, f.dir, "plan.md", "base")
		held, release := make(chan struct{}), make(chan struct{})
		var reached atomic.Bool
		f.files.Hook = func(at string) {
			if at == step && reached.CompareAndSwap(false, true) {
				close(held)
				<-release
			}
		}
		first, second := make(chan control.FilesAnswer, 1), make(chan control.FilesAnswer, 1)
		call := func(api control.API, r control.FilesRequest, to chan control.FilesAnswer) {
			ans, err := api.Files(context.Background(), r)
			if err != nil {
				t.Error(err)
			}
			to <- ans
		}
		go call(f.own, writeOf("plan.md", "first", versionOf("base")), first)
		<-held
		go call(f.conv, writeOf("plan.md", "second", versionOf("base")), second)
		made(t, f.other, writeOf("plan.md", "other", ""))
		do(t, f.conv, readOf("plan.md"))
		select {
		case ans := <-second:
			t.Fatalf("at %q, another write was answered %+v", step, ans)
		case <-time.After(200 * time.Millisecond):
		}
		close(release)
		if ans := <-first; ans.Refused != "" || ans.File.Version != versionOf("first") {
			t.Fatalf("at %q, the first is answered %+v", step, ans)
		}
		if ans := <-second; ans.Refused != "the file has changed since that version" || ans.File.Text != "first" {
			t.Fatalf("at %q, the second is answered %+v %+v", step, ans, ans.File)
		}
	}
}

// Callers at once do not together pass a limit that each alone would keep.
func TestFilesLimitsAtOnce(t *testing.T) {
	apis := func(f *filesFixture) []control.API { return []control.API{f.own, f.conv} }
	slow := func(step string) {
		if step == "written" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	one := func(f *filesFixture, rs []control.FilesRequest, why string) {
		t.Helper()
		f.files.Hook = slow
		answers, errs := atOnce(apis(f), rs)
		n := 0
		for i := range rs {
			if errs[i] == nil && answers[i].Refused == "" {
				n++
			} else if errs[i] == nil || !strings.HasSuffix(errs[i].Error(), why) {
				t.Errorf("%+v, %v", answers[i], errs[i])
			}
		}
		if n != 1 {
			t.Errorf("%d writes were made, want 1", n)
		}
		if own := ownFiles(t, f.dir); len(own) != 0 {
			t.Errorf("left: %v", own)
		}
	}
	f := newFilesFixture(t)
	for i := range dirFiles - 1 {
		put(t, f.dir, fmt.Sprintf("notes/%d", i), "")
	}
	var rs []control.FilesRequest
	for i := range 6 {
		rs = append(rs, writeOf(fmt.Sprintf("new%d.md", i), "new", ""))
	}
	one(f, rs, "more than 500 files in the directory")

	f = newFilesFixture(t)
	sized(t, f.dir, "large", dirBytes-150)
	rs = nil
	for i := range 6 {
		rs = append(rs, writeOf(fmt.Sprintf("new%d.md", i), strings.Repeat("x", 100), ""))
	}
	one(f, rs, "more than 20 MB in the directory")
}

// Another name of a replaced file, a hard link, keeps the old content, also
// one that is outside; and the file is another one then.
func TestFilesAnotherNameKeepsTheOld(t *testing.T) {
	f := newFilesFixture(t)
	put(t, f.dir, "plan.md", "old")
	for _, name := range []string{filepath.Join(f.outside, "name.md"), filepath.Join(f.dir, "name.md")} {
		if err := os.Link(filepath.Join(f.dir, "plan.md"), name); err != nil {
			t.Fatal(err)
		}
	}
	made(t, f.own, writeOf("plan.md", "new", versionOf("old")))
	made(t, f.own, editOf("plan.md", "new", "newer"))
	if holds(t, f.dir, "plan.md") != "newer" || holds(t, f.dir, "name.md") != "old" || holds(t, f.outside, "name.md") != "old" {
		t.Fatalf("plan.md %q, its other names %q and %q", holds(t, f.dir, "plan.md"), holds(t, f.dir, "name.md"), holds(t, f.outside, "name.md"))
	}
}

// swap is what happens in the directory at a step of a call, once.
type swap struct {
	step string
	nth  int // which time the step is reached, from 1
	do   func(t *testing.T, f *filesFixture)
}

// at sets s as f's hook.
func (f *filesFixture) at(t *testing.T, s swap) (done func() bool) {
	n := 0
	f.files.Hook = func(step string) {
		if step != s.step {
			return
		}
		if n++; n == s.nth {
			s.do(t, f)
		}
	}
	return func() bool { f.files.Hook = nil; return n >= s.nth }
}

// replace puts what by makes in the place of the file or directory name of
// the fixture's directory, which is kept under name-moved.
func replace(name string, by func(f *filesFixture, at string) error) func(*testing.T, *filesFixture) {
	return func(t *testing.T, f *filesFixture) {
		at := filepath.Join(f.dir, name)
		err := os.Rename(at, at+"-moved")
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			err = by(f, at)
		}
		if err != nil {
			t.Error(err)
		}
	}
}

func linkTo(target func(f *filesFixture) string) func(*filesFixture, string) error {
	return func(f *filesFixture, at string) error { return os.Symlink(target(f), at) }
}

func pipe(_ *filesFixture, at string) error { return syscall.Mkfifo(at, 0o600) }

// What is put in the place of the file, of a directory on its way or of the
// directory itself between the check and the write is not written to: the
// call is refused or does not fit, whatever is there then stays as it was
// put, and nothing of the daemon's own is left.
func TestFilesSwappedMeanwhile(t *testing.T) {
	outFile := linkTo(func(f *filesFixture) string { return filepath.Join(f.outside, "plan.md") })
	outDir := linkTo(func(f *filesFixture) string { return f.outside })
	inFile := linkTo(func(*filesFixture) string { return "kept.md" })
	inDir := linkTo(func(*filesFixture) string { return "kept" })
	another := func(text string) func(*filesFixture, string) error {
		return func(_ *filesFixture, at string) error { return os.WriteFile(at, []byte(text), 0o600) }
	}
	dir := func(_ *filesFixture, at string) error { return os.MkdirAll(filepath.Join(at, "deeper"), 0o700) }
	const (
		replaced    = "the file was replaced while it was read"
		changed     = "the file changed while it was written"
		onTheWay    = "a part of the path was replaced meanwhile"
		notADir     = "a part of the path is a link, or no directory"
		notAFile    = "not a regular file"
		escapes     = "path escapes from parent"
		file, fresh = "notes/plan.md", "notes/new.md"
	)
	for _, c := range []struct {
		name  string
		path  string // notes/plan.md is there before, with base
		at    swap
		err   string // the call is refused for it
		there string // or it does not fit, and is answered with this; "-" for none
	}{
		// Once the file was looked at and before it is opened, the first time
		// (to see that the write fits) and the last (before it is placed).
		{name: "file, a link outside, opening", path: file, at: swap{"opening", 1, replace(file, outFile)}, err: escapes},
		{name: "file, a link inside, opening", path: file, at: swap{"opening", 1, replace(file, inFile)}, err: replaced},
		{name: "file, a pipe, opening", path: file, at: swap{"opening", 1, replace(file, pipe)}, err: replaced},
		{name: "file, another, opening", path: file, at: swap{"opening", 1, replace(file, another("base"))}, err: replaced},
		{name: "file, a link outside, opening last", path: file, at: swap{"opening", 2, replace(file, outFile)}, err: escapes},
		{name: "file, a link inside, opening last", path: file, at: swap{"opening", 2, replace(file, inFile)}, err: replaced},
		{name: "file, a pipe, opening last", path: file, at: swap{"opening", 2, replace(file, pipe)}, err: replaced},
		{name: "directory, a link outside, entering", path: file, at: swap{"entering", 1, replace("notes", outDir)}, err: escapes},
		{name: "directory, a link inside, entering", path: file, at: swap{"entering", 1, replace("notes", inDir)}, err: onTheWay},
		{name: "directory, a pipe, entering", path: file, at: swap{"entering", 1, replace("notes", pipe)}, err: "not a directory"},
		{name: "directory, another, entering", path: file, at: swap{"entering", 1, replace("notes", dir)}, err: onTheWay},
		{name: "directory, a link inside, entering for a new file", path: fresh, at: swap{"entering", 1, replace("notes", inDir)}, err: onTheWay},

		// Once the new content is written beside it.
		{name: "file, a link outside", path: file, at: swap{"written", 1, replace(file, outFile)}, err: notAFile},
		{name: "file, a link inside", path: file, at: swap{"written", 1, replace(file, inFile)}, err: notAFile},
		{name: "file, a pipe", path: file, at: swap{"written", 1, replace(file, pipe)}, err: notAFile},
		{name: "file, a directory", path: file, at: swap{"written", 1, replace(file, dir)}, err: notAFile},
		{name: "file, changed", path: file, at: swap{"written", 1, replace(file, another("a person's"))}, there: "a person's"},
		{name: "file, gone", path: file, at: swap{"written", 1, replace(file, func(*filesFixture, string) error { return nil })}, there: "-"},
		{name: "file, other permissions", path: file, at: swap{"written", 1, func(t *testing.T, f *filesFixture) { os.Chmod(filepath.Join(f.dir, file), 0o644) }}, there: "base"},
		{name: "new file, there", path: fresh, at: swap{"written", 1, replace(fresh, another("a person's"))}, there: "a person's"},
		{name: "new file, a link outside", path: fresh, at: swap{"written", 1, replace(fresh, outFile)}, err: notAFile},
		{name: "new file, a link that leads nowhere", path: fresh, at: swap{"written", 1, replace(fresh, linkTo(func(*filesFixture) string { return "nowhere" }))}, err: notAFile},
		{name: "new file, there, placing", path: fresh, at: swap{"placing", 1, replace(fresh, another("a person's"))}, there: "a person's"},
		{name: "new file, a link outside, placing", path: fresh, at: swap{"placing", 1, replace(fresh, outFile)}, err: notAFile},
		{name: "new file, a link that leads nowhere, placing", path: fresh, at: swap{"placing", 1, replace(fresh, linkTo(func(*filesFixture) string { return "nowhere" }))}, err: notAFile},
		{name: "new file, a directory, placing", path: fresh, at: swap{"placing", 1, replace(fresh, dir)}, err: notAFile},
		{name: "directory, a link outside", path: file, at: swap{"written", 1, replace("notes", outDir)}, err: notADir},
		{name: "directory, a link inside", path: file, at: swap{"written", 1, replace("notes", inDir)}, err: notADir},
		{name: "directory, another", path: file, at: swap{"written", 1, replace("notes", dir)}, there: "-"},
		{name: "directory, gone", path: file, at: swap{"written", 1, replace("notes", func(*filesFixture, string) error { return nil })}, there: "-"},
		{name: "directory, a link outside, for a new file", path: fresh, at: swap{"written", 1, replace("notes", outDir)}, err: notADir},
		{name: "directory, another, for a new file", path: fresh, at: swap{"written", 1, replace("notes", dir)}, there: "-"},
		{name: "made directory, a link outside", path: "made/new.md", at: swap{"written", 1, replace("made", outDir)}, err: notADir},
		{name: "made directory, a link outside, entering", path: "made/new.md", at: swap{"entering", 1, replace("made", outDir)}, err: escapes},
		{name: "made directory, a link inside, entering", path: "made/new.md", at: swap{"entering", 1, replace("made", inDir)}, err: onTheWay},
		{name: "the directory, a link outside", path: file, at: swap{"written", 1, replace("", outDir)}, there: "base"},
		{name: "the directory, a link to another scope's", path: file, at: swap{"written", 1, replace("", linkTo(func(f *filesFixture) string { return f.dir2 }))}, there: "base"},
		{name: "the directory, a link to itself", path: file, at: swap{"written", 1, replace("", linkTo(func(f *filesFixture) string { return f.dir + "-moved" }))}, there: "base"},
		{name: "the directory, another", path: file, at: swap{"written", 1, replace("", dir)}, there: "base"},
		{name: "the directory, gone", path: file, at: swap{"written", 1, replace("", func(*filesFixture, string) error { return nil })}, there: "base"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFilesFixture(t)
			for _, dir := range []string{f.dir, f.dir2} {
				put(t, dir, file, "base")
				put(t, dir, "notes/kept.md", "base")
				put(t, dir, "kept/plan.md", "base")
			}
			put(t, f.outside, "plan.md", "base")
			r := writeOf(c.path, "written", versionOf("base"))
			if c.path != file {
				r.Version = ""
			}
			var put string
			done := f.at(t, swap{c.at.step, c.at.nth, func(t *testing.T, f *filesFixture) {
				c.at.do(t, f)
				put = withoutStore(withoutOwn(tree(t, f.root)))
			}})
			type answer struct {
				ans control.FilesAnswer
				err error
			}
			got := make(chan answer, 1)
			go func() {
				ans, err := f.own.Files(context.Background(), r)
				got <- answer{ans, err}
			}()
			var a answer
			select {
			case a = <-got:
			case <-time.After(10 * time.Second):
				t.Fatal("not answered")
			}
			if !done() {
				t.Fatal("the step was not reached")
			}
			switch {
			case c.err != "":
				if a.err == nil || !strings.HasSuffix(a.err.Error(), c.err) {
					t.Errorf("answered %+v, %v, want it refused: %s", a.ans, a.err, c.err)
				}
			case a.err != nil || a.ans.Refused != changed:
				t.Errorf("answered %+v, %v, want %q", a.ans, a.err, changed)
			case c.there == "-":
				if a.ans.File != nil {
					t.Errorf("answered with %+v, want none", a.ans.File)
				}
			case a.ans.File == nil || a.ans.File.Text != c.there || a.ans.File.Version != versionOf(c.there):
				t.Errorf("answered with %+v, want %q", a.ans.File, c.there)
			}
			if after := withoutStore(tree(t, f.root)); after != put {
				t.Errorf("there is now\n%s\nand was when it was swapped\n%s", after, put)
			}
			if n := f.recorded(t); n != 0 {
				t.Errorf("%d writes are recorded", n)
			}
		})
	}
}

// withoutOwn is a tree without the daemon's own files, and without the
// directories that the write made for them.
func withoutOwn(tree string) string {
	var keep []string
	for _, l := range strings.Split(tree, "\n") {
		if !strings.Contains(l, "/"+tmpPrefix) {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

// A read and a list of what is swapped meanwhile give nothing of what is
// outside.
func TestFilesSwappedWhileRead(t *testing.T) {
	f := newFilesFixture(t)
	put(t, f.dir, "notes/plan.md", "base")
	put(t, f.outside, "plan.md", "outside")
	outside := linkTo(func(f *filesFixture) string { return f.outside })
	for _, c := range []struct {
		r  control.FilesRequest
		at swap
	}{
		{readOf("notes/plan.md"), swap{"entering", 1, replace("notes", outside)}},
		{readOf("notes/plan.md"), swap{"opening", 1, replace("notes/plan.md", linkTo(func(f *filesFixture) string { return filepath.Join(f.outside, "plan.md") }))}},
		{readOf("notes/plan.md"), swap{"opening", 1, replace("notes/plan.md", pipe)}},
		{control.FilesRequest{Op: control.FilesList}, swap{"entering", 1, replace("notes", outside)}},
		{control.FilesRequest{Op: control.FilesList}, swap{"entering", 1, replace("notes", pipe)}},
	} {
		done := f.at(t, c.at)
		ans, err := f.own.Files(context.Background(), c.r)
		if !done() {
			t.Fatal("the step was not reached")
		}
		if c.r.Op == control.FilesRead && err == nil || ans.File != nil || len(ans.Files) != 0 {
			t.Errorf("%s: answered %+v %+v, %v", c.r.Op, ans.File, ans.Files, err)
		}
		for _, name := range []string{"notes/plan.md", "notes"} {
			at := filepath.Join(f.dir, name)
			if _, err := os.Lstat(at + "-moved"); err == nil {
				os.Remove(at)
				if err := os.Rename(at+"-moved", at); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// A file that grows over the limit while it is opened is not read.
func TestFilesGrowsWhileRead(t *testing.T) {
	f := newFilesFixture(t)
	put(t, f.dir, "plan.md", "base")
	done := f.at(t, swap{"opening", 1, func(t *testing.T, _ *filesFixture) {
		if err := os.Truncate(filepath.Join(f.dir, "plan.md"), FileMax+1); err != nil {
			t.Error(err)
		}
	}})
	if ans, err := f.own.Files(context.Background(), readOf("plan.md")); err == nil || !strings.HasSuffix(err.Error(), "the file is over 256 KB, or not UTF-8") || !done() {
		t.Fatalf("answered %+v, %v", ans, err)
	}
}

// What a write cannot take back, as where its directory may not be written
// any more, stays recorded and is taken back by the next write.
func TestFilesTakenBackLater(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes everywhere")
	}
	for _, mode := range []os.FileMode{0o500, 0} { // it cannot be removed, or not even looked at
		t.Run(mode.String(), func(t *testing.T) {
			f := newFilesFixture(t)
			put(t, f.dir, "notes/plan.md", "base")
			notes := filepath.Join(f.dir, "notes")
			done := f.at(t, swap{"written", 1, func(t *testing.T, _ *filesFixture) { os.Chmod(notes, mode) }})
			t.Cleanup(func() { os.Chmod(notes, 0o700) })
			leftOver := func() {
				t.Helper()
				os.Chmod(notes, 0o700)
				if own, has := ownFiles(t, f.dir), holds(t, f.dir, "notes/plan.md"); len(own) != 1 || f.recorded(t) != 1 || has != "base" {
					t.Fatalf("own files %v, %d recorded, the file holds %q", own, f.recorded(t), has)
				}
				os.Chmod(notes, mode)
			}
			if ans, err := f.own.Files(context.Background(), writeOf("notes/plan.md", "new", versionOf("base"))); err == nil || !done() {
				t.Fatalf("answered %+v", ans)
			}
			leftOver()
			if !strings.Contains(f.log.take(), "is not settled") {
				t.Error("not logged")
			}
			made(t, f.conv, writeOf("other.md", "x", "")) // it cannot yet
			leftOver()
			if !strings.Contains(f.log.take(), "cannot be settled") {
				t.Error("not logged")
			}
			if err := os.Chmod(notes, 0o700); err != nil {
				t.Fatal(err)
			}
			made(t, f.other, writeOf("other.md", "x", "")) // another directory's
			if f.recorded(t) != 1 {
				t.Fatal("a write of another directory settled it")
			}
			if ans := do(t, f.conv, writeOf("notes/plan.md", "y", "")); ans.Refused == "" { // any write does it
				t.Fatalf("answered %+v", ans)
			}
			if own := ownFiles(t, f.dir); len(own) != 0 || f.recorded(t) != 0 {
				t.Fatalf("own files %v, %d recorded", own, f.recorded(t))
			}
		})
	}
}

// What someone puts in the place of the daemon's own file is neither placed
// nor removed, also where that was removed and its inode is free.
func TestFilesOwnFileIsAnothers(t *testing.T) {
	const file, fresh = "notes/plan.md", "notes/new.md"
	for _, c := range []struct {
		name, path, step string
		moved            bool   // and not removed
		another          bool   // it is put there; else there is none any more
		there            string // what is at the path then, for the answer
	}{
		{"replacing a file", file, "written", true, true, "base"},
		{"replacing a file, removed", file, "written", false, true, "base"},
		{"replacing a file, gone", file, "written", true, false, "base"},
		{"replacing a changed file, removed", file, "written", false, true, "another's"},
		{"a new file", fresh, "written", true, true, ""},
		{"a new file, removed", fresh, "written", false, true, ""},
		{"a new file, gone", fresh, "written", true, false, ""},
		{"a new file, placing", fresh, "placing", true, true, "another's"},
		{"a new file, placing, removed", fresh, "placing", false, true, "another's"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFilesFixture(t)
			put(t, f.dir, file, "base")
			var own, after string
			done := f.at(t, swap{c.step, 1, func(t *testing.T, f *filesFixture) {
				own = ownFiles(t, f.dir)[0]
				if err := os.Rename(own, own+"-moved"); err != nil {
					t.Error(err)
				}
				if !c.moved {
					if err := os.Remove(own + "-moved"); err != nil {
						t.Error(err)
					}
				}
				if c.another {
					put(t, filepath.Dir(own), filepath.Base(own), "a person's")
				}
				if c.there == "another's" {
					put(t, f.dir, c.path, c.there)
				}
				after = f.tree(t)
			}})
			version := ""
			if c.path == file {
				version = versionOf("base")
			}
			f.log.take()
			ans := do(t, f.own, writeOf(c.path, "new", version))
			if !done() || ans.Refused != "the file changed while it was written" || (ans.File == nil) != (c.there == "") || ans.File != nil && ans.File.Text != c.there {
				t.Fatalf("answered %q with %+v", ans.Refused, ans.File)
			}
			if now := f.tree(t); now != after {
				t.Errorf("there is\n%s\nand was\n%s", now, after)
			}
			log := f.log.take()
			if strings.Contains(log, "it stays") != c.another || c.another && !strings.Contains(log, "path=notes/"+filepath.Base(own)) {
				t.Errorf("logged %s", log)
			}
			if n := f.recorded(t); n != 0 {
				t.Errorf("%d writes are recorded", n)
			}
		})
	}
}

// A directory on the way that a person makes in the moment of the write is
// the person's: a write that fails then does not take it back.
func TestFilesDirectoryMadeMeanwhile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes everywhere")
	}
	for _, c := range []struct {
		step string
		nth  int
		path string
	}{
		{"making", 1, "notes/made"},
		{"making", 2, "notes/made/deeper"},
		{"entering", 3, "notes/made"}, // after what was there was recorded, and before it is made
	} {
		t.Run(c.step+" "+c.path, func(t *testing.T) {
			f := newFilesFixture(t)
			put(t, f.dir, "notes/plan.md", "plan")
			at := filepath.Join(f.dir, c.path)
			done := f.at(t, swap{c.step, c.nth, func(t *testing.T, _ *filesFixture) {
				if err := os.Mkdir(at, 0o500); err != nil { // nothing can be written in it
					t.Error(err)
				}
			}})
			t.Cleanup(func() { os.Chmod(at, 0o700) })
			ans, err := f.own.Files(context.Background(), writeOf("notes/made/deeper/new.md", "x", ""))
			if err == nil || !done() {
				t.Fatalf("answered %+v", ans)
			}
			if st, err := os.Stat(at); err != nil || !st.IsDir() {
				t.Fatalf("the person's directory: %v", err)
			}
			if n := f.recorded(t); n != 0 {
				t.Fatalf("%d writes are recorded", n)
			}
		})
	}
}

// Nor is an empty one that the daemon may not enter, when it was made after
// what was there was recorded, nor one that it is in.
func TestFilesDirectoryClosedMeanwhile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root enters everywhere")
	}
	for _, path := range []string{"notes/made", "notes/made/deeper"} {
		t.Run(path, func(t *testing.T) {
			f := newFilesFixture(t)
			put(t, f.dir, "notes/plan.md", "plan")
			at := filepath.Join(f.dir, path)
			done := f.at(t, swap{"entering", 3, func(t *testing.T, _ *filesFixture) {
				if err := errors.Join(os.MkdirAll(filepath.Dir(at), 0o755), os.Mkdir(at, 0)); err != nil {
					t.Error(err)
				}
			}})
			ans, err := f.own.Files(context.Background(), writeOf(path+"/new.md", "x", ""))
			if err == nil || !done() || f.recorded(t) != 0 {
				t.Fatalf("answered %+v; %d recorded", ans, f.recorded(t))
			}
			if st, err := os.Lstat(at); err != nil || !st.IsDir() {
				t.Errorf("the person's directory: %v", err)
			}
		})
	}
}

// So it is when the write is settled only later, since the daemon died, or its
// own file or the record of the write could not be removed.
func TestFilesDirectoryMadeMeanwhileLater(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes everywhere")
	}
	for _, c := range []struct {
		keeps, step string
		nth         int
	}{
		{"own file", "making", 1},
		{"record", "making", 1},
		{"record", "entering", 3}, // after what was there was recorded, and before it is made
	} {
		t.Run(c.keeps+" "+c.step, func(t *testing.T) {
			f := newFilesFixture(t)
			put(t, f.dir, "notes/plan.md", "plan")
			at, deeper := filepath.Join(f.dir, "notes/made"), filepath.Join(f.dir, "notes/made/deeper")
			n := 0
			f.files.Hook = func(step string) {
				if step == c.step {
					if n++; n == c.nth {
						if err := os.Mkdir(at, 0o700); err != nil {
							t.Error(err)
						}
					}
				}
				switch step {
				case "made":
					var there int // for a daemon that dies here
					if err := f.files.Store.DB.QueryRow(`SELECT there FROM supervisor_writes`).Scan(&there); err != nil || there != 2 {
						t.Errorf("recorded that %d of the directories were there, %v", there, err)
					}
				case "written":
					put(t, f.dir, "notes/made/deeper/new.md", "a person's")
					if c.keeps == "record" {
						f.keepsRecords(t)
					} else if err := os.Chmod(deeper, 0o500); err != nil {
						t.Error(err)
					}
				}
			}
			t.Cleanup(func() { os.Chmod(deeper, 0o700) })
			ans := do(t, f.own, writeOf("notes/made/deeper/new.md", "new", ""))
			if own := ownFiles(t, f.dir); ans.Refused == "" || f.recorded(t) != 1 || (len(own) == 1) != (c.keeps == "own file") {
				t.Fatalf("answered %+v; own files %v, %d recorded", ans, own, f.recorded(t))
			}
			f.files.Hook = nil
			f.losesRecords(t)
			if err := errors.Join(os.Chmod(deeper, 0o700), os.Remove(filepath.Join(deeper, "new.md")), f.files.Sweep(context.Background())); err != nil {
				t.Fatal(err)
			}
			if own := f.tree(t); f.recorded(t) != 0 || strings.Contains(own, "deeper") || strings.Contains(own, tmpPrefix) {
				t.Errorf("%d recorded; there is\n%s", f.recorded(t), own)
			}
			if st, err := os.Stat(at); err != nil || !st.IsDir() {
				t.Errorf("the person's directory: %v", err)
			}
		})
	}
}

// keepsRecords has no recorded write forgotten, until losesRecords.
func (f *filesFixture) keepsRecords(t *testing.T) {
	t.Helper()
	f.sql(t, `CREATE TRIGGER keeps BEFORE DELETE ON supervisor_writes BEGIN SELECT RAISE(ABORT, 'kept'); END`)
}

func (f *filesFixture) losesRecords(t *testing.T) {
	t.Helper()
	f.sql(t, `DROP TRIGGER IF EXISTS keeps`)
}

// A write that was refused is named in no line, also where the daemon's own
// file or the record of it could not be removed then, so that only what is
// there would tell later.
func TestFilesRefusedIsNotNamed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes everywhere")
	}
	ctx := context.Background()
	for _, c := range []struct{ was, keeps string }{{"", "own file"}, {"", "record"}, {"old", "own file"}, {"old", "record"}} {
		t.Run(fmt.Sprintf("%q %s", c.was, c.keeps), func(t *testing.T) {
			f := newFilesFixture(t)
			version := ""
			if c.was != "" {
				put(t, f.dir, "AGENTS.md", c.was)
				version = versionOf(c.was)
			}
			done := f.at(t, swap{"written", 1, func(t *testing.T, f *filesFixture) {
				put(t, f.dir, "AGENTS.md", "a person's")
				if c.keeps == "record" {
					f.keepsRecords(t)
				} else if err := os.Chmod(f.dir, 0o500); err != nil {
					t.Error(err)
				}
			}})
			t.Cleanup(func() { os.Chmod(f.dir, 0o700) })
			ans := do(t, f.own, writeOf("AGENTS.md", "new", version))
			if !done() || ans.Refused == "" || f.recorded(t) != 1 {
				t.Fatalf("answered %+v; %d recorded", ans, f.recorded(t))
			}
			f.losesRecords(t)
			if err := errors.Join(os.Chmod(f.dir, 0o700), f.files.Ended(ctx, "c1", filesOwn, 1), f.files.Sweep(ctx)); err != nil {
				t.Fatal(err)
			}
			if turns, err := f.files.Untold(ctx, "c1"); err != nil || len(turns) != 0 {
				t.Errorf("to tell: %+v, %v", turns, err)
			}
			if own := ownFiles(t, f.dir); f.recorded(t) != 0 || len(own) != 0 || holds(t, f.dir, "AGENTS.md") != "a person's" {
				t.Errorf("%d recorded, own files %v, the file holds %q", f.recorded(t), own, holds(t, f.dir, "AGENTS.md"))
			}
		})
	}
}

// One that was made, and answered as made, is named, whatever is there by the
// time it is settled.
func TestFilesMadeIsNamed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes everywhere")
	}
	ctx := context.Background()
	for _, c := range []struct{ path, was, keeps string }{
		{"AGENTS.md", "", "own file"}, {"AGENTS.md", "", "record"}, {"AGENTS.md", "old", "record"},
		{"agents.md", "", "own file"}, // one file where case does not count, made so here
	} {
		t.Run(fmt.Sprintf("%s %q %s", c.path, c.was, c.keeps), func(t *testing.T) {
			f := newFilesFixture(t)
			version := ""
			if c.was != "" {
				put(t, f.dir, c.path, c.was)
				version = versionOf(c.was)
			}
			done := f.at(t, swap{"placed", 1, func(t *testing.T, f *filesFixture) {
				if err := os.Link(filepath.Join(f.dir, c.path), filepath.Join(f.dir, "AGENTS.md")); err != nil && !errors.Is(err, fs.ErrExist) {
					t.Error(err)
				}
				if c.keeps == "record" {
					f.sql(t, `CREATE TRIGGER open BEFORE UPDATE OF done ON supervisor_writes BEGIN SELECT RAISE(ABORT, 'open'); END`)
				} else if err := os.Chmod(f.dir, 0o500); err != nil {
					t.Error(err)
				}
			}})
			t.Cleanup(func() { os.Chmod(f.dir, 0o700) })
			made(t, f.own, writeOf(c.path, "new", version))
			if !done() || f.recorded(t) != 1 || (len(ownFiles(t, f.dir)) == 1) != (c.keeps == "own file") {
				t.Fatalf("%d recorded, own files %v", f.recorded(t), ownFiles(t, f.dir))
			}
			f.sql(t, `DROP TRIGGER IF EXISTS open`)
			err := os.Chmod(f.dir, 0o700)
			if c.was == "" {
				os.Remove(filepath.Join(f.dir, "agents.md"))
				os.Remove(filepath.Join(f.dir, "AGENTS.md"))
			} else {
				put(t, f.dir, "AGENTS.md", c.was)
			}
			if err := errors.Join(err, f.files.Ended(ctx, "c1", filesOwn, 1), f.files.Sweep(ctx)); err != nil {
				t.Fatal(err)
			}
			if turns, err := f.files.Untold(ctx, "c1"); err != nil || len(turns) != 1 || !slices.Equal(turns[0].Names, []string{"AGENTS.md"}) {
				t.Errorf("to tell: %+v, %v", turns, err)
			}
			if own := ownFiles(t, f.dir); f.recorded(t) != 1 || len(own) != 0 {
				t.Errorf("%d recorded, own files %v", f.recorded(t), own)
			}
		})
	}
}

// What someone puts at the name that the daemon's own file had until it got
// its place is theirs; and a replaced program can be run before the
// directory is synced.
func TestFilesPlaced(t *testing.T) {
	for _, was := range []string{"", "#!/bin/sh\necho old\n"} {
		t.Run(fmt.Sprintf("%q", was), func(t *testing.T) {
			f := newFilesFixture(t)
			at, version, own := filepath.Join(f.dir, "run"), "", ""
			if was != "" {
				put(t, f.dir, "run", was)
				if err := os.Chmod(at, 0o700); err != nil {
					t.Fatal(err)
				}
				version = versionOf(was)
			}
			f.files.Hook = func(step string) {
				switch step {
				case "written":
					own = ownFiles(t, f.dir)[0]
				case "placed":
					if was == "" {
						if err := os.Remove(own); err != nil {
							t.Error(err)
						}
					}
					put(t, filepath.Dir(own), filepath.Base(own), "a person's")
				case "dropped":
					if was == "" {
						return
					}
					if out, err := osexec.Command(at).CombinedOutput(); err != nil || string(out) != "new\n" {
						t.Errorf("run: %q, %v", out, err)
					}
				}
			}
			f.log.take()
			made(t, f.own, writeOf("run", "#!/bin/sh\necho new\n", version))
			if has := holds(t, filepath.Dir(own), filepath.Base(own)); has != "a person's" || f.recorded(t) != 0 {
				t.Errorf("at the daemon's own name is %q; %d recorded", has, f.recorded(t))
			}
			if !strings.Contains(f.log.take(), notItsOwn) {
				t.Error("not logged")
			}
		})
	}
}

// A rename to another name of the daemon's own file, which someone gave it,
// does nothing: its own name goes all the same.
func TestFilesPlacedAlready(t *testing.T) {
	f := newFilesFixture(t)
	put(t, f.dir, "plan.md", "old")
	done := f.at(t, swap{"placing", 1, func(t *testing.T, f *filesFixture) {
		at := filepath.Join(f.dir, "plan.md")
		if err := errors.Join(os.Remove(at), os.Link(ownFiles(t, f.dir)[0], at)); err != nil {
			t.Error(err)
		}
	}})
	made(t, f.own, writeOf("plan.md", "new", versionOf("old")))
	if own := ownFiles(t, f.dir); !done() || len(own) != 0 || f.recorded(t) != 0 || holds(t, f.dir, "plan.md") != "new" {
		t.Errorf("own files %v, %d recorded, the file holds %q", own, f.recorded(t), holds(t, f.dir, "plan.md"))
	}
}

// killedAt, in the environment of this test's own process, is the step at
// which it is killed, the root of its fixture and the request's number.
const killedAt = "HATAORI_TEST_FILES_KILLED"

// What a killed process writes, and what stood there before.
var killedWrites = []struct {
	name   string
	r      control.FilesRequest
	before map[string]string
	tells  string // the user's file that it writes
}{
	{"replacing a file", writeOf("notes/plan.md", "new", versionOf("old")), map[string]string{"notes/plan.md": "old"}, ""},
	{"an edit of a user's file", editOf("AGENTS.md", "old", "new"), map[string]string{"AGENTS.md": "old"}, "AGENTS.md"},
	{"a new file", writeOf("notes/new.md", "new", ""), map[string]string{"notes/plan.md": "old"}, ""},
	{"a new file in new directories", writeOf("made/deeper/new.md", "new", ""), nil, ""},
	{"a new user's file", writeOf("SOUL.md", "new", ""), map[string]string{"AGENTS.md": "old"}, "SOUL.md"},
}

// A daemon killed between a write and its rename leaves the old file whole,
// and no file or directory of its own after its next start; one killed right
// after has made the write, and its turn is told of it.
func TestFilesKilled(t *testing.T) {
	if at := os.Getenv(killedAt); at != "" {
		var i int
		var step, root string
		if _, err := fmt.Sscanf(at, "%d %s %s", &i, &step, &root); err != nil {
			t.Fatal(err)
		}
		f := openFilesFixture(t, root)
		f.files.Hook = func(at string) {
			if at == step {
				syscall.Kill(os.Getpid(), syscall.SIGKILL)
				select {}
			}
		}
		ans, err := f.own.Files(context.Background(), killedWrites[i].r)
		t.Fatalf("not killed: %+v, %v", ans, err)
	}
	for i, c := range killedWrites {
		for _, step := range []string{"made", "written", "placing", "placed", "dropped"} {
			placed := step == "placed" || step == "dropped"
			t.Run(c.name+", "+step, func(t *testing.T) {
				f := newFilesFixture(t)
				for name, text := range c.before {
					put(t, f.dir, name, text)
				}
				before := f.tree(t)
				cmd := osexec.Command(os.Args[0], "-test.run=^TestFilesKilled$")
				cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d %s %s", killedAt, i, step, f.root))
				out, err := cmd.CombinedOutput()
				var exit *osexec.ExitError
				if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
					t.Fatalf("the process ended with %v: %s", err, out)
				}
				for name, text := range c.before {
					want := text
					if placed && name == c.r.Path {
						want = "new"
					}
					if holds(t, f.dir, name) != want {
						t.Fatalf("killed, %s holds %q, want %q", name, holds(t, f.dir, name), want)
					}
				}
				// The process did die in the middle of it.
				gone := step == "dropped" || placed && c.before[c.r.Path] != ""
				if own := ownFiles(t, f.dir); (len(own) == 0) != gone || len(own) > 1 {
					t.Fatalf("killed, its own files are %v", own)
				}
				if f.recorded(t) != 1 {
					t.Fatalf("killed, %d writes are recorded", f.recorded(t))
				}

				// The next start.
				if err := f.files.Sweep(context.Background()); err != nil {
					t.Fatal(err)
				}
				if own := ownFiles(t, f.root); len(own) != 0 {
					t.Fatalf("started again, own files are left: %v", own)
				}
				if !placed {
					if after := f.tree(t); after != before {
						t.Fatalf("started again, there is\n%s\nand was before the write\n%s", after, before)
					}
				} else if holds(t, f.dir, c.r.Path) != "new" {
					t.Fatalf("started again, the file holds %q", holds(t, f.dir, c.r.Path))
				}
				want := ""
				if placed {
					want = c.tells
				}
				if err := f.files.Ended(context.Background(), "c1", filesOwn, 1); err != nil {
					t.Fatal(err)
				}
				turns, err := f.files.Untold(context.Background(), "c1")
				if err != nil {
					t.Fatal(err)
				}
				var told string
				for _, turn := range turns {
					told += strings.Join(turn.Names, " ")
				}
				if told != want {
					t.Fatalf("the turn is told of %q, want %q", told, want)
				}
				if want == "" && f.recorded(t) != 0 {
					t.Fatalf("started again, %d writes are recorded", f.recorded(t))
				}
			})
		}
	}
}

// At a start, only the daemon's own file goes, by its recorded name, where
// it is a file. What is recorded of a scope's directory that is not there
// stays recorded, and is settled at a start that finds it.
func TestFilesSweepRemovesItsOwnOnly(t *testing.T) {
	ctx := context.Background()
	f := newFilesFixture(t)
	record := func(dir, path, tmp, was string, there int) {
		t.Helper()
		f.sql(t, fmt.Sprintf(`INSERT INTO supervisor_writes (scope, thread, dir, path, tmp, was, version, there, pos) VALUES ('c1', %d, '%s', '%s', '%s', '%s', '%s', %d, 0)`,
			filesOwn, dir, path, tmp, was, versionOf("the daemon's"), there))
	}
	put(t, f.dir, "notes/plan.md", "old")
	put(t, f.dir, "notes/.hataori-1", "the daemon's")
	put(t, f.dir, "notes/.hataori-7", "the dae")
	if err := os.Chmod(filepath.Join(f.dir, "notes/.hataori-7"), 0); err != nil { // not whole yet
		t.Fatal(err)
	}
	put(t, f.dir, "notes/.hataori-8", "a person's")
	put(t, f.dir, "notes/placed.md", "the daemon's, and changed since")
	if err := os.Link(filepath.Join(f.dir, "notes/placed.md"), filepath.Join(f.dir, "notes/.hataori-9")); err != nil {
		t.Fatal(err)
	}
	put(t, f.dir, "notes/.hataori-2", "not recorded")
	put(t, f.dir, "notes/.hataori-3/x", "a directory")
	put(t, f.dir, "notes/mine.md", "the daemon's") // the text, but no name of the daemon's
	put(t, f.dir, "full/new.md-beside", "a person's")
	put(t, f.dir, "file", "a person's")
	put(t, f.outside, ".hataori-6", "outside")
	for _, l := range [][2]string{{"plan.md", "notes/.hataori-4"}, {f.outside, "out"}} {
		if err := os.Symlink(l[0], filepath.Join(f.dir, l[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(f.dir, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(f.dir, "notes/.hataori-0"), 0); err != nil { // closed, but no file
		t.Fatal(err)
	}
	old := versionOf("old")
	record(f.dir, "notes/plan.md", ".hataori-1", old, 1)
	record(f.dir, "notes/plan.md", ".hataori-7", old, 1)
	record(f.dir, "notes/plan.md", ".hataori-8", old, 1)
	record(f.dir, "notes/placed.md", ".hataori-9", "", 1)
	record(f.dir, "notes/plan.md", ".hataori-3", old, 1)
	record(f.dir, "notes/plan.md", ".hataori-4", old, 1)
	record(f.dir, "notes/plan.md", ".hataori-0", old, 1)
	record(f.dir, "notes/plan.md", "mine.md", old, 1)
	record(f.dir, "full/new.md", ".hataori-5", "", 0)  // the directory is not empty any more
	record(f.dir, "file/new.md", ".hataori-5", "", 0)  // a file took its place
	record(f.dir, "empty/new.md", ".hataori-5", "", 1) // it was there before
	record(f.dir, "gone/deeper/new.md", ".hataori-5", "", 0)
	record(f.dir, "out/new.md", ".hataori-6", "", 0)
	record(filepath.Join(f.root, "gone"), "plan.md", ".hataori-1", old, 0)
	before := withoutStore(tree(t, f.root))
	if err := f.files.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, l := range strings.Split(before, "\n") {
		if !regexp.MustCompile(`/\.hataori-[179] `).MatchString(l) {
			want = append(want, l)
		}
	}
	log := f.log.take()
	if strings.Count(log, "it stays") != 5 || !strings.Contains(log, "notes/.hataori-8") {
		t.Errorf("logged %s", log)
	}
	if strings.Count(log, "may be where it was moved to") != 1 || !strings.Contains(log, "path=gone/deeper/new.md own=.hataori-5") {
		t.Errorf("logged of a directory that is not there: %s", log)
	}
	if strings.Count(log, "cannot be settled") != 3 || !strings.Contains(log, `level=ERROR msg="a supervisor's writes cannot be settled" dir=`+filepath.Join(f.root, "gone")) {
		t.Errorf("logged of what cannot be settled: %s", log)
	}
	if after := withoutStore(tree(t, f.root)); after != strings.Join(want, "\n") {
		t.Fatalf("there is\n%s\nand was\n%s", after, before)
	}
	// What lies behind a link cannot be looked at: it stays recorded.
	left := func(want string) {
		t.Helper()
		var left string
		if err := f.files.Store.DB.QueryRow(`SELECT coalesce(group_concat(path ORDER BY id), '') FROM supervisor_writes`).Scan(&left); err != nil || left != want {
			t.Fatalf("recorded: %s, %v, want %s", left, err, want)
		}
	}
	left("file/new.md,out/new.md,plan.md")
	put(t, f.root, "gone/plan.md", "old")
	put(t, f.root, "gone/.hataori-1", "the daemon's")
	if err := f.files.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if own := ownFiles(t, filepath.Join(f.root, "gone")); len(own) != 0 || holds(t, f.root, "gone/plan.md") != "old" {
		t.Fatalf("its directory is there again: own files %v, plan.md holds %q", own, holds(t, f.root, "gone/plan.md"))
	}
	left("file/new.md,out/new.md")
}

// Of a write that the daemon died in, with nothing of its own left, only
// what it was to replace tells whether it was made. It is told of when that
// is not there any more, whoever changed it. With its own file left it was
// not made, where it was to replace one; where it was to make one, and
// another is there, that one may have taken its place.
func TestFilesSweepTellsWhenInDoubt(t *testing.T) {
	const at = "AGENTS.md"
	person := func(t *testing.T, f *filesFixture) { put(t, f.dir, at, "a person's") }
	for _, c := range []struct {
		name, was string
		there     func(t *testing.T, f *filesFixture)
		told      bool
		own       string // "whole" or "half" for its own file; "" for none
	}{
		{"its own file, and another text", "old", person, false, "whole"},
		{"its own file, and nothing any more", "old", func(*testing.T, *filesFixture) {}, false, "whole"},
		{"its own file, and nothing, of a new file", "", func(*testing.T, *filesFixture) {}, false, "whole"},
		{"its own file, and another's, of a new file", "", person, true, "whole"},
		{"its own file, not whole, and another's, of a new file", "", person, false, "half"},
		{"what it was to replace", "old", func(t *testing.T, f *filesFixture) { put(t, f.dir, at, "old") }, false, ""},
		{"what it wrote", "old", func(t *testing.T, f *filesFixture) { put(t, f.dir, at, "new") }, true, ""},
		{"another text", "old", person, true, ""},
		{"nothing any more", "old", func(*testing.T, *filesFixture) {}, true, ""},
		{"no text", "old", func(t *testing.T, f *filesFixture) { put(t, f.dir, at, "\xff") }, true, ""},
		{"no file", "old", func(t *testing.T, f *filesFixture) {
			if err := syscall.Mkfifo(filepath.Join(f.dir, at), 0o600); err != nil {
				t.Fatal(err)
			}
		}, true, ""},
		{"nothing, of a new file", "", func(*testing.T, *filesFixture) {}, false, ""},
		{"a new file", "", func(t *testing.T, f *filesFixture) { put(t, f.dir, at, "new") }, true, ""},
		{"a file, another's", "", person, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFilesFixture(t)
			c.there(t, f)
			was := ""
			if c.was != "" {
				was = versionOf(c.was)
			}
			f.sql(t, fmt.Sprintf(`INSERT INTO supervisor_writes (scope, thread, dir, path, tmp, was, version, there, pos) VALUES ('c1', %d, '%s', '%s', '.hataori-1', '%s', '%s', 0, 0)`,
				filesConv, f.dir, at, was, versionOf("new")))
			before := f.tree(t)
			if c.own != "" {
				put(t, f.dir, ".hataori-1", "new")
			}
			if c.own == "half" {
				if err := os.Chmod(filepath.Join(f.dir, ".hataori-1"), 0); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.files.Sweep(ctx); err != nil {
				t.Fatal(err)
			}
			if err := f.files.Ended(ctx, "c1", filesConv, 1); err != nil {
				t.Fatal(err)
			}
			turns, err := f.files.Untold(ctx, "c1")
			if err != nil || (len(turns) == 1) != c.told || c.told && !slices.Equal(turns[0].Names, []string{at}) {
				t.Errorf("to tell: %+v, %v", turns, err)
			}
			if n := f.recorded(t); n != len(turns) || f.tree(t) != before {
				t.Errorf("%d recorded; there is\n%s\nand was\n%s", n, f.tree(t), before)
			}
		})
	}
}

// A turn whose lines were posted, but not recorded as that, before it was
// known all that it wrote has lines that name all of it then; and what is
// recorded as told has no more.
func TestFilesLineOfMore(t *testing.T) {
	ctx := context.Background()
	f := newFilesFixture(t)
	posted := map[string]bool{}
	var lines []string
	died := errors.New("died")
	var dies error = died
	r := &waking{sv: filesOwn, w: &Waker{Scope: "c1", Files: &Sessions{Files: f.files},
		Tell: func(_ context.Context, id control.ThreadID, key, text string) error {
			if k := fmt.Sprint(id, " ", key); !posted[k] { // as core posts a key once in a thread
				posted[k] = true
				lines = append(lines, fmt.Sprintf("%d: %s", id, text))
			}
			return nil
		},
		Hook: func(string) error { return dies },
	}}
	tell := func(want error, more ...string) {
		t.Helper()
		lines = nil
		if err := f.files.Ended(ctx, "c1", filesConv, 3); err != nil {
			t.Fatal(err)
		}
		if err := r.tellFiles(ctx); (err == nil) != (want == nil) {
			t.Fatalf("told: %v, want %v", err, want)
		}
		if !slices.Equal(lines, more) {
			t.Fatalf("posted %q, want %q", lines, more)
		}
	}
	both := func(names ...string) []string {
		return []string{fmt.Sprintf("%d: %s", filesConv, FilesNotice(names)), fmt.Sprintf("%d: %s", filesOwn, FilesNoticeOf(filesConv, names))}
	}
	made(t, f.conv, writeOf("AGENTS.md", "x", ""))
	tell(died, both("AGENTS.md")...)
	tell(died)
	put(t, f.dir, "SOUL.md", "x")
	f.sql(t, fmt.Sprintf(`INSERT INTO supervisor_writes (scope, thread, dir, path, tmp, was, version, there, pos) VALUES ('c1', %d, '%s', 'SOUL.md', '.hataori-1', '', '%s', 0, 0)`,
		filesConv, f.dir, versionOf("x")))
	if err := f.files.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	tell(died, both("AGENTS.md", "SOUL.md")...)
	dies = nil
	tell(nil)
	if n := f.recorded(t); n != 0 {
		t.Fatalf("%d recorded", n)
	}
	tell(nil)
	made(t, f.conv, writeOf("USER.md", "x", "")) // as one that was not settled before
	tell(nil, both("USER.md")...)
	if n := f.recorded(t); n != 0 {
		t.Fatalf("%d recorded", n)
	}
}

// A turn's end names the user's files that its thread wrote before it, once
// each and in their order, whatever the thread wrote besides; another
// thread's, another scope's and a later turn's are named with their own.
func TestFilesOfATurn(t *testing.T) {
	ctx := context.Background()
	f := newFilesFixture(t)
	events := func(n int) {
		t.Helper()
		for range n {
			f.sql(t, `INSERT INTO events (scope, thread, time, kind, data) VALUES ('c1', '5', 0, 'x', '{}')`)
		}
	}
	untold := func(scope string, want string) {
		t.Helper()
		turns, err := f.files.Untold(ctx, scope)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, turn := range turns {
			got = append(got, fmt.Sprintf("%d@%d %s", turn.Thread, turn.Pos, strings.Join(turn.Names, "+")))
		}
		if strings.Join(got, ", ") != want {
			t.Fatalf("to tell of %s: %s, want %s", scope, strings.Join(got, ", "), want)
		}
	}
	ended := func(scope string, thread control.ThreadID, pos uint64) {
		t.Helper()
		if err := f.files.Ended(ctx, scope, thread, pos); err != nil {
			t.Fatal(err)
		}
	}
	last, first := UserFiles[len(UserFiles)-1], UserFiles[0]
	made(t, f.own, writeOf("notes/plan.md", "x", ""))
	made(t, f.own, writeOf("notes/"+first, "x", ""))
	if f.recorded(t) != 0 {
		t.Fatalf("%d writes of other files are recorded", f.recorded(t))
	}
	ended("c1", filesOwn, 1)
	untold("c1", "")
	made(t, f.own, writeOf(last, "1", ""))
	made(t, f.own, writeOf(first, "1", ""))
	made(t, f.own, editOf(last, "1", "2"))
	f.misfit(t, f.own, editOf(UserFiles[0], "nowhere", "2"), "the passage is not in the file", "1")
	f.refused(t, f.own, writeOf(UserFiles[1], strings.Repeat("x", UserFileMax+1), ""), "one of the user's files over 32 KB")
	made(t, f.own, writeOf(first, "1", versionOf("1"))) // what is there already
	made(t, f.conv, writeOf(UserFiles[1], "1", ""))
	made(t, f.other, writeOf(first, "1", ""))
	untold("c1", "") // nothing ended
	events(5)
	made(t, f.own, writeOf(UserFiles[1], "2", versionOf("1"))) // at 5
	ended("c1", filesOwn, 5)
	untold("c1", fmt.Sprintf("5@5 %s+%s", first, last))
	untold("c2", "")
	ended("c1", filesOwn, 7)
	ended("c1", filesOwn, 8) // a turn that wrote none
	ended("c2", filesOther, 6)
	ended("c2", filesOwn, 9) // none of c2's
	ended("c1", filesConv, 6)
	both := fmt.Sprintf("5@5 %s+%s, 8@6 %s, 5@7 %s", first, last, UserFiles[1], UserFiles[1])
	untold("c1", both)
	untold("c2", "20@6 "+first)
	named := func(thread control.ThreadID, pos uint64, names ...string) {
		t.Helper()
		if err := f.files.Named(ctx, "c1", FilesTurn{Thread: thread, Pos: pos, Names: names}); err != nil {
			t.Fatal(err)
		}
	}
	named(filesOwn, 6, UserFiles...) // none of these
	named(filesConv, 5, UserFiles...)
	named(filesOther, 6, UserFiles...)
	named(filesOwn, 5, UserFiles[1])
	untold("c1", both)
	untold("c2", "20@6 "+first)
	named(filesOwn, 5, last)
	untold("c1", fmt.Sprintf("5@5 %s, 8@6 %s, 5@7 %s", first, UserFiles[1], UserFiles[1]))
	named(filesOwn, 5, first)
	untold("c1", fmt.Sprintf("8@6 %s, 5@7 %s", UserFiles[1], UserFiles[1]))
	ended("c1", filesOwn, 20)
	untold("c1", fmt.Sprintf("8@6 %s, 5@7 %s", UserFiles[1], UserFiles[1])) // not again
	ended("c3", filesOwn, 20)                                               // no such scope
	untold("c3", "")
	// A write that is not settled is none yet, and told of once it is, with
	// the turn that made it.
	f.sql(t, fmt.Sprintf(`INSERT INTO supervisor_writes (scope, thread, dir, path, tmp, was, version, there, pos) VALUES ('c1', %d, '%s', '%s', '.hataori-1', '', '%s', 0, 0)`,
		filesOwn, f.dir, first, versionOf("1")))
	ended("c1", filesOwn, 21)
	untold("c1", fmt.Sprintf("8@6 %s, 5@7 %s", UserFiles[1], UserFiles[1]))
	named(filesOwn, 21, append([]string{""}, UserFiles...)...)
	if err := f.files.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	untold("c1", fmt.Sprintf("8@6 %s, 5@7 %s, 5@21 %s", UserFiles[1], UserFiles[1], first))
	ended("c1", filesOwn, 22)
	untold("c1", fmt.Sprintf("8@6 %s, 5@7 %s, 5@21 %s", UserFiles[1], UserFiles[1], first))
}

// The end of a turn waits for the write that is being made, which is one of
// that turn.
func TestFilesTurnEndWaitsForTheWrite(t *testing.T) {
	ctx := context.Background()
	f := newFilesFixture(t)
	held, release := make(chan struct{}), make(chan struct{})
	f.files.Hook = func(at string) {
		if at == "placed" {
			close(held)
			<-release
		}
	}
	wrote, ended := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := f.own.Files(ctx, writeOf(UserFiles[0], "new", ""))
		wrote <- err
	}()
	<-held
	go func() { ended <- f.files.Ended(ctx, "c1", filesOwn, 3) }()
	select {
	case err := <-ended:
		t.Fatalf("the turn ended in the middle of the write: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := errors.Join(<-wrote, <-ended); err != nil {
		t.Fatal(err)
	}
	if turns, err := f.files.Untold(ctx, "c1"); err != nil || len(turns) != 1 || len(turns[0].Names) != 1 || turns[0].Names[0] != UserFiles[0] {
		t.Fatalf("to tell: %+v, %v", turns, err)
	}
}

// A caller that is gone in the middle of a write does not leave it half made
// or untold.
func TestFilesCallerGone(t *testing.T) {
	for _, step := range []string{"written", "placed"} {
		f := newFilesFixture(t)
		put(t, f.dir, UserFiles[0], "old")
		ctx, cancel := context.WithCancel(context.Background())
		f.files.Hook = func(at string) {
			if at == step {
				cancel()
			}
		}
		if ans, err := f.own.Files(ctx, writeOf(UserFiles[0], "new", versionOf("old"))); err != nil || ans.Refused != "" {
			t.Fatalf("at %s: %+v, %v", step, ans, err)
		}
		if holds(t, f.dir, UserFiles[0]) != "new" || len(ownFiles(t, f.root)) != 0 {
			t.Fatalf("at %s: the file holds %q; own files %v", step, holds(t, f.dir, UserFiles[0]), ownFiles(t, f.root))
		}
		if err := f.files.Ended(context.Background(), "c1", filesOwn, 1); err != nil {
			t.Fatal(err)
		}
		if turns, err := f.files.Untold(context.Background(), "c1"); err != nil || len(turns) != 1 {
			t.Fatalf("at %s: to tell: %+v, %v", step, turns, err)
		}
	}
}
