package supervisor

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

// FileMax is the largest file that Files reads and writes.
const FileMax = 256 << 10

// The most that a supervisor's directory holds besides docs/ after a write,
// unless the write makes it no larger.
const (
	dirBytes = 20 << 20
	dirFiles = 500
)

const (
	pathParts = 16   // the most parts of a path
	listMax   = 2000 // the most files that a list gives
)

// ScopeFiles lists, reads and writes the files of the supervisors'
// directories for their sessions (control.API.Files; design, section 7.1,
// "No write is lost"). Everything is done inside the directory of the
// caller's scope, part by part of the path and through no link of any kind.
// The writes of a directory are made one after another, each whole or not
// at all, and each only where it fits what is there at that moment. Only
// the daemon's own files, which it recorded before it made them
// (supervisor_writes), are ever removed.
type ScopeFiles struct {
	Store *store.Store
	Log   *slog.Logger
	// Hook, when a test sets it, is called between two steps that something
	// may come between: "entering" a directory and "opening" a file, once it
	// was looked at; "making" a directory on the way; "made", once the
	// daemon's own file of a write is there, empty; "written", once it holds
	// the whole new content and before what is there is looked at for the
	// last time; "placing", after that and before it takes the file's name;
	// "placed", once it has it; and "dropped", once it has no other name and
	// before the directory is synced.
	Hook func(step string)

	locks keyed // of the directories, for their writes
}

// API returns api with Files answered for thread, a supervisor's of scope:
// in their directory, whatever a request says.
func (s *ScopeFiles) API(api control.API, scope string, thread control.ThreadID) control.API {
	return &filing{API: api, files: s, scope: scope, thread: thread}
}

type filing struct {
	control.API
	files  *ScopeFiles
	scope  string
	thread control.ThreadID
}

// A refusal is why a call is refused. It quotes neither the path nor
// anything in a file, so that a log line may say it.
type refusal string

func (r refusal) Error() string { return string(r) }

// errMeanwhile is a file that changed while it was written.
var errMeanwhile = errors.New("it changed meanwhile")

const errClosed = refusal("the thread of this session is closed")

// Files is part of control.API.
func (f *filing) Files(ctx context.Context, r control.FilesRequest) (control.FilesAnswer, error) {
	ans, err := f.files.do(ctx, f.scope, f.thread, r)
	op := r.Op
	if !slices.Contains([]string{control.FilesList, control.FilesRead, control.FilesWrite, control.FilesEdit}, op) {
		op = "unknown"
	}
	switch {
	case err != nil:
		f.files.Log.Warn("a supervisor's file call is refused", "scope", f.scope, "thread", int64(f.thread), "op", op, "reason", err.Error())
		return control.FilesAnswer{}, fmt.Errorf("supervisor: Files: %w", err)
	case ans.Refused != "":
		f.files.Log.Info("a supervisor's write does not fit the file", "scope", f.scope, "thread", int64(f.thread), "op", op, "reason", ans.Refused)
	}
	return ans, nil
}

func (s *ScopeFiles) do(ctx context.Context, scope string, thread control.ThreadID, r control.FilesRequest) (control.FilesAnswer, error) {
	dir, err := s.dir(ctx, scope, thread)
	if err != nil {
		return control.FilesAnswer{}, err
	}
	var parts []string
	if r.Op != control.FilesList {
		if parts, err = filePath(r.Path); err != nil {
			return control.FilesAnswer{}, err
		}
	}
	switch r.Op {
	case control.FilesList:
		return s.list(ctx, dir)
	case control.FilesRead:
		return s.read(dir, parts)
	case control.FilesWrite:
		if r.Text == nil {
			return control.FilesAnswer{}, refusal("a write without a text")
		}
	case control.FilesEdit:
		if r.Old == "" || r.New == nil {
			return control.FilesAnswer{}, refusal("an edit without the passage or what replaces it")
		}
	default:
		return control.FilesAnswer{}, refusal("no such operation on files")
	}
	return s.write(ctx, scope, thread, dir, parts, r)
}

// dir is the directory of scope's supervisor threads, for thread, which
// must be one that works there.
func (s *ScopeFiles) dir(ctx context.Context, scope string, thread control.ThreadID) (string, error) {
	var dir string
	err := s.Store.DB.QueryRowContext(ctx, `SELECT dir FROM supervisor_dirs WHERE scope = ?`, scope).Scan(&dir)
	if errors.Is(err, sql.ErrNoRows) {
		return "", refusal("the channel's supervisor has no directory yet")
	}
	if err != nil {
		return "", fmt.Errorf("the supervisor's directory: %w", err)
	}
	var own string
	err = s.Store.DB.QueryRowContext(ctx, `SELECT workspace FROM threads WHERE id = ? AND scope = ?`, int64(thread), scope).Scan(&own)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("the thread's directory: %w", err)
	}
	if err != nil || filepath.Clean(own) != filepath.Clean(dir) {
		return "", refusal("this session does not work in the channel's supervisor's directory")
	}
	return dir, nil
}

