package attachments

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

type source struct {
	*fake.Driver
	files map[string][]byte
	calls []string
}

func (s *source) OpenAttachment(ctx context.Context, address string) (io.ReadCloser, error) {
	s.calls = append(s.calls, address)
	if _, ok := ctx.Deadline(); !ok {
		return nil, fmt.Errorf("missing fetch deadline")
	}
	b, ok := s.files[address]
	if !ok {
		return nil, fmt.Errorf("failed fetch")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func fixture(t *testing.T) (*Manager, *source) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "state"), 0700)
	st, err := store.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, q := range []string{
		`INSERT INTO threads(id,scope,key,started_by,state,created_at,updated_at) VALUES(1,'s','k','owner','idle',0,0)`,
		`INSERT INTO sessions(id,thread_id,agent,state,created_at,updated_at) VALUES(1,1,'fake','idle',0,0)`,
	} {
		if _, err := st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	src := &source{Driver: fake.New(), files: map[string][]byte{}}
	return &Manager{Root: root, Store: st, Driver: func(string) surface.Driver { return src }}, src
}
func resolve(t *testing.T, m *Manager, key string, fs ...control.IncomingFile) []control.InputFile {
	t.Helper()
	out, err := m.Resolve(context.Background(), 1, "chat", key, fs)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestNamesCopiesAndRestart(t *testing.T) {
	m, s := fixture(t)
	var picture bytes.Buffer
	png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	s.files["picture"] = picture.Bytes()
	names := []string{"../x", ".x", "a/b", "", strings.Repeat("x", 300), "x\n\x00", "same", "same", "image.png"}
	var files []control.IncomingFile
	for _, name := range names {
		files = append(files, control.IncomingFile{Name: name, URL: "picture"})
	}
	out := resolve(t, m, "../message", files...)
	seen := map[string]bool{}
	for i, f := range out {
		if f.Reason != "" || filepath.Dir(f.Path) != filepath.Join(m.Root, "state", "sessions", "1", "attachments") || strings.HasPrefix(filepath.Base(f.Path), ".") || seen[f.Path] {
			t.Fatalf("bad result %+v", f)
		}
		seen[f.Path] = true
		b, err := os.ReadFile(f.Path)
		if err != nil || !bytes.Equal(b, picture.Bytes()) {
			t.Fatalf("copy %d differs: %v", i, err)
		}
	}
	// Another manager models a restart after acceptance and before delivery.
	next := &Manager{Root: m.Root, Store: m.Store, Driver: m.Driver}
	again := resolve(t, next, "../message", files...)
	if !reflect.DeepEqual(out, again) || len(s.calls) != len(files) {
		t.Fatalf("restart fetched again or changed files: %+v", again)
	}
}
func TestPreexistingNamesAndDirectoryLinks(t *testing.T) {
	for _, link := range []bool{false, true} {
		t.Run(fmt.Sprint(link), func(t *testing.T) {
			m, s := fixture(t)
			s.files["text"] = []byte("hello")
			r, err := m.directory(1)
			if err != nil {
				t.Fatal(err)
			}
			r.Close()
			outside := filepath.Join(t.TempDir(), "person")
			os.WriteFile(outside, []byte("work"), 0600)
			path := filepath.Join(m.Root, "state", "sessions", "1", "attachments", filename("message", 0, "../x"))
			if link {
				err = os.Symlink(outside, path)
			} else {
				err = os.Link(outside, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			out := resolve(t, m, "message", control.IncomingFile{Name: "../x", URL: "text"})
			if out[0].Path == path {
				t.Fatal("preexisting name was overwritten")
			}
			b, _ := os.ReadFile(outside)
			if string(b) != "work" {
				t.Fatal("lost person's file")
			}
		})
	}
	t.Run("directory", func(t *testing.T) {
		m, s := fixture(t)
		s.files["text"] = []byte("hello")
		dir := filepath.Join(m.Root, "state", "sessions", "1")
		os.MkdirAll(dir, 0700)
		target := t.TempDir()
		os.Symlink(target, filepath.Join(dir, "attachments"))
		out := resolve(t, m, "m", control.IncomingFile{Name: "x", URL: "text"})
		if out[0].Reason == "" || len(s.calls) != 0 {
			t.Fatal("followed directory symlink")
		}
		entries, _ := os.ReadDir(target)
		if len(entries) != 0 {
			t.Fatal("wrote through directory link")
		}
	})
}
func TestLimitsAndSkips(t *testing.T) {
	m, s := fixture(t)
	s.files["large"] = bytes.Repeat([]byte("x"), FileLimit+1024)
	s.files["full"] = bytes.Repeat([]byte("x"), FileLimit)
	s.files["text"] = []byte("valid text")
	s.files["wrong"] = []byte{0xff, 0xfe, 0, 1}
	s.files["fake-image"] = []byte("this is not an image")
	var z bytes.Buffer
	w := zip.NewWriter(&z)
	w.Create("x")
	w.Close()
	s.files["zip"] = z.Bytes()
	var tarbuf bytes.Buffer
	tw := tar.NewWriter(&tarbuf)
	tw.WriteHeader(&tar.Header{Name: "x", Size: 0})
	tw.Close()
	s.files["tar"] = tarbuf.Bytes()
	for _, c := range []struct{ url, name, reason string }{{"large", "x", "file byte limit"}, {"wrong", "image.png", "unsupported"}, {"fake-image", "image.png", "does not match"}, {"zip", "archive.txt", "unsupported"}, {"tar", "archive.txt", "unsupported"}, {"failed", "x", "not fetched"}} {
		out := resolve(t, m, c.url, control.IncomingFile{Name: c.name, URL: c.url})
		if !strings.Contains(out[0].Reason, c.reason) {
			t.Fatalf("%s: %+v", c.url, out)
		}
	}
	files := make([]control.IncomingFile, 11)
	for i := range files {
		files[i] = control.IncomingFile{Name: "x", URL: "text"}
	}
	n := len(s.calls)
	out := resolve(t, m, "count", files...)
	if len(s.calls)-n != 10 || !strings.Contains(out[10].Reason, "too many") {
		t.Fatal("count limit not enforced")
	}
	files = []control.IncomingFile{{Name: "1", URL: "full"}, {Name: "2", URL: "full"}, {Name: "3", URL: "full"}, {Name: "4", URL: "full"}}
	n = len(s.calls)
	out = resolve(t, m, "bytes", files...)
	if len(s.calls)-n != 3 || out[0].Path == "" || out[1].Path == "" || !strings.Contains(out[2].Reason, "message byte limit") || out[3].Path != "" {
		t.Fatalf("byte limit: %+v", out)
	}
}
func TestPruneOnlyOwnedUnchangedDeliveredCopies(t *testing.T) {
	m, s := fixture(t)
	s.files["text"] = []byte("1234")
	m.sessionLimit = 24
	var files []control.InputFile
	for _, k := range []string{"old", "changed", "replaced", "pending"} {
		files = append(files, resolve(t, m, k, control.IncomingFile{Name: "x", URL: "text"})[0])
	}
	if err := m.Store.Update(context.Background(), func(tx *store.Tx) error { return GivenTx(context.Background(), tx, 1, files[:3]) }); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(files[1].Path, []byte("work"), 0600)
	// Same bytes under a different inode must not become the daemon's copy.
	replacement := files[2].Path + "-person"
	os.WriteFile(replacement, []byte("1234"), 0600)
	os.Rename(replacement, files[2].Path)
	untracked := filepath.Join(filepath.Dir(files[0].Path), "person")
	os.WriteFile(untracked, []byte("work"), 0600)
	// 20 bytes exist. One new four-byte copy fits, the next prunes the oldest
	// delivered unchanged copy. None of the other files is eligible.
	resolve(t, m, "new1", control.IncomingFile{Name: "x", URL: "text"})
	next := resolve(t, m, "new2", control.IncomingFile{Name: "x", URL: "text"})
	if next[0].Path == "" {
		t.Fatalf("failed to prune eligible copy: %+v", next)
	}
	if _, err := os.Lstat(files[0].Path); !os.IsNotExist(err) {
		t.Fatal("old delivered copy was not pruned")
	}
	for _, p := range []string{files[1].Path, files[2].Path, files[3].Path, untracked} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("removed protected file %s", p)
		}
	}
	last := resolve(t, m, "refused", control.IncomingFile{Name: "x", URL: "text"})
	if last[0].Path != "" || !strings.Contains(last[0].Reason, "session storage limit") {
		t.Fatalf("over limit: %+v", last)
	}
	b, _ := os.ReadFile(files[1].Path)
	if string(b) != "work" {
		t.Fatal("changed file lost")
	}
}

func TestRestartRetainsMessageByteBudget(t *testing.T) {
	m, s := fixture(t)
	s.files["full"] = bytes.Repeat([]byte("x"), FileLimit)
	files := []control.IncomingFile{{Name: "1", URL: "full"}, {Name: "2", URL: "full"}, {Name: "3", URL: "full"}}
	resolve(t, m, "interrupted", files[:1]...)
	next := &Manager{Root: m.Root, Store: m.Store, Driver: m.Driver}
	out := resolve(t, next, "interrupted", files...)
	if out[0].Path == "" || out[1].Path == "" || !strings.Contains(out[2].Reason, "message byte limit") {
		t.Fatalf("restart reset message budget: %+v", out)
	}
}

func TestPrunePreservesChangesAfterEligibilityCheck(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint(replace), func(t *testing.T) {
			m, s := fixture(t)
			m.sessionLimit = 4
			s.files["text"] = []byte("1234")
			old := resolve(t, m, "old", control.IncomingFile{Name: "x", URL: "text"})
			if err := m.Store.Update(context.Background(), func(tx *store.Tx) error { return GivenTx(context.Background(), tx, 1, old) }); err != nil {
				t.Fatal(err)
			}
			m.beforePrune = func() {
				if replace {
					tmp := old[0].Path + "-person"
					if err := os.WriteFile(tmp, []byte("work"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(tmp, old[0].Path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(old[0].Path, []byte("work"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			out := resolve(t, m, "new", control.IncomingFile{Name: "x", URL: "text"})
			if out[0].Path != "" {
				t.Fatal("removed changed file to make room")
			}
			b, err := os.ReadFile(old[0].Path)
			if err != nil || string(b) != "work" {
				t.Fatalf("lost change after eligibility check: %q %v", b, err)
			}
		})
	}
}

func TestIncompleteUnrecordedCopyDoesNotLoseRecoveredPicture(t *testing.T) {
	m, s := fixture(t)
	var pic bytes.Buffer
	png.Encode(&pic, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	s.files["picture"] = pic.Bytes()
	r, err := m.directory(1)
	if err != nil {
		t.Fatal(err)
	}
	name := filename("m", 0, "picture.png")
	partial, err := r.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	partial.Write([]byte("partial copy"))
	partial.Close()
	r.Close()
	out := resolve(t, m, "m", control.IncomingFile{Name: "picture.png", URL: "picture"})
	if out[0].Path == "" || filepath.Base(out[0].Path) == name {
		t.Fatalf("could not recover picture beside uncertain file: %+v", out)
	}
	b, err := os.ReadFile(out[0].Path)
	if err != nil || !bytes.Equal(b, pic.Bytes()) {
		t.Fatal("recovered picture differs")
	}
	b, err = os.ReadFile(filepath.Join(filepath.Dir(out[0].Path), name))
	if err != nil || string(b) != "partial copy" {
		t.Fatal("uncertain file was changed")
	}
}
