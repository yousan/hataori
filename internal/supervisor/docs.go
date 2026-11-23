package supervisor

import (
	"context"
	"crypto/rand"
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
	"sync"
	"syscall"
)

// DocsDir is hataori's directory in a supervisor's (design, section 7.1,
// "Its directory and its notes"): the daemon's copies of the files that
// scopes[].supervisor.files lists. The session may not write there.
const DocsDir = "docs"

// DocMax is the largest file the daemon copies into docs/.
const DocMax = 1 << 20

// docMode is a copy's mode: readable, not writable.
const docMode = 0o444

// tmpPrefix starts the names of the daemon's own links in docs/: a copy
// being written, the link that keeps each copy's inode, and a copy moved
// aside. The session can write no name that starts with a dot.
const tmpPrefix = ".hataori-"

// Docs copies files from the host into docs/ of supervisors' directories.
// It writes over or removes only a file it wrote there itself and that has
// not changed since: it records each copy's name, size, SHA-256 and inode
// in the store (supervisor_docs), and keeps a second link of its own to
// the copy in docs/, so that the inode stays the copy's while the record
// names it. Any other file there is left as it is, also one of the same
// content that someone else put there.
type Docs struct {
	DB  *sql.DB
	Log *slog.Logger

	mu sync.Mutex // one copy at a time: a launch and a wake may come together
}

// copyID tells one copy the daemon wrote from any other file: its size,
// SHA-256 and inode. While a record names it, a link of the daemon's own
// holds the inode, so no file made later gets it.
type copyID struct {
	size int64
	hash string
	ino  uint64
}

// record is what the store says the daemon wrote at one name: the copy in
// place, and the one being put in place when a daemon died meanwhile, each
// with the name of the daemon's own link to it in docs/.
type record struct {
	placed  copyID
	keep    string
	pending *copyID
	pkeep   string
}

// ours reports whether c is a copy r records.
func (r record) ours(c copyID) bool {
	return c == r.placed || r.pending != nil && c == *r.pending
}

// errNotOurs is a file that is not the daemon's copy.
var errNotOurs = errors.New("not hataori's copy")

// Copy puts the current content of each of files into docs/ of dir, under
// its base name, before a launch or a wake of the session that works in
// dir, and removes its own copies of files no longer listed. A listed file
// that is missing, not a regular file or larger than DocMax, and one whose
// place in docs/ holds a file the daemon did not write or that changed
// since, is skipped with one log line each; nothing here stops the launch
// or the wake. With no files and no copies recorded for dir, nothing is
// touched, not even docs/.
func (d *Docs) Copy(ctx context.Context, dir string, files []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	records, err := d.records(ctx, dir)
	if err != nil {
		d.Log.Error("supervisor docs: the store", "dir", dir, "err", err)
		return
	}
	if len(files) == 0 && len(records) == 0 {
		return
	}
	docs, err := openDocs(dir, len(files) > 0)
	if err != nil {
		d.Log.Warn("supervisor docs: nothing copied", "dir", dir, "err", err)
		return
	}
	if docs == nil {
		// No docs/ and nothing to copy: the copies recorded are gone.
		for name := range records {
			d.forget(ctx, dir, name)
		}
		return
	}
	defer docs.Close()
	listed := map[string]bool{}
	for _, f := range files {
		listed[filepath.Base(f)] = true
	}
	// First the copies no longer listed, so that a name listed again in
	// another case finds its place free where case does not count.
	for name, r := range records {
		if listed[name] {
			continue
		}
		// Removed only when it is still the copy.
		side, err := d.aside(docs, dir, name, r)
		switch {
		case errors.Is(err, errNotOurs):
			d.Log.Warn("supervisor docs: a copy no longer listed has changed since hataori wrote it; left as it is", "dir", dir, "name", name)
		case err != nil:
			d.Log.Error("supervisor docs: removing a copy no longer listed", "dir", dir, "name", name, "err", err)
			continue
		}
		if d.replace(ctx, docs, dir, name, r, nil) && side != "" {
			if err := docs.Remove(side); err != nil {
				d.Log.Error("supervisor docs: removing a copy no longer listed", "dir", dir, "name", name, "err", err)
			}
		}
	}
	for _, f := range files {
		name := filepath.Base(f)
		if name == "." || name == ".." || name == string(filepath.Separator) {
			d.Log.Warn("supervisor docs: not a file's path; skipped", "dir", dir, "path", f)
			continue
		}
		content, why := readDoc(f)
		if why != "" {
			// The copy made before, if any, stays as it was.
			d.Log.Warn("supervisor docs: a listed file is "+why+"; skipped", "dir", dir, "path", f)
			continue
		}
		r, known := records[name]
		d.put(ctx, docs, dir, name, content, r, known)
	}
}

