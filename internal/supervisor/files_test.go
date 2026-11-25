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
	for i := range listMax {
		put(t, f.dir, fmt.Sprintf("many/%d", i), "")
	}
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
	// A socket's path is short on every system: bound by a relative name.
	sock, err := os.MkdirTemp("", "s")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sock)
	if l, err := net.Listen("unix", filepath.Join(sock, "s")); err != nil {
		t.Fatal(err)
	} else {
		defer l.Close()
		if err := os.Link(filepath.Join(sock, "s"), filepath.Join(f.dir, "sock")); err != nil {
			t.Logf("no socket in the directory: %v", err)
		}
	}
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
		{" plan.md", hidden}, {"plan.md ", hidden}, {"notes /secret.md", hidden}, {"notes/ secret.md", hidden}, {"\u200b/secret.md", hidden},
		{"notes", regular}, {"pipe", regular}, {"file-link", regular}, {"out-file-link", regular}, {"gone-link", regular}, {"dir-link", regular}, {"out-link", regular}, {"notes/self", regular},
		{"dir-link/secret.md", link}, {"out-link/secret.md", link}, {"out-link/sub/secret.md", link}, {"out-link/new/secret.md", link}, {"other-link/secret.md", link},
		{"docs-link/secret.md", link}, {"notes/self/secret.md", link}, {"gone-link/secret.md", link}, {"plan.md/secret.md", link}, {"plan.md/new/secret.md", link}, {"pipe/secret.md", link},
	} {
		f.refused(t, f.own, readOf(c.path), c.why)
		f.refused(t, f.own, writeOf(c.path, "changed", ""), c.why)
		f.refused(t, f.own, writeOf(c.path, "changed", versionOf(secret)), c.why)
		f.refused(t, f.own, editOf(c.path, secret, "changed"), c.why)
	}
	if _, err := os.Lstat(filepath.Join(f.dir, "sock")); err == nil {
		f.refused(t, f.own, readOf("sock"), regular)
		f.refused(t, f.own, writeOf("sock", "changed", ""), regular)
	}
	// docs/ and what is named as it where case does not count, as a file, and
	// in a directory that would have to be made. ſ is another case of s.
	for _, path := range []string{"docs/secret.md", "docs/new.md", "docs/new/new.md", "docs", "DOCS/secret.md", "Docs/new.md", "dOcS/new/new.md", "DOCS", "doc\u017f/new.md"} {
		f.refused(t, f.own, writeOf(path, "changed", ""), docs)
		f.refused(t, f.own, writeOf(path, "changed", versionOf(secret)), docs)
		f.refused(t, f.own, editOf(path, secret, "changed"), docs)
	}
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
		(30, 'c1', 'k30', 'p', 'running', 1, 'conversation', '/elsewhere', 0, 0), (31, 'c1', 'k31', 'p', 'running', 1, 'conversation', '', 0, 0)`)
	all(f.files.API(nil, "c1", 30), not)         // it works elsewhere
	all(f.files.API(nil, "c1", 31), not)         // nowhere yet
	all(f.files.API(nil, "c1", filesOther), not) // a thread of another scope
	all(f.files.API(nil, "c1", 99), not)         // no thread
	all(f.files.API(nil, "c3", filesOwn), "the channel's supervisor has no directory yet")

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
		put(t, f.dir, fmt.Sprintf("notes/%d/x", i), "")
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

// What a write cannot take back, as where its directory may not be written
// any more, stays recorded and is taken back by the next write.
func TestFilesTakenBackLater(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes everywhere")
	}
	f := newFilesFixture(t)
	put(t, f.dir, "notes/plan.md", "base")
	notes := filepath.Join(f.dir, "notes")
	done := f.at(t, swap{"written", 1, func(t *testing.T, _ *filesFixture) { os.Chmod(notes, 0o500) }})
	t.Cleanup(func() { os.Chmod(notes, 0o700) })
	if ans, err := f.own.Files(context.Background(), writeOf("notes/plan.md", "new", versionOf("base"))); err == nil {
		t.Fatalf("answered %+v", ans)
	}
	if !done() || len(ownFiles(t, f.dir)) != 1 || f.recorded(t) != 1 || holds(t, f.dir, "notes/plan.md") != "base" {
		t.Fatalf("own files %v, %d recorded, the file holds %q", ownFiles(t, f.dir), f.recorded(t), holds(t, f.dir, "notes/plan.md"))
	}
	if !strings.Contains(f.log.take(), "is not settled") {
		t.Error("not logged")
	}
	made(t, f.conv, writeOf("other.md", "x", "")) // it cannot yet
	if len(ownFiles(t, f.dir)) != 1 || f.recorded(t) != 1 || !strings.Contains(f.log.take(), "cannot be settled") {
		t.Fatalf("own files %v, %d recorded", ownFiles(t, f.dir), f.recorded(t))
	}
	if err := os.Chmod(notes, 0o700); err != nil {
		t.Fatal(err)
	}
	made(t, f.other, writeOf("other.md", "x", "")) // another directory's
	if f.recorded(t) != 1 {
		t.Fatal("a write of another directory settled it")
	}
	if ans := do(t, f.conv, writeOf("other.md", "y", "")); ans.Refused == "" { // any write does it
		t.Fatalf("answered %+v", ans)
	}
	if own := ownFiles(t, f.dir); len(own) != 0 || f.recorded(t) != 0 {
		t.Fatalf("own files %v, %d recorded", own, f.recorded(t))
	}
}

// A directory on the way that a person makes in the moment of the write is
// the person's: a write that fails then does not take it back.
func TestFilesDirectoryMadeMeanwhile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes everywhere")
	}
	for nth, path := range map[int]string{1: "made", 2: "made/deeper"} {
		t.Run(path, func(t *testing.T) {
			f := newFilesFixture(t)
			at := filepath.Join(f.dir, path)
			done := f.at(t, swap{"making", nth, func(t *testing.T, _ *filesFixture) {
				if err := os.Mkdir(at, 0o500); err != nil { // nothing can be written in it
					t.Error(err)
				}
			}})
			t.Cleanup(func() { os.Chmod(at, 0o700) })
			ans, err := f.own.Files(context.Background(), writeOf("made/deeper/new.md", "x", ""))
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
		for _, step := range []string{"made", "written", "placing", "placed"} {
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
					if step == "placed" && name == c.r.Path {
						want = "new"
					}
					if holds(t, f.dir, name) != want {
						t.Fatalf("killed, %s holds %q, want %q", name, holds(t, f.dir, name), want)
					}
				}
				// The process did die in the middle of it.
				replaced := step == "placed" && c.before[c.r.Path] != ""
				if own := ownFiles(t, f.dir); len(own) != 1 && !replaced || replaced && len(own) != 0 {
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
				if step != "placed" {
					if after := f.tree(t); after != before {
						t.Fatalf("started again, there is\n%s\nand was before the write\n%s", after, before)
					}
				} else if holds(t, f.dir, c.r.Path) != "new" {
					t.Fatalf("started again, the file holds %q", holds(t, f.dir, c.r.Path))
				}
				want := ""
				if step == "placed" {
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
// it is a file; and what is recorded of a directory that is gone is
// forgotten.
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
	put(t, f.dir, "notes/mine.md", "no name of the daemon's")
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
	old := versionOf("old")
	record(f.dir, "notes/plan.md", ".hataori-1", old, 1)
	record(f.dir, "notes/plan.md", ".hataori-7", old, 1)
	record(f.dir, "notes/plan.md", ".hataori-8", old, 1)
	record(f.dir, "notes/placed.md", ".hataori-9", "", 1)
	record(f.dir, "notes/plan.md", ".hataori-3", old, 1)
	record(f.dir, "notes/plan.md", ".hataori-4", old, 1)
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
	if log := f.log.take(); strings.Count(log, "it stays") != 4 || !strings.Contains(log, "notes/.hataori-8") {
		t.Errorf("logged %s", log)
	}
	if after := withoutStore(tree(t, f.root)); after != strings.Join(want, "\n") {
		t.Fatalf("there is\n%s\nand was\n%s", after, before)
	}
	// What lies behind a link cannot be looked at: it stays recorded.
	var left string
	if err := f.files.Store.DB.QueryRow(`SELECT coalesce(group_concat(path), '') FROM supervisor_writes`).Scan(&left); err != nil || left != "file/new.md,out/new.md" {
		t.Fatalf("recorded: %s, %v", left, err)
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
	for _, other := range []FilesTurn{{Thread: filesOwn, Pos: 6}, {Thread: filesConv, Pos: 5}, {Thread: filesOther, Pos: 6}} {
		if err := f.files.Named(ctx, "c1", other); err != nil { // none of these
			t.Fatal(err)
		}
	}
	untold("c1", both)
	untold("c2", "20@6 "+first)
	if err := f.files.Named(ctx, "c1", FilesTurn{Thread: filesOwn, Pos: 5}); err != nil {
		t.Fatal(err)
	}
	untold("c1", fmt.Sprintf("8@6 %s, 5@7 %s", UserFiles[1], UserFiles[1]))
	ended("c1", filesOwn, 20)
	untold("c1", fmt.Sprintf("8@6 %s, 5@7 %s", UserFiles[1], UserFiles[1])) // not again
	ended("c3", filesOwn, 20)                                               // no such scope
	untold("c3", "")
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