// filePath is the parts of path, which must be one that Files takes.
func filePath(path string) ([]string, error) {
	switch {
	case path == "":
		return nil, refusal("no path")
	case !utf8.ValidString(path):
		return nil, refusal("a path that is not UTF-8")
	case strings.HasPrefix(path, "/"):
		return nil, refusal("an absolute path")
	}
	if strings.Count(path, "/") >= pathParts {
		return nil, refusal(fmt.Sprintf("a path with more than %d parts", pathParts))
	}
	parts := strings.Split(path, "/")
	for _, p := range parts {
		if err := pathPart(p); err != nil {
			return nil, err
		}
	}
	return parts, nil
}

// blank are the characters drawn as nothing that no property of Unicode's
// tells of.
const blank = "\u2800\U00013441\U00013442\U00016fe4\U0001d159"

// pathPart says why p is no part of a path that Files takes: it is empty,
// starts with a dot, as "..", "." and the daemon's own names do, or holds
// what does not show, so that two names could look the same.
func pathPart(p string) error {
	switch {
	case p == "":
		return refusal("a path with an empty part")
	case strings.HasPrefix(p, "."):
		return refusal("a path with a part that starts with a dot")
	case p != strings.Trim(p, " ") || !utf8.ValidString(p):
		return refusal("a path with a character that does not show")
	}
	for _, r := range p {
		// IsPrint leaves out control and format characters and every space
		// but the ASCII one; the others here are letters, marks and symbols
		// that are drawn as nothing.
		if !unicode.IsPrint(r) || unicode.In(r, unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector) || strings.ContainsRune(blank, r) {
			return refusal("a path with a character that does not show")
		}
	}
	return nil
}

// open opens the scope's directory for Files.
func (s *ScopeFiles) open(dir string) (*os.Root, error) {
	root, err := openDir(dir)
	if err != nil {
		s.Log.Error("a supervisor's directory cannot be opened", "dir", dir, "err", err)
		return nil, refusal("the channel's supervisor's directory cannot be opened")
	}
	return root, nil
}

func (s *ScopeFiles) step(step string) {
	if s.Hook != nil {
		s.Hook(step)
	}
}

// enter opens the directory name in root, which must be a directory itself.
func (s *ScopeFiles) enter(root *os.Root, name string) (*os.Root, error) {
	st, err := root.Lstat(name)
	if err != nil {
		return nil, fault("looking at a directory on the way", err)
	}
	if !st.IsDir() {
		return nil, refusal("a part of the path is a link, or no directory")
	}
	s.step("entering")
	// As a directory on the way, which must be one: OpenRoot alone would wait
	// on a pipe, should one have taken its place.
	sub, err := root.OpenRoot(name + "/.")
	if err != nil {
		return nil, fault("opening a directory on the way", err)
	}
	// OpenRoot follows a link that stays in root, should one have taken the
	// directory's place since.
	if after, err := sub.Stat("."); err != nil || !os.SameFile(st, after) {
		sub.Close()
		return nil, refusal("a part of the path was replaced meanwhile")
	}
	return sub, nil
}

// fault is err of doing what, without the path that err names. That there
// is no such file stays known of it.
func fault(what string, err error) error {
	var (
		path *fs.PathError
		link *os.LinkError
	)
	switch {
	case errors.As(err, &path):
		err = path.Err
	case errors.As(err, &link):
		err = link.Err
	}
	return fmt.Errorf("%s: %w", what, err)
}

// way opens the last directory of parts, names in root, that is there, and
// returns the names of those after it.
func (s *ScopeFiles) way(root *os.Root, parts []string) (*os.Root, []string, error) {
	at, err := root.OpenRoot(".")
	if err != nil {
		return nil, nil, fault("opening the directory", err)
	}
	for i, p := range parts {
		next, err := s.enter(at, p)
		if errors.Is(err, fs.ErrNotExist) {
			return at, parts[i:], nil
		}
		at.Close()
		if err != nil {
			return nil, nil, err
		}
		at = next
	}
	return at, nil, nil
}

// found is the regular file that stands at a path. It has a version and a
// text only when Files reads it.
type found struct {
	control.ScopeFile
	access
}

// access is who may do what with a file. A file that takes another's place
// is given that one's, whole: a write changes the text and nothing else.
type access struct {
	uid, gid int
	perm     fs.FileMode // with the bits that run it as another, which give does not give
	acl      string      // the access control list, where the system has one of it; "" for none
}

// accessOf is the access of fh.
func accessOf(fh *os.File) (access, error) {
	st, err := fh.Stat()
	if err != nil {
		return access{}, err
	}
	acl, err := aclOf(fh)
	if err != nil {
		return access{}, err
	}
	a := access{perm: st.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky), acl: acl}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		a.uid, a.gid = int(sys.Uid), int(sys.Gid)
	}
	return a, nil
}

// give gives fh, which the daemon made and nobody may read or write yet, the
// access a, or refuses. Its permissions come last: as long as it is another's
// it stays closed to everyone.
func (a access) give(fh *os.File) error {
	now, err := accessOf(fh)
	if err == nil && (now.uid != a.uid || now.gid != a.gid) {
		err = fh.Chown(a.uid, a.gid)
	}
	if err == nil && now.acl != a.acl {
		err = setACL(fh, a.acl)
	}
	if err == nil {
		err = fh.Chmod(a.perm.Perm())
	}
	if err == nil {
		now, err = accessOf(fh)
	}
	if err != nil || now != a {
		return refusal("a file whose owner, group, permissions or access list the daemon cannot keep")
	}
	return nil
}

