// Package attachments keeps bounded copies of authorized chat input outside
// workspaces. Its ledger records ownership and delivery before any pruning.
package attachments

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
)

const (
	FileLimit    = 8 * 1024 * 1024
	MessageLimit = 20 * 1024 * 1024
	SessionLimit = 100 * 1024 * 1024
	CountLimit   = 10
)

type Manager struct {
	Root   string
	Store  *store.Store
	Driver func(string) surface.Driver
	locks  sync.Map
	// sessionLimit is overridden only by storage-pruning tests.
	sessionLimit int64
	beforePrune  func() // tests: a file changes after the eligibility check
}

func (m *Manager) Resolve(ctx context.Context, sid int64, client, message string, files []control.IncomingFile) ([]control.InputFile, error) {
	v, _ := m.locks.LoadOrStore(sid, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	root, err := m.directory(sid)
	if err != nil {
		return skipped(files, "attachment storage is unavailable"), nil
	}
	defer root.Close()
	var reader surface.AttachmentReader
	if m.Driver != nil {
		reader, _ = m.Driver(client).(surface.AttachmentReader)
	}
	var out []control.InputFile
	var total int64
	for i, f := range files {
		result := control.InputFile{Name: f.Name}
		var size int64
		err := m.Store.DB.QueryRowContext(ctx, `SELECT name, path, reason, size FROM input_attachments WHERE session_id=? AND message=? AND ordinal=?`, sid, message, i).Scan(&result.Name, &result.Path, &result.Reason, &size)
		if err == nil {
			total += size
			if result.Path != "" {
				if ok, _ := m.unchanged(ctx, root, sid, filepath.Base(result.Path)); !ok {
					result.Path = ""
					result.Reason = "the saved copy changed or is unavailable"
				}
			}
			out = append(out, result)
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		var data []byte
		switch {
		case i >= CountLimit:
			result.Reason = "too many files (limit 10)"
		case reader == nil:
			result.Reason = "the chat service cannot fetch attachments"
		case total >= MessageLimit:
			result.Reason = "message byte limit (20 MB)"
		default:
			data, result.Reason = read(fetchCtx, reader, f.URL, MessageLimit-total)
			size = int64(len(data))
			total += size
			if result.Reason == "" && !matchesName(f.Name, data) {
				result.Reason = "file type does not match its name"
			}
			if result.Reason == "" && total > MessageLimit {
				result.Reason = "message byte limit (20 MB)"
			}
		}
		var digest, identity string
		if result.Reason == "" {
			size = int64(len(data))
			if err := m.makeRoom(ctx, root, sid, size); err != nil {
				result.Reason = "session storage limit (100 MB), no safe copy to remove"
			} else {
				name := filename(message, i, f.Name)
				// O_EXCL refuses symlinks, hard links and existing names alike.
				dst, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
				if errors.Is(err, os.ErrExist) {
					// A crash may have left an unrecorded copy. It is not ours to
					// overwrite or remove: make a different new file on recovery.
					var nonce [16]byte
					if _, err = rand.Read(nonce[:]); err == nil {
						name += "-" + hex.EncodeToString(nonce[:])
						dst, err = root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
					}
				}
				if err != nil {
					result.Reason = "a new attachment file could not be created"
				} else {
					_, err = dst.Write(data)
					if err == nil {
						err = dst.Sync()
					}
					st, statErr := dst.Stat()
					closeErr := dst.Close()
					if err != nil || statErr != nil || closeErr != nil {
						result.Reason = "the attachment copy could not be saved"
					} else {
						digest = hash(data)
						identity = fileIdentity(st)
						result.Path = filepath.Join(m.Root, "state", "sessions", strconv.FormatInt(sid, 10), "attachments", name)
					}
				}
			}
		}
		// Never delete an unrecorded or half-written file: there is no durable
		// ownership proof for it. Directory accounting still counts its bytes.
		_, err = m.Store.DB.ExecContext(ctx, `INSERT INTO input_attachments (session_id,message,ordinal,name,path,digest,identity,size,reason) VALUES (?,?,?,?,?,?,?,?,?)`, sid, message, i, result.Name, result.Path, digest, identity, size, result.Reason)
		if err != nil {
			return nil, err
		}
		out = append(out, result)
	}
	return out, nil
}

func skipped(files []control.IncomingFile, reason string) []control.InputFile {
	out := make([]control.InputFile, len(files))
	for i, f := range files {
		out[i] = control.InputFile{Name: f.Name, Reason: reason}
	}
	return out
}

func read(ctx context.Context, r surface.AttachmentReader, address string, remaining int64) ([]byte, string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	src, err := r.OpenAttachment(ctx, address)
	if err != nil {
		return nil, "not fetched"
	}
	defer src.Close()
	limit := int64(FileLimit + 1)
	if remaining+1 < limit {
		limit = remaining + 1
	}
	data, err := io.ReadAll(io.LimitReader(src, limit))
	if err != nil {
		return data, "not fetched"
	}
	if int64(len(data)) > remaining {
		return data, "message byte limit (20 MB)"
	}
	if len(data) > FileLimit {
		return data, "file byte limit (8 MB)"
	}
	if !allowed(data) {
		return data, "unsupported file type"
	}
	return data, ""
}

func allowed(b []byte) bool {
	// Binary signatures take precedence over extensions and reported MIME.
	t := http.DetectContentType(b)
	if strings.HasPrefix(t, "image/png") || strings.HasPrefix(t, "image/jpeg") || strings.HasPrefix(t, "image/gif") || strings.HasPrefix(t, "image/webp") || bytes.HasPrefix(b, []byte("%PDF-")) {
		return true
	}
	// Avoid accepting archives whose headers happen to be valid UTF-8 (tar,
	// zip with empty entries, gzip, 7z, RAR); never expand anything.
	if strings.Contains(t, "zip") || strings.Contains(t, "rar") || strings.Contains(t, "gzip") ||
		bytes.HasPrefix(b, []byte{'P', 'K', 3, 4}) || bytes.HasPrefix(b, []byte{'P', 'K', 5, 6}) || bytes.HasPrefix(b, []byte{'P', 'K', 7, 8}) ||
		bytes.HasPrefix(b, []byte{'7', 'z', 0xbc, 0xaf, 0x27, 0x1c}) || bytes.HasPrefix(b, []byte("Rar!")) ||
		(len(b) > 262 && string(b[257:262]) == "ustar") {
		return false
	}
	if !utf8.Valid(b) {
		return false
	}
	for _, c := range b {
		if c < 32 && c != '\n' && c != '\r' && c != '\t' {
			return false
		}
	}
	return true
}

func hash(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func fileIdentity(st os.FileInfo) string {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", s.Dev, s.Ino)
	}
	return ""
}
func safe(s string, n int) string {
	var b strings.Builder
	for _, c := range s {
		if b.Len() >= n {
			break
		}
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			b.WriteByte(byte(c))
		} else {
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), ".")
}
func filename(message string, i int, name string) string {
	return "m" + safe(message, 48) + "-" + hash([]byte(message))[:12] + "-" + strconv.Itoa(i) + "-" + safe(name, 100)
}

// directory opens every component without following a link. os.Root also
// contains races where a component is replaced while it is being opened.
func (m *Manager) directory(sid int64) (*os.Root, error) {
	return m.openDirectories([]string{"state", "sessions", strconv.FormatInt(sid, 10), "attachments"})
}

func (m *Manager) openDirectories(parts []string) (*os.Root, error) {
	r, err := os.OpenRoot(m.Root)
	if err != nil {
		return nil, err
	}
	for _, p := range parts {
		if err = r.Mkdir(p, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			r.Close()
			return nil, err
		}
		st, e := r.Lstat(p)
		if e != nil || !st.IsDir() {
			r.Close()
			return nil, fmt.Errorf("attachment directory is not a directory")
		}
		sub, e := r.OpenRoot(p)
		if e != nil {
			r.Close()
			return nil, e
		}
		at, e := sub.Stat(".")
		if e != nil || !os.SameFile(st, at) {
			sub.Close()
			r.Close()
			return nil, fmt.Errorf("attachment directory changed")
		}
		r.Close()
		r = sub
	}
	return r, nil
}

func (m *Manager) unchanged(ctx context.Context, r *os.Root, sid int64, name string) (bool, error) {
	var digest, identity string
	err := m.Store.DB.QueryRowContext(ctx, `SELECT digest,identity FROM input_attachments WHERE session_id=? AND path=?`, sid, filepath.Join(m.Root, "state", "sessions", strconv.FormatInt(sid, 10), "attachments", name)).Scan(&digest, &identity)
	if err != nil {
		return false, err
	}
	st, err := r.Lstat(name)
	if err != nil || !st.Mode().IsRegular() || identity == "" || fileIdentity(st) != identity || st.Size() > FileLimit {
		return false, err
	}
	f, err := r.Open(name)
	if err != nil {
		return false, err
	}
	defer f.Close()
	at, err := f.Stat()
	if err != nil || !os.SameFile(st, at) {
		return false, err
	}
	b, err := io.ReadAll(io.LimitReader(f, FileLimit+1))
	if err != nil {
		return false, err
	}
	end, err := r.Lstat(name)
	return err == nil && os.SameFile(at, end) && hash(b) == digest, nil
}

func used(r *os.Root) (int64, error) {
	d, err := r.Open(".")
	if err != nil {
		return 0, err
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		st, err := r.Lstat(entry.Name())
		if err != nil {
			return 0, err
		}
		if st.Mode().IsRegular() {
			total += st.Size()
		} else {
			return 0, fmt.Errorf("unknown attachment storage entry")
		}
	}
	return total, nil
}
func (m *Manager) makeRoom(ctx context.Context, r *os.Root, sid int64, size int64) error {
	limit := int64(SessionLimit)
	if m.sessionLimit > 0 {
		limit = m.sessionLimit
	}
	n, err := used(r)
	if err != nil {
		return err
	}
	if n+size <= limit {
		return nil
	}
	rows, err := m.Store.DB.QueryContext(ctx, `SELECT id,path FROM input_attachments WHERE session_id=? AND given=1 AND path!='' ORDER BY id`, sid)
	if err != nil {
		return err
	}
	type copy struct {
		id   int64
		path string
	}
	var copies []copy
	for rows.Next() {
		var c copy
		if err = rows.Scan(&c.id, &c.path); err != nil {
			rows.Close()
			return err
		}
		copies = append(copies, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range copies {
		name := filepath.Base(c.path)
		ok, err := m.unchanged(ctx, r, sid, name)
		if err != nil || !ok {
			continue
		}
		if m.beforePrune != nil {
			m.beforePrune()
		}
		if err = m.prune(ctx, r, sid, name); err != nil {
			continue
		}
		if _, err = m.Store.DB.ExecContext(ctx, `UPDATE input_attachments SET path='',reason='copy expired at session storage limit' WHERE id=?`, c.id); err != nil {
			return err
		}
		n, err = used(r)
		if err != nil {
			return err
		}
		if n+size <= limit {
			return nil
		}
	}
	return fmt.Errorf("no safe attachment copy to remove")
}

// GivenTx records only files whose input the adapter confirmed as delivered.
// Merely accepting an input, writing it, or failing delivery is not enough.
func GivenTx(ctx context.Context, tx *store.Tx, sid int64, files []control.InputFile) error {
	for _, f := range files {
		if f.Path != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE input_attachments SET given=1 WHERE session_id=? AND path=?`, sid, f.Path); err != nil {
				return err
			}
		}
	}
	return nil
}

func matchesName(name string, b []byte) bool {
	want := map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp", ".pdf": "application/pdf"}[strings.ToLower(filepath.Ext(name))]
	return want == "" || http.DetectContentType(b) == want
}

// prune moves the entry into a fresh private directory before verifying it
// again. An entry replaced at the old name is never unlinked after a stale
// check. Anything uncertain is kept, with an exclusive link back when the
// original name is still free; it is never restored over another file.
func (m *Manager) prune(ctx context.Context, r *os.Root, sid int64, name string) error {
	parent, err := m.openDirectories([]string{"state", "sessions", strconv.FormatInt(sid, 10)})
	if err != nil {
		return err
	}
	defer parent.Close()
	at, err := parent.Lstat("attachments")
	if err != nil {
		return err
	}
	opened, err := r.Stat(".")
	if err != nil || !os.SameFile(at, opened) {
		return fmt.Errorf("attachment directory changed")
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	dir := ".attachment-gc-" + hex.EncodeToString(nonce[:])
	if err = parent.Mkdir(dir, 0700); err != nil {
		return err
	}
	isolated, err := parent.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer isolated.Close()
	if err = parent.Rename("attachments/"+name, dir+"/"+name); err != nil {
		parent.Remove(dir)
		return err
	}
	ok, err := m.unchanged(ctx, isolated, sid, name)
	if err != nil || !ok {
		// Keep the isolated link even when the public name could be restored:
		// an open descriptor may still be writing the preserved file.
		parent.Link(dir+"/"+name, "attachments/"+name)
		return fmt.Errorf("changed attachment preserved in %s", dir)
	}
	if err = isolated.Remove(name); err != nil {
		return err
	}
	parent.Remove(dir) // only our now-empty directory; refuses another entry
	return nil
}