// openDocs opens docs/ of dir, making it when make is set and it is not
// there; nil without make when it is not there. dir and docs/ must be
// directories themselves, not links.
func openDocs(dir string, make bool) (*os.Root, error) {
	st, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if after, err := root.Stat("."); err != nil || !os.SameFile(st, after) {
		return nil, fmt.Errorf("%s was replaced", dir)
	}
	if make {
		if err := root.Mkdir(DocsDir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	dst, err := root.Lstat(DocsDir)
	if errors.Is(err, fs.ErrNotExist) && !make {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !dst.IsDir() {
		return nil, fmt.Errorf("%s in %s is not a directory", DocsDir, dir)
	}
	docs, err := root.OpenRoot(DocsDir)
	if err != nil {
		return nil, err
	}
	if after, err := docs.Stat("."); err != nil || !os.SameFile(dst, after) {
		docs.Close()
		return nil, fmt.Errorf("%s in %s was replaced", DocsDir, dir)
	}
	return docs, nil
}

// readDoc reads the listed file f, or says why it is not copied.
func readDoc(f string) ([]byte, string) {
	st, err := os.Stat(f)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, "missing"
	case err != nil:
		return nil, "unreadable (" + err.Error() + ")"
	case !st.Mode().IsRegular():
		return nil, "not a regular file"
	case st.Size() > DocMax:
		return nil, "larger than 1 MB"
	}
	// Not blocking: a FIFO put at the path since it was looked at would
	// hold every launch and wake.
	fh, err := os.OpenFile(f, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, "unreadable (" + err.Error() + ")"
	}
	defer fh.Close()
	if st, err := fh.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, "not a regular file"
	}
	b, err := io.ReadAll(io.LimitReader(fh, DocMax+1))
	if err != nil {
		return nil, "unreadable (" + err.Error() + ")"
	}
	if len(b) > DocMax {
		return nil, "larger than 1 MB"
	}
	return b, ""
}

// identify is the copyID of the regular file name in docs; errNotOurs
// for anything else there, and the error for a file it could not read.
// errNotOurs too for one that is a copy r records but no longer readable
// only, as when someone made it writable to write it, or whose own link of
// the daemon's is gone, so that its inode may have been another file's. A
// file whose size and inode are none that r records is not read: it is
// not the copy, and its hash is "".
func identify(docs *os.Root, name string, r record) (copyID, error) {
	st, err := docs.Lstat(name)
	if err != nil {
		return copyID{}, err
	}
	if !st.Mode().IsRegular() {
		return copyID{}, errNotOurs
	}
	c := copyID{size: st.Size(), ino: inode(st)}
	if st.Mode().Perm() != docMode {
		return c, nil
	}
	same := func(x copyID) bool { return x.size == c.size && x.ino == c.ino }
	if !same(r.placed) && (r.pending == nil || !same(*r.pending)) {
		return c, nil
	}
	fh, err := docs.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return copyID{}, err
	}
	defer fh.Close()
	if after, err := fh.Stat(); err != nil || !os.SameFile(st, after) {
		return copyID{}, errNotOurs
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(fh, DocMax+1)); err != nil {
		return copyID{}, err
	}
	c.hash = hex.EncodeToString(h.Sum(nil))
	if r.ours(c) && !held(docs, r.keepOf(c), c.ino) {
		return copyID{}, errNotOurs
	}
	return c, nil
}

// held reports whether link, the daemon's own link to a copy, is there and
// is still the link to inode ino.
func held(docs *os.Root, link string, ino uint64) bool {
	if link == "" {
		return false
	}
	st, err := docs.Lstat(link)
	return err == nil && st.Mode().IsRegular() && inode(st) == ino
}

// inode is st's inode number, 0 where the system has none.
func inode(st fs.FileInfo) uint64 {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(sys.Ino)
	}
	return 0
}

// tmpName is a new name in docs/ for a link of the daemon's own.
func tmpName() string {
	var b [8]byte
	rand.Read(b[:])
	return tmpPrefix + hex.EncodeToString(b[:])
}