// without is f without its text, for an answer; nil for none.
func (f *found) without() *control.ScopeFile {
	if f == nil {
		return nil
	}
	file := f.ScopeFile
	file.Text = ""
	return &file
}

// with is f with its text, for an answer; nil for none.
func (f *found) with() *control.ScopeFile {
	if f == nil {
		return nil
	}
	return &f.ScopeFile
}

// version is f's; "" for none.
func (f *found) version() string {
	if f == nil {
		return ""
	}
	return f.Version
}

func versionOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// look reads what stands at name in dir, at path in the answers: nil when
// nothing does, and an error for what is not a regular file.
func (s *ScopeFiles) look(dir *os.Root, name, path string) (*found, error) {
	st, err := dir.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fault("looking at the file", err)
	}
	if !st.Mode().IsRegular() {
		return nil, refusal("not a regular file")
	}
	f := &found{ScopeFile: control.ScopeFile{Path: path, Size: st.Size()}}
	if st.Size() > FileMax {
		return f, nil
	}
	s.step("opening")
	// Not waiting on a pipe, should one have taken the file's place. os.Root
	// follows a link that stays in the directory whatever the flags, so what
	// was opened must be what was looked at.
	fh, err := dir.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fault("opening the file", err)
	}
	defer fh.Close()
	if after, err := fh.Stat(); err != nil || !os.SameFile(st, after) || !after.Mode().IsRegular() {
		return nil, refusal("the file was replaced while it was read")
	}
	b, err := io.ReadAll(io.LimitReader(fh, FileMax+1))
	if err != nil {
		return nil, fault("reading the file", err)
	}
	if f.access, err = accessOf(fh); err != nil {
		return nil, fault("looking at the file", err)
	}
	f.Size = int64(len(b))
	if len(b) > FileMax || !utf8.Valid(b) {
		return f, nil
	}
	f.Text = string(b)
	f.Version = versionOf(f.Text)
	return f, nil
}

// list gives every file of dir that a path of Files names.
func (s *ScopeFiles) list(ctx context.Context, dir string) (control.FilesAnswer, error) {
	root, err := s.open(dir)
	if err != nil {
		return control.FilesAnswer{}, err
	}
	defer root.Close()
	var files []control.ScopeFile
	var walk func(at *os.Root, prefix string, depth int) error
	walk = func(at *os.Root, prefix string, depth int) error {
		es, err := entries(at)
		if err != nil {
			return err
		}
		for _, e := range es {
			if err := ctx.Err(); err != nil {
				return err
			}
			if pathPart(e.Name()) != nil {
				continue
			}
			if e.IsDir() {
				if depth == pathParts {
					continue
				}
				// One that went meanwhile, or that the daemon may not open,
				// has no file that a path names.
				if sub, err := s.enter(at, e.Name()); err == nil {
					err = walk(sub, prefix+e.Name()+"/", depth+1)
					sub.Close()
					if err != nil {
						return err
					}
				}
				continue
			}
			f, err := s.look(at, e.Name(), prefix+e.Name())
			switch {
			case err == nil && f != nil:
				files = append(files, *f.without())
			case err == nil:
				continue // gone
			default: // it cannot be read, or is not a regular file
				if st, err := at.Lstat(e.Name()); err == nil && st.Mode().IsRegular() {
					files = append(files, control.ScopeFile{Path: prefix + e.Name(), Size: st.Size()})
				}
			}
			if len(files) > listMax {
				return refusal(fmt.Sprintf("the directory has more than %d files", listMax))
			}
		}
		return nil
	}
	if err := walk(root, "", 1); err != nil {
		return control.FilesAnswer{}, err
	}
	slices.SortFunc(files, func(a, b control.ScopeFile) int { return cmp.Compare(a.Path, b.Path) })
	return control.FilesAnswer{Files: files}, nil
}

// entries is what stands in the directory at.
func entries(at *os.Root) ([]fs.DirEntry, error) {
	d, err := at.Open(".")
	if err != nil {
		return nil, fault("reading a directory", err)
	}
	defer d.Close()
	es, err := d.ReadDir(-1)
	if err != nil {
		return nil, fault("reading a directory", err)
	}
	return es, nil
}

// read gives the file at parts in dir.
func (s *ScopeFiles) read(dir string, parts []string) (control.FilesAnswer, error) {
	root, err := s.open(dir)
	if err != nil {
		return control.FilesAnswer{}, err
	}
	defer root.Close()
	last := len(parts) - 1
	parent, missing, err := s.way(root, parts[:last])
	if err != nil {
		return control.FilesAnswer{}, err
	}
	defer parent.Close()
	var f *found
	if len(missing) == 0 {
		if f, err = s.look(parent, parts[last], strings.Join(parts, "/")); err != nil {
			return control.FilesAnswer{}, err
		}
	}
	switch {
	case f == nil:
		return control.FilesAnswer{}, refusal("there is no such file")
	case f.Version == "":
		return control.FilesAnswer{}, refusal("the file is over 256 KB, or not UTF-8")
	}
	return control.FilesAnswer{File: f.with()}, nil
}

