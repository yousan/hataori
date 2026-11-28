// Package desktopstate manages only an explicitly selected desktop instance.
// It does not edit chat configuration, acquire the daemon's lock, remove any
// file or find/signal a process by a recorded PID.
package desktopstate

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/yousan/hataori/internal/nativehttp"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
	_ "modernc.org/sqlite"
)

type State struct {
	root, state, desktop *os.Root
	db                   *sql.DB
}

// Selection is committed atomically with intent. Replacing an app or selected
// daemon never changes Stopped to Running. Paths are immutable versioned files;
// referenced old files are retained, including on uninstall.
type Selection struct {
	Intent  string `json:"intent"`
	Version string `json:"version"`
	Binary  string `json:"binary"`
}

const (
	Running = "running"
	Stopped = "stopped"
)

// Open requires the root to exist already. Only state/desktop is created. A
// separate database gives transactional intent without replacing configuration
// or a daemon database, and leaves SQLite's recovery journal intact on a crash.
func Open(ctx context.Context, path string) (_ *State, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := &State{}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	s.root, err = nativehttp.OpenPrivateRoot(path)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"state", "bin", "config"} {
		if err = RefuseCollision(s.root.Name(), name); err != nil {
			return nil, err
		}
	}
	s.state, err = nativehttp.OpenPrivateDir(s.root, "state", true)
	if err != nil {
		return nil, err
	}
	if err = RefuseCollision(s.state.Name(), "desktop"); err != nil {
		return nil, err
	}
	s.desktop, err = nativehttp.OpenPrivateDir(s.state, "desktop", true)
	if err != nil {
		return nil, err
	}
	if err = RefuseCollision(s.desktop.Name(), "launcher.sqlite"); err != nil {
		return nil, err
	}
	_, found := s.desktop.Lstat("launcher.sqlite")
	fresh := errors.Is(found, os.ErrNotExist)
	ownedInitialization, err := s.initializationMarker(fresh)
	if err != nil {
		return nil, err
	}
	flags := os.O_RDWR
	if fresh {
		flags |= os.O_CREATE | os.O_EXCL
	}
	f, err := nativehttp.OpenPrivateFile(s.desktop, "launcher.sqlite", flags)
	if err != nil {
		return nil, err
	}
	if !fresh {
		var header [72]byte
		n, readErr := f.ReadAt(header[:], 0)
		recognized := readErr == nil && string(header[:16]) == "SQLite format 3\x00" && binary.BigEndian.Uint32(header[68:]) == 0x48544453
		// A permanent private marker reserves this database before its first
		// creation. Empty/interrupted SQLite initialization can be retried;
		// an unknown pre-existing file without that provenance stays intact.
		initial := ownedInitialization && (n == 0 && errors.Is(readErr, io.EOF) || n == len(header) && string(header[:16]) == "SQLite format 3\x00" && binary.BigEndian.Uint32(header[68:]) == 0)
		if !recognized && !initial {
			f.Close()
			return nil, errors.New("desktop: existing state is not a recognized launcher database")
		}
		fresh = initial
	}
	f.Close()
	// SQLite opens its recovery files by pathname too. Refuse an inherited
	// link/ACL/mode before SQLite can follow it or recover another file.
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		name := "launcher.sqlite" + suffix
		if err = RefuseCollision(s.desktop.Name(), name); err != nil {
			return nil, err
		}
		var side *os.File
		side, err = nativehttp.OpenPrivateFile(s.desktop, name, os.O_RDWR)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
			continue
		}
		if err != nil {
			return nil, err
		}
		side.Close()
	}
	u := url.URL{Scheme: "file", Path: filepath.Join(s.desktop.Name(), "launcher.sqlite")}
	u.RawQuery = "mode=rw&_pragma=busy_timeout(5000)"
	s.db, err = sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	s.db.SetMaxOpenConns(1)
	if fresh {
		_, err = s.db.ExecContext(ctx, `PRAGMA application_id=1213482067`)
		if err != nil {
			return nil, err
		}
	}
	_, err = s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS desired (
		id INTEGER PRIMARY KEY CHECK(id=1),
		intent TEXT NOT NULL CHECK(intent IN ('running','stopped')),
		version TEXT NOT NULL, binary TEXT NOT NULL)`)
	if err != nil {
		return nil, err
	}
	_, err = s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS login (id INTEGER PRIMARY KEY CHECK(id=1), path TEXT NOT NULL, plist BLOB NOT NULL)`)
	if err != nil {
		return nil, err
	}
	return s, nil
}