// put writes content at name in docs, unless a file is there that is not
// the daemon's copy, or is already the same. r is the record of name,
// known when there is one.
func (d *Docs) put(ctx context.Context, docs *os.Root, dir, name string, content []byte, r record, known bool) {
	sum := sha256.Sum256(content)
	size, hash := int64(len(content)), hex.EncodeToString(sum[:])
	cur, err := identify(docs, name, r)
	absent := errors.Is(err, fs.ErrNotExist)
	switch {
	case absent:
	case errors.Is(err, errNotOurs) || err == nil && (!known || !r.ours(cur)):
		d.Log.Warn("supervisor docs: a file in docs/ that hataori did not write, or that changed since it wrote it, is left as it is; the listed file is not copied",
			"dir", dir, "name", name)
		return
	case err != nil:
		d.Log.Error("supervisor docs: copying a listed file", "dir", dir, "name", name, "err", err)
		return
	case cur.size == size && cur.hash == hash:
		// Already in place, as after a daemon died before its record.
		if r.pending != nil {
			d.replace(ctx, docs, dir, name, r, &record{placed: cur, keep: r.keepOf(cur)})
		}
		return
	}
	// The new copy is written to tmp, which stays as the daemon's own link
	// to it, and recorded as pending beside the one in place: a daemon that
	// dies after the link still knows the file for its own, and one whose
	// write fails still knows the one in place. With nothing in place the
	// new copy is recorded at once.
	tmp := tmpName()
	c, err := writeDoc(docs, tmp, content)
	if err != nil {
		if !errors.Is(err, fs.ErrExist) { // not a file this call made
			docs.Remove(tmp)
		}
		d.Log.Error("supervisor docs: copying a listed file", "dir", dir, "name", name, "err", err)
		return
	}
	next := record{placed: c, keep: tmp}
	if !absent {
		next = record{placed: cur, keep: r.keepOf(cur), pending: &c, pkeep: tmp}
	}
	if !d.record(ctx, dir, name, next) {
		docs.Remove(tmp)
		return
	}
	side := ""
	if !absent {
		if side, err = d.aside(docs, dir, name, r); err != nil {
			if d.record(ctx, dir, name, r) {
				docs.Remove(tmp)
			}
			if errors.Is(err, errNotOurs) {
				d.Log.Warn("supervisor docs: a file in docs/ that hataori did not write, or that changed since it wrote it, is left as it is; the listed file is not copied",
					"dir", dir, "name", name)
			} else {
				d.Log.Error("supervisor docs: copying a listed file", "dir", dir, "name", name, "err", err)
			}
			return
		}
	}
	// Only where nothing is: a file put there meanwhile stays. The links
	// of the records before go once the store no longer names them.
	if err := docs.Link(tmp, name); err != nil {
		d.Log.Error("supervisor docs: copying a listed file", "dir", dir, "name", name, "err", err)
		if d.replace(ctx, docs, dir, name, next, nil) {
			d.dropLinks(docs, r, nil)
			if side != "" {
				docs.Remove(side)
			}
		}
		return
	}
	if d.replace(ctx, docs, dir, name, next, &record{placed: c, keep: tmp}) {
		d.dropLinks(docs, r, &record{placed: c, keep: tmp})
		if side != "" {
			docs.Remove(side)
		}
	}
}

// keepOf is the name of the daemon's own link to c, one of r's copies.
func (r record) keepOf(c copyID) string {
	if c == r.placed {
		return r.keep
	}
	return r.pkeep
}

// replace stores next as the record of name, or drops the record when
// next is nil, and then removes the links of old that next does not name.
// It reports whether the store took it.
func (d *Docs) replace(ctx context.Context, docs *os.Root, dir, name string, old record, next *record) bool {
	var ok bool
	if next == nil {
		ok = d.forget(ctx, dir, name)
	} else {
		ok = d.record(ctx, dir, name, *next)
	}
	if ok {
		d.dropLinks(docs, old, next)
	}
	return ok
}

// dropLinks removes the daemon's own links that old names and next does
// not, each only while it is still the link to the copy old records.
func (d *Docs) dropLinks(docs *os.Root, old record, next *record) {
	links := []struct {
		name string
		id   *copyID
	}{{old.keep, &old.placed}, {old.pkeep, old.pending}}
	for _, l := range links {
		if l.name == "" || l.id == nil || next != nil && (l.name == next.keep || l.name == next.pkeep) {
			continue
		}
		if st, err := docs.Lstat(l.name); err == nil && st.Mode().IsRegular() && inode(st) == l.id.ino {
			docs.Remove(l.name)
		}
	}
}