// write makes r's write or edit of the file at parts in dir.
func (s *ScopeFiles) write(ctx context.Context, scope string, thread control.ThreadID, dir string, parts []string, r control.FilesRequest) (control.FilesAnswer, error) {
	if strings.EqualFold(parts[0], DocsDir) {
		return control.FilesAnswer{}, refusal("no write in docs/")
	}
	defer s.locks.lock(dir)()
	root, err := s.open(dir)
	if err != nil {
		return control.FilesAnswer{}, err
	}
	defer root.Close()
	// Also where the disk knows it by another name, such as one with a dot
	// after it.
	if same(root, parts[0], DocsDir) {
		return control.FilesAnswer{}, refusal("no write in docs/")
	}
	// What a write before this one left half made, when it could not be
	// taken back then.
	if err := s.settle(ctx, dir, root); err != nil {
		return control.FilesAnswer{}, err
	}
	cur, next, why, err := s.fit(root, parts, r)
	if err != nil {
		return control.FilesAnswer{}, err
	}
	if why != "" {
		return control.FilesAnswer{File: cur.with(), Refused: why}, nil
	}
	if cur != nil && cur.Text == next {
		return control.FilesAnswer{File: cur.without()}, nil
	}
	if err := s.allowed(ctx, root, parts, cur, next); err != nil {
		return control.FilesAnswer{}, err
	}

	// From here on it is done or taken back, also when the caller is gone.
	ctx = context.WithoutCancel(ctx)
	path := strings.Join(parts, "/")
	there, err := s.there(root, parts)
	if err != nil {
		return control.FilesAnswer{}, err
	}
	w := placing{path: path, tmp: tmpName(), was: cur.version(), version: versionOf(next), there: there}
	id, err := s.record(ctx, scope, thread, dir, w)
	if err != nil {
		return control.FilesAnswer{}, err
	}
	var as *access
	if cur != nil {
		as = &cur.access
	}
	left, err := s.place(ctx, id, dir, root, &w, next, as)
	if err == nil {
		w.name = userFile(root, path)
	}
	if left == nil {
		left = s.finish(ctx, root, id, w, err == nil)
	}
	if left != nil { // the next write does it, or the next start
		w.made = sql.NullBool{Bool: err == nil, Valid: true}
		left = errors.Join(left, s.amend(ctx, id, w))
		s.Log.Error("a supervisor's write is not settled", "scope", scope, "err", left)
	}
	if errors.Is(err, errMeanwhile) {
		cur, _, _, err := s.fit(root, parts, r)
		if err != nil {
			return control.FilesAnswer{}, err
		}
		return control.FilesAnswer{File: cur.with(), Refused: "the file changed while it was written"}, nil
	}
	if err != nil {
		return control.FilesAnswer{}, err
	}
	s.Log.Info("a supervisor's file is written", "scope", scope, "thread", int64(thread), "path", path, "bytes", len(next), "new", cur == nil)
	return control.FilesAnswer{File: &control.ScopeFile{Path: path, Size: int64(len(next)), Version: w.version}}, nil
}

// same reports whether a and b are two names of one thing in root.
func same(root *os.Root, a, b string) bool {
	if a == b {
		return true
	}
	x, err := root.Lstat(a)
	if err != nil {
		return false
	}
	y, err := root.Lstat(b)
	return err == nil && os.SameFile(x, y)
}

// fit is what stands at parts in root, what r makes of it, and why r does
// not fit it; empty when it does.
func (s *ScopeFiles) fit(root *os.Root, parts []string, r control.FilesRequest) (cur *found, next, why string, err error) {
	last := len(parts) - 1
	parent, missing, err := s.way(root, parts[:last])
	if err != nil {
		return nil, "", "", err
	}
	defer parent.Close()
	if len(missing) == 0 {
		if cur, err = s.look(parent, parts[last], strings.Join(parts, "/")); err != nil {
			return nil, "", "", err
		}
	}
	if cur != nil && cur.Version == "" {
		return cur, "", "the file is over 256 KB, or not UTF-8", nil
	}
	if r.Op == control.FilesEdit {
		if cur == nil {
			return nil, "", "there is no such file", nil
		}
		i := strings.Index(cur.Text, r.Old)
		if i < 0 {
			return cur, "", "the passage is not in the file", nil
		}
		// Also where the second one overlaps the first.
		if strings.Contains(cur.Text[i+1:], r.Old) {
			return cur, "", "the passage is in the file more than once", nil
		}
		return cur, cur.Text[:i] + *r.New + cur.Text[i+len(r.Old):], "", nil
	}
	switch {
	case cur == nil && r.Version != "":
		return nil, "", "there is no such file any more", nil
	case cur != nil && r.Version == "":
		return cur, "", "the file is there already", nil
	case cur != nil && r.Version != cur.Version:
		return cur, "", "the file has changed since that version", nil
	}
	return cur, *r.Text, "", nil
}

