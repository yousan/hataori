package supervisor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/store"
)

// docsFixture is a store, a supervisor's directory and a directory of
// host files to list.
type docsFixture struct {
	docs           *Docs
	log            *bytes.Buffer
	dir, host, tmp string
}

func newDocsFixture(t *testing.T) *docsFixture {
	t.Helper()
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "state"), 0o700)
	st, err := store.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &docsFixture{log: &bytes.Buffer{}, dir: filepath.Join(root, "confined", "scope-c1"), host: t.TempDir()}
	os.MkdirAll(f.dir, 0o700)
	f.docs = &Docs{DB: st.DB, Log: slog.New(slog.NewTextHandler(f.log, nil))}
	return f
}

// file writes a host file and returns its path.
func (f *docsFixture) file(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(f.host, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// copy runs one Copy and returns the log lines it wrote.
func (f *docsFixture) copy(files ...string) []string {
	f.log.Reset()
	f.docs.Copy(context.Background(), f.dir, files)
	return strings.Split(strings.TrimSpace(f.log.String()), "\n")
}

// in reads name in docs/, "" when it is not there.
func (f *docsFixture) in(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, DocsDir, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// Each listed file is copied into docs/ under its base name, readable and
// not writable, and again with its new content after it changed; a copy
// whose file is no longer listed is removed.
func TestDocsCopyCurrentContent(t *testing.T) {
	f := newDocsFixture(t)
	design, rules := f.file(t, "design.md", "v1\n"), f.file(t, "rules.md", "r1\n")
	if lines := f.copy(design, rules); lines[0] != "" {
		t.Fatalf("log %q", lines)
	}
	if f.in(t, "design.md") != "v1\n" || f.in(t, "rules.md") != "r1\n" {
		t.Fatalf("docs/: %q, %q", f.in(t, "design.md"), f.in(t, "rules.md"))
	}
	st, err := os.Stat(filepath.Join(f.dir, DocsDir, "design.md"))
	if err != nil || st.Mode().Perm() != 0o444 {
		t.Fatalf("the copy's mode: %v, %v; want readable only", st, err)
	}
	f.file(t, "design.md", "v2, longer\n")
	f.copy(design, rules)
	if got := f.in(t, "design.md"); got != "v2, longer\n" {
		t.Fatalf("after a change: %q", got)
	}
	// rules.md no longer listed: its copy goes.
	f.copy(design)
	if _, err := os.Lstat(filepath.Join(f.dir, DocsDir, "rules.md")); !os.IsNotExist(err) {
		t.Fatalf("the copy of a file no longer listed: %v", err)
	}
	if got := f.names(t); got != "design.md +1" {
		t.Fatalf("docs/ holds %s", got)
	}
}

// A file in docs/ that the daemon did not write is left as it was, with
// one log line, and the listed file of its name is not copied.
func TestDocsLeaveAFileTheyDidNotWrite(t *testing.T) {
	f := newDocsFixture(t)
	os.MkdirAll(filepath.Join(f.dir, DocsDir), 0o700)
	byHand := filepath.Join(f.dir, DocsDir, "design.md")
	os.WriteFile(byHand, []byte("copied by hand\n"), 0o600)
	design := f.file(t, "design.md", "v1\n")
	lines := f.copy(design)
	if len(lines) != 1 || !strings.Contains(lines[0], "did not write") || !strings.Contains(lines[0], "design.md") {
		t.Fatalf("log %q; want one line", lines)
	}
	if got := f.in(t, "design.md"); got != "copied by hand\n" {
		t.Fatalf("the file by hand is now %q", got)
	}
	// The same content as the listed file does not make it the daemon's.
	os.WriteFile(design, []byte("copied by hand\n"), 0o644)
	f.copy(design)
	os.WriteFile(design, []byte("v3\n"), 0o644)
	f.copy(design)
	if got := f.in(t, "design.md"); got != "copied by hand\n" {
		t.Fatalf("the file by hand is now %q", got)
	}
	// Nor is it removed once the path is no longer listed.
	f.copy()
	if got := f.in(t, "design.md"); got != "copied by hand\n" {
		t.Fatalf("the file by hand after the list emptied: %q", got)
	}
}

// A copy of the daemon's own that was changed after it wrote it is left as
// it was, with one log line: not written over, and not removed once its
// file is no longer listed.
func TestDocsLeaveAChangedCopy(t *testing.T) {
	f := newDocsFixture(t)
	design := f.file(t, "design.md", "v1\n")
	f.copy(design)
	copied := filepath.Join(f.dir, DocsDir, "design.md")
	os.Chmod(copied, 0o644)
	os.WriteFile(copied, []byte("v1, and a person's line\n"), 0o644)
	f.file(t, "design.md", "v2\n")
	lines := f.copy(design)
	if len(lines) != 1 || !strings.Contains(lines[0], "changed since") {
		t.Fatalf("log %q; want one line", lines)
	}
	if got := f.in(t, "design.md"); got != "v1, and a person's line\n" {
		t.Fatalf("the changed copy is now %q", got)
	}
	lines = f.copy()
	if len(lines) != 1 || !strings.Contains(lines[0], "changed since") {
		t.Fatalf("log %q; want one line", lines)
	}
	if got := f.in(t, "design.md"); got != "v1, and a person's line\n" {
		t.Fatalf("the changed copy after its file was no longer listed: %q", got)
	}
}

// A missing path, a directory and a file larger than 1 MB are each skipped
// with one log line; the other listed files are copied.
func TestDocsSkipWhatIsNoFile(t *testing.T) {
	f := newDocsFixture(t)
	big := f.file(t, "big.md", strings.Repeat("x", DocMax+1))
	limit := f.file(t, "limit.md", strings.Repeat("y", DocMax))
	dir := filepath.Join(f.host, "adir")
	os.Mkdir(dir, 0o700)
	missing := filepath.Join(f.host, "missing.md")
	ok := f.file(t, "ok.md", "fine\n")
	lines := f.copy(missing, dir, big, limit, ok)
	if len(lines) != 3 {
		t.Fatalf("log %q; want three lines", lines)
	}
	for i, why := range []string{"missing", "not a regular file", "larger than 1 MB"} {
		if !strings.Contains(lines[i], why) {
			t.Fatalf("line %d %q; want %q", i, lines[i], why)
		}
	}
	if f.in(t, "ok.md") != "fine\n" || len(f.in(t, "limit.md")) != DocMax || f.in(t, "big.md") != "" || f.in(t, "adir") != "" {
		t.Fatal("the listed files that are files were not copied, or the others were")
	}
}

// A daemon that died after the new copy was put in place, before it was
// recorded, still takes it for its own; so does one that died before the
// link, for the old copy; and a write that then fails, as on a full disk,
// loses neither. The daemon's own links of the records before go.
func TestDocsAfterADaemonDied(t *testing.T) {
	ctx := context.Background()
	f := newDocsFixture(t)
	design := f.file(t, "design.md", "v1\n")
	f.copy(design)
	copied := filepath.Join(f.dir, DocsDir, "design.md")
	docsDir := filepath.Join(f.dir, DocsDir)
	// Died after the link of v2: the record has v1 in place, v2 pending;
	// v1 is aside.
	r := f.record(t, "design.md")
	v2 := f.written(t, "v2\n")
	r.pending, r.pkeep = &v2, filepath.Base(f.tmp)
	f.docs.record(ctx, f.dir, "design.md", r)
	aside := filepath.Join(docsDir, tmpPrefix+"aside")
	if err := os.Rename(copied, aside); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(f.tmp, copied); err != nil {
		t.Fatal(err)
	}
	// The next copy, of v3, fails to write.
	os.Chmod(docsDir, 0o500)
	f.file(t, "design.md", "v3\n")
	f.copy(design)
	os.Chmod(docsDir, 0o700)
	if got := f.in(t, "design.md"); got != "v2\n" {
		t.Fatalf("after a failed write: %q", got)
	}
	f.file(t, "design.md", "v4\n")
	if lines := f.copy(design); lines[0] != "" {
		t.Fatalf("log %q", lines)
	}
	if got := f.in(t, "design.md"); got != "v4\n" {
		t.Fatalf("after a pending copy in place and a failed write: %q", got)
	}
	os.Remove(aside) // what the daemon that died left
	if got := f.names(t); got != "design.md +1" {
		t.Fatalf("docs/ holds %s", got)
	}
	// Died before the link of v5: v4 in place, v5 pending.
	r = f.record(t, "design.md")
	v5 := f.written(t, "v5\n")
	r.pending, r.pkeep = &v5, filepath.Base(f.tmp)
	f.docs.record(ctx, f.dir, "design.md", r)
	f.file(t, "design.md", "v6\n")
	if lines := f.copy(design); lines[0] != "" {
		t.Fatalf("log %q", lines)
	}
	if got := f.in(t, "design.md"); got != "v6\n" {
		t.Fatalf("after a pending copy not in place: %q", got)
	}
	if got := f.names(t); got != "design.md +1" {
		t.Fatalf("docs/ holds %s", got)
	}
}

// A first copy recorded that never got in place, as when the daemon died
// first, does not make a file of the same content that a person put there
// the daemon's: it is neither written over nor removed.
func TestDocsPendingCopyNeverInPlace(t *testing.T) {
	ctx := context.Background()
	f := newDocsFixture(t)
	// What the daemon that died left: the record, and the copy not linked.
	v1 := f.written(t, "v1\n")
	f.docs.record(ctx, f.dir, "design.md", record{placed: v1, keep: filepath.Base(f.tmp)})
	byHand := filepath.Join(f.dir, DocsDir, "design.md")
	os.WriteFile(byHand, []byte("v1\n"), 0o444)
	design := f.file(t, "design.md", "v2\n")
	if lines := f.copy(design); len(lines) != 1 || !strings.Contains(lines[0], "did not write") {
		t.Fatalf("log %q; want one line", lines)
	}
	f.copy()
	if got := f.in(t, "design.md"); got != "v1\n" {
		t.Fatalf("the file by hand is now %q", got)
	}
}

// A copy that a person removed and put back by hand, with the same
// content, is theirs: the inode of the daemon's copy is not free for it.
func TestDocsCopyPutBackByHand(t *testing.T) {
	f := newDocsFixture(t)
	design := f.file(t, "design.md", "v1\n")
	f.copy(design)
	copied := filepath.Join(f.dir, DocsDir, "design.md")
	os.Remove(copied)
	os.WriteFile(copied, []byte("v1\n"), 0o444)
	f.file(t, "design.md", "v2\n")
	if lines := f.copy(design); len(lines) != 1 || !strings.Contains(lines[0], "did not write") {
		t.Fatalf("log %q; want one line", lines)
	}
	f.copy()
	if got := f.in(t, "design.md"); got != "v1\n" {
		t.Fatalf("the file by hand is now %q", got)
	}
}

// A copy of the daemon's made writable, as to write it, and one whose own
// link of the daemon's is gone, are not taken for the daemon's: neither
// is written over.
func TestDocsCopyNoLongerProvedOurs(t *testing.T) {
	f := newDocsFixture(t)
	design, rules := f.file(t, "design.md", "v1\n"), f.file(t, "rules.md", "r1\n")
	f.copy(design, rules)
	os.Chmod(filepath.Join(f.dir, DocsDir, "design.md"), 0o644)
	os.Remove(filepath.Join(f.dir, DocsDir, f.record(t, "rules.md").keep))
	f.file(t, "design.md", "v2\n")
	f.file(t, "rules.md", "r2\n")
	if lines := f.copy(design, rules); len(lines) != 2 {
		t.Fatalf("log %q; want two lines", lines)
	}
	if f.in(t, "design.md") != "v1\n" || f.in(t, "rules.md") != "r1\n" {
		t.Fatalf("docs/: %q, %q", f.in(t, "design.md"), f.in(t, "rules.md"))
	}
}

// A directory a person put where a copy was stays where it is once its
// file is no longer listed.
func TestDocsLeaveADirectory(t *testing.T) {
	f := newDocsFixture(t)
	f.copy(f.file(t, "x", "v1\n"))
	copied := filepath.Join(f.dir, DocsDir, "x")
	os.Remove(copied)
	os.Mkdir(copied, 0o700)
	if lines := f.copy(); len(lines) != 1 || !strings.Contains(lines[0], "changed since") {
		t.Fatalf("log %q; want one line", lines)
	}
	if st, err := os.Lstat(copied); err != nil || !st.IsDir() {
		t.Fatalf("the directory: %v, %v", st, err)
	}
	if got := f.names(t); got != "x +0" {
		t.Fatalf("docs/ holds %s", got)
	}
}

// Without files, and with no copy recorded, nothing is made or touched.
func TestDocsWithoutFilesChangeNothing(t *testing.T) {
	f := newDocsFixture(t)
	f.copy()
	if _, err := os.Lstat(filepath.Join(f.dir, DocsDir)); !os.IsNotExist(err) {
		t.Fatalf("docs/ without files: %v", err)
	}
	// A docs/ of the supervisor's own, from before, is left as it is.
	os.Mkdir(filepath.Join(f.dir, DocsDir), 0o700)
	os.WriteFile(filepath.Join(f.dir, DocsDir, "mine.md"), []byte("mine\n"), 0o600)
	f.copy()
	if f.in(t, "mine.md") != "mine\n" {
		t.Fatal("a file in docs/ changed without files")
	}
}

// docs/ that is a link, and a supervisor's directory that is one, are not
// followed: nothing is copied, with one log line.
func TestDocsFollowNoLink(t *testing.T) {
	f := newDocsFixture(t)
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(f.dir, DocsDir)); err != nil {
		t.Fatal(err)
	}
	lines := f.copy(f.file(t, "design.md", "v1\n"))
	if len(lines) != 1 || !strings.Contains(lines[0], "nothing copied") {
		t.Fatalf("log %q", lines)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("copied where docs/ points: %v", entries)
	}
}

// written writes content to a new file in docs/, as the daemon would its
// next copy, at f.tmp, and returns what identifies it.
func (f *docsFixture) written(t *testing.T, content string) copyID {
	t.Helper()
	os.MkdirAll(filepath.Join(f.dir, DocsDir), 0o700)
	f.tmp = filepath.Join(f.dir, DocsDir, tmpPrefix+"test")
	os.Remove(f.tmp)
	if err := os.WriteFile(f.tmp, []byte(content), 0o444); err != nil {
		t.Fatal(err)
	}
	return f.id(t, f.tmp)
}

// id is what identifies the file at p.
func (f *docsFixture) id(t *testing.T, p string) copyID {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return copyID{size: int64(len(b)), hash: hashText(string(b)), ino: inode(st)}
}

// record is what the store says of name in docs/ of f.dir.
func (f *docsFixture) record(t *testing.T, name string) record {
	t.Helper()
	records, err := f.docs.records(context.Background(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	return records[name]
}

// names lists docs/ of f.dir, then the number of the daemon's own links,
// as "a.md,b.md +2".
func (f *docsFixture) names(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.dir, DocsDir))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	own := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			own++
		} else {
			names = append(names, e.Name())
		}
	}
	return fmt.Sprintf("%s +%d", strings.Join(names, ","), own)
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