// aside moves the daemon's copy at name in docs out of the way, to a new
// name it returns, "" when nothing is at name: what it checks is then what
// the caller removes. A file there that is not the copy r records is left,
// or put back, and errNotOurs returned; one that could not be read is put
// back, and the error returned. If a file was put at name while one was
// aside, the one aside stays under its new name, logged.
func (d *Docs) aside(docs *os.Root, dir, name string, r record) (string, error) {
	st, err := docs.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case err != nil:
		return "", err
	case !st.Mode().IsRegular() || inode(st) != r.placed.ino && (r.pending == nil || inode(st) != r.pending.ino):
		return "", errNotOurs
	}
	side := tmpName()
	if err := docs.Rename(name, side); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	c, err := identify(docs, side, r)
	if err == nil && r.ours(c) {
		return side, nil
	}
	if err == nil {
		err = errNotOurs
	}
	if lerr := docs.Link(side, name); lerr == nil {
		docs.Remove(side) // the file stays at name
	} else if _, serr := docs.Lstat(name); !errors.Is(serr, fs.ErrNotExist) || docs.Rename(side, name) != nil {
		d.Log.Error("supervisor docs: a file in docs/ could not be put back; it is kept under another name", "dir", dir, "name", name, "kept_as", side, "err", lerr)
	}
	return "", err
}

// writeDoc writes content to a new file tmp in docs, readable only, and
// returns what identifies it.
func writeDoc(docs *os.Root, tmp string, content []byte) (copyID, error) {
	fh, err := docs.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return copyID{}, err
	}
	_, err = fh.Write(content)
	if err == nil {
		err = fh.Sync()
	}
	var st fs.FileInfo
	if err == nil {
		st, err = fh.Stat()
	}
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = docs.Chmod(tmp, docMode)
	}
	if err != nil {
		return copyID{}, err
	}
	sum := sha256.Sum256(content)
	return copyID{size: int64(len(content)), hash: hex.EncodeToString(sum[:]), ino: inode(st)}, nil
}

// records reads what the store says the daemon wrote in docs/ of dir.
func (d *Docs) records(ctx context.Context, dir string) (map[string]record, error) {
	rows, err := d.DB.QueryContext(ctx, `SELECT name, size, hash, ino, keep, pending_size, pending_hash, pending_ino, pending_keep FROM supervisor_docs WHERE dir = ?`, dir)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]record{}
	for rows.Next() {
		var (
			name        string
			r           record
			ino         int64
			psize, pino sql.NullInt64
			phash, pk   sql.NullString
		)
		if err := rows.Scan(&name, &r.placed.size, &r.placed.hash, &ino, &r.keep, &psize, &phash, &pino, &pk); err != nil {
			return nil, err
		}
		r.placed.ino = uint64(ino)
		if phash.Valid {
			r.pending = &copyID{size: psize.Int64, hash: phash.String, ino: uint64(pino.Int64)}
			r.pkeep = pk.String
		}
		out[name] = r
	}
	return out, rows.Err()
}

// record stores r as the record of name. It reports whether it was stored.
func (d *Docs) record(ctx context.Context, dir, name string, r record) bool {
	var psize, phash, pino, pkeep any
	if r.pending != nil {
		psize, phash, pino, pkeep = r.pending.size, r.pending.hash, int64(r.pending.ino), r.pkeep
	}
	_, err := d.DB.ExecContext(ctx, `INSERT INTO supervisor_docs (dir, name, size, hash, ino, keep, pending_size, pending_hash, pending_ino, pending_keep) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (dir, name) DO UPDATE SET size = excluded.size, hash = excluded.hash, ino = excluded.ino, keep = excluded.keep,
			pending_size = excluded.pending_size, pending_hash = excluded.pending_hash, pending_ino = excluded.pending_ino, pending_keep = excluded.pending_keep`,
		dir, name, r.placed.size, r.placed.hash, int64(r.placed.ino), r.keep, psize, phash, pino, pkeep)
	if err != nil {
		d.Log.Error("supervisor docs: the store", "dir", dir, "name", name, "err", err)
		return false
	}
	return true
}

// forget drops the record of the copy at name, and reports whether it did.
func (d *Docs) forget(ctx context.Context, dir, name string) bool {
	if _, err := d.DB.ExecContext(ctx, `DELETE FROM supervisor_docs WHERE dir = ? AND name = ?`, dir, name); err != nil {
		d.Log.Error("supervisor docs: the store", "dir", dir, "name", name, "err", err)
		return false
	}
	return true
}