// allowed says why next may not stand at parts in root, where cur does.
func (s *ScopeFiles) allowed(ctx context.Context, root *os.Root, parts []string, cur *found, next string) error {
	switch {
	case !utf8.ValidString(next):
		return refusal("a text that is not UTF-8")
	case len(next) > FileMax:
		return refusal("a file over 256 KB")
	}
	if len(parts) == 1 && len(next) > UserFileMax {
		for _, name := range UserFiles {
			if strings.EqualFold(parts[0], name) {
				return refusal("one of the user's files over 32 KB")
			}
		}
	}
	var size int64
	if cur != nil {
		size = cur.Size
		if err := s.writable(root, parts); err != nil {
			return err
		}
	}
	return room(ctx, root, cur == nil, int64(len(next))-size)
}

// writable says that the daemon may not write the file at parts, though it
// could put another in its place.
func (s *ScopeFiles) writable(root *os.Root, parts []string) error {
	const why = refusal("a file that the daemon may not write")
	last := len(parts) - 1
	parent, missing, err := s.way(root, parts[:last])
	if err != nil {
		return err
	}
	defer parent.Close()
	if len(missing) > 0 {
		return why
	}
	fh, err := parent.OpenFile(parts[last], os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return why
	}
	return fh.Close()
}

// room says that root has no room besides docs/ for one more file, when
// there is one more, or for grown bytes more than it holds. Everything that
// is not a directory counts, whatever its name, but not what is in a
// directory that the daemon may not read.
func room(ctx context.Context, root *os.Root, more bool, grown int64) error {
	if !more && grown <= 0 {
		return nil
	}
	var (
		files int
		bytes int64
	)
	return fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission):
			return nil // gone meanwhile, or not for the daemon to count
		case err != nil:
			return fault("counting the directory's files", err)
		case ctx.Err() != nil:
			return ctx.Err()
		case d.IsDir() && strings.EqualFold(path, DocsDir):
			return fs.SkipDir
		case !d.IsDir():
			st, err := d.Info()
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return fault("counting the directory's files", err)
			}
			files++
			bytes += st.Size()
		}
		if more && files >= dirFiles {
			return refusal(fmt.Sprintf("more than %d files in the directory", dirFiles))
		}
		if grown > 0 && bytes+grown > dirBytes {
			return refusal("more than 20 MB in the directory")
		}
		return nil
	})
}

// placing is a write as supervisor_writes records it before it is made.
type placing struct {
	path    string
	tmp     string       // the daemon's own name for the new content, beside the file
	was     string       // the version of the file it replaces; "" for a new one
	version string       // that of the new content
	there   int          // how many of the directories on the way were there before
	made    sql.NullBool // what its session was told of it, if anything
	name    string       // the user's file that was made, "" for none
}

func (w placing) parts() []string { return strings.Split(w.path, "/") }

// own is the path of the daemon's own file.
func (w placing) own() string {
	parts := w.parts()
	parts[len(parts)-1] = w.tmp
	return strings.Join(parts, "/")
}

const notItsOwn = "what has the name of the daemon's own file is none of its: it stays"

// there is how many of the directories on the way of parts are in root.
func (s *ScopeFiles) there(root *os.Root, parts []string) (int, error) {
	last := len(parts) - 1
	at, missing, err := s.way(root, parts[:last])
	if err != nil {
		return 0, err
	}
	at.Close()
	return last - len(missing), nil
}

// place puts text where w says, whole and with the access of the file that
// it replaces (as; nil for a new one): it is written beside the file and
// takes its name only where what stands there is still what w replaces. It
// leaves no file of the daemon's own, or left says why one. A directory that
// someone else made on the way meanwhile counts in w, the write id, as one
// that was there.
func (s *ScopeFiles) place(ctx context.Context, id int64, dir string, root *os.Root, w *placing, text string, as *access) (left, err error) {
	parts := w.parts()
	last := len(parts) - 1
	parent, missing, err := s.way(root, parts[:last])
	if err != nil {
		w.there = last // it made none
		return nil, err
	}
	defer func() { parent.Close() }()
	if err := s.others(ctx, id, w, last-len(missing)); err != nil {
		return nil, err
	}
	for i, p := range missing {
		s.step("making")
		if err := parent.Mkdir(p, 0o700); errors.Is(err, fs.ErrExist) {
			if err := s.others(ctx, id, w, last-len(missing)+i+1); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, fault("making a directory on the way", err)
		}
		syncDir(parent)
		next, err := s.enter(parent, p)
		if err != nil {
			return nil, err
		}
		parent.Close()
		parent = next
	}
	own, err := parent.OpenFile(w.tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0)
	if err != nil {
		return nil, fault("writing the file", err)
	}
	s.step("made")
	if err = fill(own, text, as); err != nil {
		err = fault("writing the file", err)
	} else {
		s.step("written")
		err = s.name(dir, root, parent, *w, own, as)
	}
	left = s.drop(own, parent, *w)
	s.step("dropped")
	syncDir(parent)
	return left, err
}