const initializationMagic = "hataori desktop launcher initialization v1\n"

func (s *State) initializationMarker(create bool) (bool, error) {
	const name = "launcher.initialization"
	if err := RefuseCollision(s.desktop.Name(), name); err != nil {
		return false, err
	}
	if create {
		f, err := nativehttp.OpenPrivateFile(s.desktop, name, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		if err == nil {
			_, err = io.WriteString(f, initializationMagic)
			if err == nil {
				err = f.Sync()
			}
			f.Close()
			return err == nil, err
		}
		if !errors.Is(err, os.ErrExist) {
			return false, err
		}
	}
	f, err := nativehttp.OpenPrivateFile(s.desktop, name, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 128))
	if err != nil || string(data) != initializationMagic {
		return false, errors.New("desktop: unrecognized initialization marker was preserved")
	}
	return true, nil
}

// RefuseCollision catches aliases even on a case-sensitive CI filesystem.
// On macOS it also refuses differently spelled existing names rather than
// depending on a volume's case/normalization behavior. It never renames them.
func RefuseCollision(parent, wanted string) error {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	fold := func(s string) string { return cases.Fold().String(norm.NFC.String(s)) }
	for _, entry := range entries {
		if fold(entry.Name()) == fold(wanted) && entry.Name() != wanted {
			return errors.New("desktop: ambiguous filename spelling")
		}
	}
	return nil
}

// Register is explicit first-run adoption of an app-managed instance. Reading
// an existing root or replacing an app must never call it implicitly.
func (s *State) Register(ctx context.Context, selection Selection) error {
	if err := s.VerifySelection(ctx, selection); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO desired(id,intent,version,binary) VALUES(1,?,?,?)`, selection.Intent, selection.Version, selection.Binary)
	return err
}

func (s *State) Read(ctx context.Context) (Selection, error) {
	var d Selection
	err := s.db.QueryRowContext(ctx, `SELECT intent,version,binary FROM desired WHERE id=1`).Scan(&d.Intent, &d.Version, &d.Binary)
	if err == nil {
		err = validSelection(d)
	}
	return d, err
}

// SetIntent is the only start/stop action. Persist Stopped before the launcher
// closes its own daemon's lifetime pipe; login/app/launcher restarts read it.
func (s *State) SetIntent(ctx context.Context, intent string) error {
	if intent != Running && intent != Stopped {
		return errors.New("desktop: invalid intent")
	}
	r, err := s.db.ExecContext(ctx, `UPDATE desired SET intent=? WHERE id=1`, intent)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("desktop: instance is not app-managed")
	}
	return err
}

// Select atomically stages a replacement without changing persisted intent.
// Compatibility and the immutable binary's manifest are checked by the caller.
func (s *State) Select(ctx context.Context, version, binary string) error {
	if err := s.VerifySelection(ctx, Selection{Intent: Stopped, Version: version, Binary: binary}); err != nil {
		return err
	}
	r, err := s.db.ExecContext(ctx, `UPDATE desired SET version=?,binary=? WHERE id=1`, version, binary)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("desktop: instance is not app-managed")
	}
	return err
}

// LockLauncher serializes launchers, independently of the daemon's permanent
// state/lock. The lease file is retained; it is never unlinked or stolen.
func (s *State) LockLauncher() (*os.File, error) {
	if err := RefuseCollision(s.desktop.Name(), "launcher.lock"); err != nil {
		return nil, err
	}
	f, err := nativehttp.OpenPrivateFile(s.desktop, "launcher.lock", os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("desktop: launcher already owns this instance: %w", err)
	}
	return f, nil
}

func validSelection(d Selection) error {
	if d.Intent != Running && d.Intent != Stopped || !filepath.IsAbs(d.Binary) || filepath.Clean(d.Binary) != d.Binary || d.Version == "" || len(d.Version) > 128 || strings.ContainsAny(d.Version, "/\\\x00\r\n") {
		return errors.New("desktop: invalid managed selection")
	}
	return nil
}

func (s *State) Close() error {
	var err error
	if s.db != nil {
		err = s.db.Close()
	}
	for _, root := range []*os.Root{s.desktop, s.state, s.root} {
		if root != nil {
			err = errors.Join(err, root.Close())
		}
	}
	return err
}
