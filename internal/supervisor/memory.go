package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

// Keep each notice small; files beyond it stay untold for the next turn.
const memoryNoticeMax = 32
const memoryNoticeBytes = 4096

// memory reads only regular, addressable files, through no symbolic link.
// Large or unreadable files are compared by size and modification time.
func (s *Sessions) memory(ctx context.Context, dir string) (map[string]string, error) {
	f := &ScopeFiles{Store: s.Core.Store, Log: s.Log}
	root, err := f.open(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	now := map[string]string{}
	for _, name := range UserFiles {
		now[name] = fingerprint(f, root, name, UserFileMax)
	}
	notes, err := f.enter(root, "notes")
	if errors.Is(err, fs.ErrNotExist) {
		return now, nil
	}
	if err != nil {
		return now, nil
	} // a link or inaccessible directory is never followed
	defer notes.Close()
	count := 0
	var walk func(*os.Root, string, int) error
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
				if depth >= pathParts {
					continue
				}
				sub, err := f.enter(at, e.Name())
				if err != nil {
					continue
				}
				err = walk(sub, prefix+e.Name()+"/", depth+1)
				sub.Close()
				if err != nil {
					return err
				}
				continue
			}
			st, err := at.Lstat(e.Name())
			if err != nil || !st.Mode().IsRegular() {
				continue
			}
			count++
			if count > listMax {
				return refusal("too many notes to compare")
			}
			now[prefix+e.Name()] = fingerprint(f, at, e.Name(), FileMax)
		}
		return nil
	}
	if err := walk(notes, "notes/", 2); err != nil {
		return nil, err
	}
	return now, nil
}

func fingerprint(f *ScopeFiles, at *os.Root, name string, limit int) string {
	st, err := at.Lstat(name)
	if err != nil || !st.Mode().IsRegular() {
		return ""
	}
	if st.Size() <= int64(limit) {
		if got, err := f.look(at, name, name); err == nil && got != nil && got.Version != "" {
			return got.Version
		}
	}
	return fmt.Sprintf("stat:%d:%d", st.Size(), st.ModTime().UnixNano())
}

func recordedSession(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, sid int64) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, told FROM supervisor_session_files WHERE session_id = ?`, sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var name, hash string
		if err := rows.Scan(&name, &hash); err != nil {
			return nil, err
		}
		m[name] = hash
	}
	return m, rows.Err()
}

func remember(ctx context.Context, tx *store.Tx, sid int64, name, hash string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO supervisor_session_files (session_id, name, told) VALUES (?, ?, ?)
 ON CONFLICT (session_id, name) DO UPDATE SET told = excluded.told`, sid, name, hash)
	return err
}

func sessionID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, thread control.ThreadID) (int64, error) {
	var sid int64
	err := q.QueryRowContext(ctx, `SELECT id FROM sessions WHERE thread_id = ? ORDER BY id DESC LIMIT 1`, int64(thread)).Scan(&sid)
	return sid, err
}

func memoryChanges(sid int64, told, now map[string]string) []Change {
	names := make([]string, 0, len(told)+len(now))
	for name := range told {
		names = append(names, name)
	}
	for name := range now {
		if _, ok := told[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	var changes []Change
	bytes := 0
	for _, name := range names {
		if told[name] == now[name] {
			continue
		}
		if len(changes) >= memoryNoticeMax || bytes+len(name) > memoryNoticeBytes {
			break
		}
		changes = append(changes, Change{Name: name, Was: told[name], Now: now[name], Session: sid})
		bytes += len(name)
	}
	return changes
}

// TurnContext is the daemon's context for a conversation's next input.
// The core persists it with the pending input, outside the input event.
func (s *Sessions) TurnContext(ctx context.Context, tx *store.Tx, sid int64) (string, error) {
	var kind control.ThreadKind
	var dir string
	if err := tx.QueryRowContext(ctx, `SELECT t.kind, t.workspace FROM threads t JOIN sessions s ON s.thread_id = t.id WHERE s.id = ?`, sid).Scan(&kind, &dir); err != nil {
		return "", err
	}
	if kind != control.KindConversation {
		return "", nil
	}
	now, err := s.memory(ctx, dir)
	if err != nil {
		return "", err
	}
	told, err := recordedSession(ctx, tx, sid)
	if err != nil {
		return "", err
	}
	// At a session's first launch its user files are supplied as instructions;
	// only notes need the separate context. Subsequent launches preserve notes.
	if len(told) == 0 {
		for _, name := range UserFiles {
			delete(now, name)
		}
	}
	changes := memoryChanges(sid, told, now)
	var names []string
	for _, c := range changes {
		if err := remember(ctx, tx, sid, c.Name, c.Now); err != nil {
			return "", err
		}
		names = append(names, c.Name)
	}
	if len(names) == 0 {
		return "", nil
	}
	return FilesChanged(names) + "\n\n", nil
}

// acknowledge remembers exactly the content given by a file call. The
// caller holds the scope's write lock when acknowledging a successful write.
func (s *ScopeFiles) acknowledge(ctx context.Context, thread control.ThreadID, path, version string) error {
	if !slices.Contains(UserFiles, path) && !strings.HasPrefix(path, "notes/") {
		return nil
	}
	return s.Store.Update(ctx, func(tx *store.Tx) error {
		sid, err := sessionID(ctx, tx, thread)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return remember(ctx, tx, sid, path, version)
	})
}

// acknowledgeFile records the response, rather than scanning after another
// session could have overwritten it. Metadata fallback is captured under
// the same directory lock as the file call.
func (s *ScopeFiles) acknowledgeFile(ctx context.Context, thread control.ThreadID, root *os.Root, path, version string, size int64) error {
	name := path
	limit := FileMax
	if user := userFile(root, path); user != "" {
		name, limit = user, UserFileMax
	}
	if size > int64(limit) {
		st, err := root.Lstat(path)
		if err != nil {
			return err
		}
		version = fmt.Sprintf("stat:%d:%d", st.Size(), st.ModTime().UnixNano())
	}
	return s.acknowledge(ctx, thread, name, version)
}