// drop removes the name that own, the daemon's own file of w, has in parent
// beside the one that name gave it, or alone: also where the path no longer
// leads to parent. It then closes own, which stays open until here so that
// nothing made meanwhile gets the inode by which it is known, and not longer,
// since nobody can run a file that is open to be written.
func (s *ScopeFiles) drop(own *os.File, parent *os.Root, w placing) error {
	defer own.Close()
	ours, err := isAt(own, parent, w.tmp)
	if err == nil && !ours {
		s.Log.Warn(notItsOwn, "path", w.own())
		return nil
	}
	if err == nil {
		err = parent.Remove(w.tmp)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fault("removing the daemon's own file", err)
	}
	return nil
}

// name gives own, which place wrote to parent, the name of the file.
func (s *ScopeFiles) name(dir string, root, parent *os.Root, w placing, own *os.File, as *access) error {
	parts := w.parts()
	last := len(parts) - 1
	// What the path names now is looked up from its beginning again: what
	// was put in the place of the directory, of one on the way or of the
	// file since it was looked at is not written to.
	now, err := openDir(dir)
	if err != nil {
		return errMeanwhile
	}
	defer now.Close()
	if !sameDir(root, now) {
		return errMeanwhile
	}
	there, missing, err := s.way(now, parts[:last])
	if err != nil {
		return err
	}
	defer there.Close()
	if len(missing) > 0 || !sameDir(parent, there) {
		return errMeanwhile
	}
	if ours, err := isAt(own, parent, w.tmp); err != nil || !ours {
		return errMeanwhile // someone else took it away, or put another there
	}
	cur, err := s.look(parent, parts[last], w.path)
	if err != nil {
		return err
	}
	if w.was == "" {
		if cur != nil {
			return errMeanwhile
		}
		s.step("placing")
		// Only where nothing is.
		if err := parent.Link(w.tmp, parts[last]); errors.Is(err, fs.ErrExist) {
			return errMeanwhile
		} else if err != nil {
			return fault("putting the file in its place", err)
		}
	} else {
		if cur == nil || cur.Version != w.was || cur.access != *as {
			return errMeanwhile
		}
		s.step("placing")
		if err := parent.Rename(w.tmp, parts[last]); err != nil {
			return fault("putting the file in its place", err)
		}
	}
	s.step("placed")
	return nil
}

// isAt reports whether own, an open file, is what has the name in dir.
func isAt(own *os.File, dir *os.Root, name string) (bool, error) {
	st, err := own.Stat()
	if err != nil {
		return false, err
	}
	at, err := dir.Lstat(name)
	return err == nil && os.SameFile(st, at), err
}

// fill writes text to own, the daemon's own file, gives it the access as, the
// daemon's own user's only for nil, and syncs it. Until it is whole nobody may
// read or write it, which is how ownFile knows it.
func fill(own *os.File, text string, as *access) error {
	_, err := own.WriteString(text)
	if err == nil {
		err = own.Sync()
	}
	if err == nil {
		if as != nil {
			err = as.give(own)
		} else {
			err = own.Chmod(0o600)
		}
	}
	if err == nil {
		err = own.Sync()
	}
	return err
}

// syncDir syncs dir, so that a write that was answered outlives the host.
// One that cannot be synced is in place all the same.
func syncDir(dir *os.Root) {
	if d, err := dir.Open("."); err == nil {
		d.Sync()
		d.Close()
	}
}

// sameDir reports whether a and b are one directory.
func sameDir(a, b *os.Root) bool {
	x, err := a.Stat(".")
	if err != nil {
		return false
	}
	y, err := b.Stat(".")
	return err == nil && os.SameFile(x, y)
}

// unmake removes the directories on the way of w that were not there before
// it, as far as they are empty.
func (s *ScopeFiles) unmake(root *os.Root, w placing) {
	parts := w.parts()
	for i := len(parts) - 2; i >= w.there; i-- {
		parent, missing, err := s.way(root, parts[:i])
		if err != nil {
			return
		}
		if len(missing) == 0 {
			// Not os.Root's Remove, which would remove a file just as well.
			if d, err := parent.Open("."); err == nil {
				unix.Unlinkat(int(d.Fd()), parts[i], unix.AT_REMOVEDIR)
				d.Close()
			}
		}
		parent.Close()
	}
}

// record stores w before anything of it is on the disk, and returns its
// number. pos is the newest event's position then: the turn it belongs to
// ends after it. A call that waited until its thread was closed is refused:
// what it wrote would be named nowhere.
func (s *ScopeFiles) record(ctx context.Context, scope string, thread control.ThreadID, dir string, w placing) (id int64, err error) {
	err = s.Store.Update(ctx, func(tx *store.Tx) error {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM threads WHERE id = ?`, int64(thread)).Scan(&state); err != nil {
			return err
		}
		if state == string(control.Closed) || state == string(control.Removed) {
			return errClosed
		}
		return tx.QueryRowContext(ctx, `INSERT INTO supervisor_writes (scope, thread, dir, path, tmp, was, version, there, pos)
			SELECT ?, ?, ?, ?, ?, ?, ?, ?, coalesce(max(pos), 0) FROM events RETURNING id`,
			scope, int64(thread), dir, w.path, w.tmp, w.was, w.version, w.there).Scan(&id)
	})
	switch {
	case errors.Is(err, errClosed):
		return 0, errClosed
	case err != nil:
		return 0, fmt.Errorf("recording the write: %w", err)
	}
	return id, nil
}

// others counts the first n directories on the way of w, the write id, as
// those of someone else: no settling of it takes them back.
func (s *ScopeFiles) others(ctx context.Context, id int64, w *placing, n int) error {
	if n <= w.there {
		return nil
	}
	w.there = n
	return s.amend(ctx, id, *w)
}

// amend records what became known of w, the write id, since it was recorded,
// for when it is settled.
func (s *ScopeFiles) amend(ctx context.Context, id int64, w placing) error {
	err := s.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE supervisor_writes SET there = ?, made = ?, name = ? WHERE id = ?`, w.there, w.made, w.name, id)
		return err
	})
	if err != nil {
		return fmt.Errorf("recording the write: %w", err)
	}
	return nil
}

// settle finds out, for each write in dir, opened as root, that is not
// recorded as done, whether it was made, and takes back what is left of it
// when it was not: those that a daemon died in, and any that could not be
// settled when it was made or refused. The directory must be locked.
func (s *ScopeFiles) settle(ctx context.Context, dir string, root *os.Root) error {
	rows, err := s.Store.DB.QueryContext(ctx, `SELECT id, path, tmp, was, version, there, made, name FROM supervisor_writes WHERE dir = ? AND done = 0 ORDER BY id`, dir)
	if err != nil {
		return fmt.Errorf("the writes before this one: %w", err)
	}
	defer rows.Close()
	type write struct {
		placing
		id int64
	}
	var open []write
	for rows.Next() {
		var w write
		if err := rows.Scan(&w.id, &w.path, &w.tmp, &w.was, &w.version, &w.there, &w.made, &w.name); err != nil {
			return fmt.Errorf("the writes before this one: %w", err)
		}
		open = append(open, w)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("the writes before this one: %w", err)
	}
	for _, w := range open {
		made, err := s.clear(root, w.placing)
		if err != nil {
			// Looked at again at the next write and the next start.
			s.Log.Error("a supervisor's write cannot be settled", "dir", dir, "err", err)
			continue
		}
		if w.made.Valid {
			made = w.made.Bool
		} else if made {
			w.name = userFile(root, w.path)
		}
		if err := s.finish(ctx, root, w.id, w.placing, made); err != nil {
			return err
		}
	}
	return nil
}

// clear removes the daemon's own file of w, where w's path leads to it, and
// reports whether w was made, by what is there. That file is gone once w
// replaced a file, and a second name of the new one once w made it; without
// it, w was made unless what it was to replace is still there. That tells
// nothing of a write whose file someone changed since, and as little does
// its own file beside another where there was none: it counts as made, so
// that one of the user's files is named once too often rather than not at
// all.
func (s *ScopeFiles) clear(root *os.Root, w placing) (made bool, err error) {
	parts := w.parts()
	last := len(parts) - 1
	parent, missing, err := s.way(root, parts[:last])
	if err != nil {
		return false, err
	}
	defer parent.Close()
	if len(missing) > 0 {
		// Gone with its directory, or not begun; or in the directory still,
		// where that was moved to.
		s.Log.Warn("the directory of a write that was not finished is not there: a file of the daemon's own may be where it was moved to",
			"path", w.path, "own", w.tmp)
		return false, nil
	}
	tmp, err := parent.Lstat(w.tmp)
	switch {
	case err == nil:
		at, err := parent.Lstat(parts[last])
		placed := err == nil && os.SameFile(tmp, at)
		if !s.ownFile(parent, tmp, w, placed) {
			s.Log.Warn(notItsOwn, "path", w.own())
			break
		}
		if err := parent.Remove(w.tmp); err != nil {
			return false, fault("removing the daemon's own file", err)
		}
		replaced := w.was == "" && err == nil && tmp.Mode().Perm() != 0
		return placed || replaced, nil
	case !errors.Is(err, fs.ErrNotExist):
		return false, fault("looking at the daemon's own file", err)
	}
	cur, err := s.look(parent, parts[last], w.path)
	if err != nil {
		return true, nil // something else is there now
	}
	return cur.version() != w.was, nil
}

// ownFile reports whether tmp, what stands at w's own name in parent, may go
// because it is the daemon's: a file that nobody may read or write yet, the
// whole of what w writes, or, placed, a second name of the file.
func (s *ScopeFiles) ownFile(parent *os.Root, tmp fs.FileInfo, w placing, placed bool) bool {
	switch {
	case !strings.HasPrefix(w.tmp, tmpPrefix) || !tmp.Mode().IsRegular():
		return false
	case placed || tmp.Mode().Perm() == 0:
		return true
	}
	f, err := s.look(parent, w.tmp, w.tmp)
	return err == nil && f != nil && f.Version == w.version
}

// finish records what became of the write id, which clear cleared, and takes
// back the directories of one that was not made. One of the user's files
// that was written stays recorded until the line that names it was posted
// (Ended); every other write is forgotten.
func (s *ScopeFiles) finish(ctx context.Context, root *os.Root, id int64, w placing, made bool) error {
	if !made {
		s.unmake(root, w)
	}
	query, args := `DELETE FROM supervisor_writes WHERE id = ?`, []any{id}
	if w.name != "" {
		query, args = `UPDATE supervisor_writes SET done = 1, name = ? WHERE id = ?`, []any{w.name, id}
	}
	err := s.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, query, args...)
		return err
	})
	if err != nil {
		return fmt.Errorf("recording the write: %w", err)
	}
	return nil
}

// userFile is the name of the user's file that path names in root, "" for
// none. Where case does not count, another spelling names it too.
func userFile(root *os.Root, path string) string {
	for _, name := range UserFiles {
		if same(root, path, name) {
			return name
		}
	}
	return ""
}

// Sweep settles the writes that the daemon died in. Call it before the
// first one of this daemon.
func (s *ScopeFiles) Sweep(ctx context.Context) error {
	rows, err := s.Store.DB.QueryContext(ctx, `SELECT DISTINCT dir FROM supervisor_writes WHERE done = 0`)
	if err != nil {
		return fmt.Errorf("supervisor: the writes left: %w", err)
	}
	defer rows.Close()
	var dirs []string
	for rows.Next() {
		var dir string
		if err := rows.Scan(&dir); err != nil {
			return fmt.Errorf("supervisor: the writes left: %w", err)
		}
		dirs = append(dirs, dir)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("supervisor: the writes left: %w", err)
	}
	for _, dir := range dirs {
		if err := s.sweep(ctx, dir); err != nil {
			s.Log.Error("a supervisor's writes cannot be settled", "dir", dir, "err", err)
		}
	}
	return nil
}

func (s *ScopeFiles) sweep(ctx context.Context, dir string) error {
	defer s.locks.lock(dir)()
	root, err := openDir(dir)
	if err != nil {
		return err // looked at again at the next start, should it be there then
	}
	defer root.Close()
	return s.settle(ctx, dir, root)
}

// FilesTurn is a turn of one of a scope's supervisor threads that wrote
// some of the user's files, until the lines that name them were posted.
type FilesTurn struct {
	Thread control.ThreadID
	Pos    uint64   // of the event that ended it
	Names  []string // in the order of UserFiles
}

// Ended has the user's files that thread, a supervisor's of scope, wrote
// before its turn's end at pos told with that end. A write that is still
// being made is waited for; one that is not settled is told of once it is.
func (s *ScopeFiles) Ended(ctx context.Context, scope string, thread control.ThreadID, pos uint64) error {
	var dir string
	err := s.Store.DB.QueryRowContext(ctx, `SELECT dir FROM supervisor_dirs WHERE scope = ?`, scope).Scan(&dir)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err == nil {
		defer s.locks.lock(dir)()
		err = s.Store.Update(ctx, func(tx *store.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE supervisor_writes SET told = ?1 WHERE scope = ?2 AND thread = ?3 AND told = 0 AND pos < ?1`,
				int64(pos), scope, int64(thread))
			return err
		})
	}
	if err != nil {
		return fmt.Errorf("supervisor: the user's files that a turn wrote: %w", err)
	}
	return nil
}

// Untold are the turns of scope's supervisor threads that ended (Ended) and
// whose files were not said to be named (Named).
func (s *ScopeFiles) Untold(ctx context.Context, scope string) ([]FilesTurn, error) {
	rows, err := s.Store.DB.QueryContext(ctx, `SELECT DISTINCT thread, told, name FROM supervisor_writes WHERE scope = ? AND done = 1 AND told > 0 ORDER BY told, thread`, scope)
	if err != nil {
		return nil, fmt.Errorf("supervisor: the user's files that a turn wrote: %w", err)
	}
	defer rows.Close()
	var turns []FilesTurn
	for rows.Next() {
		var (
			thread, pos int64
			name        string
		)
		if err := rows.Scan(&thread, &pos, &name); err != nil {
			return nil, fmt.Errorf("supervisor: the user's files that a turn wrote: %w", err)
		}
		t := FilesTurn{Thread: control.ThreadID(thread), Pos: uint64(pos)}
		if n := len(turns); n == 0 || turns[n-1].Thread != t.Thread || turns[n-1].Pos != t.Pos {
			turns = append(turns, t)
		}
		last := &turns[len(turns)-1]
		last.Names = append(last.Names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("supervisor: the user's files that a turn wrote: %w", err)
	}
	for _, t := range turns {
		slices.SortFunc(t.Names, func(a, b string) int {
			return cmp.Compare(slices.Index(UserFiles, a), slices.Index(UserFiles, b))
		})
	}
	return turns, nil
}

// Named forgets what t names of its turn: the lines that name it were posted.
func (s *ScopeFiles) Named(ctx context.Context, scope string, t FilesTurn) error {
	err := s.Store.Update(ctx, func(tx *store.Tx) error {
		for _, name := range t.Names {
			_, err := tx.ExecContext(ctx, `DELETE FROM supervisor_writes WHERE scope = ? AND thread = ? AND told = ? AND done = 1 AND name = ?`,
				scope, int64(t.Thread), int64(t.Pos), name)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("supervisor: the user's files that a turn wrote: %w", err)
	}
	return nil
}
