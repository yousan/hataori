package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// The names LinkThreads records its steps under.
const (
	linkThreadsStep = "thread numbers: links"
	// The rows a daemon wrote after the first step and before it named
	// threads by number in the control API: threads with no link or
	// workspace path, events with no thread number, outbox rows with no
	// link.
	linkLaterStep = "thread numbers: rows written before the API used them"
)

// LinkThreads fills in what the migration "thread numbers: links" could
// not without the config (design, section 6.2), once: the daemon calls it
// after Open and before anything reads a thread. In one transaction it
// gives each thread one link, its chat conversation (the client clients
// names for its scope, the scope as the channel, its key as the thread),
// fills events.thread_id from (scope, thread), gives each outbox row the
// link of its conversation, and copies each thread's workspace path from
// what its sessions recorded. A thread whose scope clients does not name
// gets no link, and one line in lg. Nothing outside the database is read
// or written: no workspace moves.
//
// A second step does the same, once, for the rows written after the first
// step by a daemon that did not write them itself yet: threads started
// since the first step, and events and outbox rows that still have no
// thread number or link. When both run in one call, the second finds
// nothing.
//
// Each step is recorded in startup_steps, and does nothing when it was.
func (s *Store) LinkThreads(ctx context.Context, clients map[string]string, lg *slog.Logger) error {
	since, err := s.linkStep(ctx, linkThreadsStep, clients, -1, lg)
	if err != nil {
		return err
	}
	if since < 0 { // the first step ran before; threads started after it
		if err := s.DB.QueryRowContext(ctx, `SELECT done_at FROM startup_steps WHERE name = ?`, linkThreadsStep).Scan(&since); err != nil {
			return fmt.Errorf("store: link threads: %w", err)
		}
	} else {
		since = time.Now().UnixMilli() + 1 // it ran now: none
	}
	_, err = s.linkStep(ctx, linkLaterStep, clients, since, lg)
	return err
}

// linkStep runs linkThreads as the step name, for the threads started at
// or after since (every thread when since < 0), unless the step was
// recorded. It returns when the step was recorded now, or -1 when it was
// before.
func (s *Store) linkStep(ctx context.Context, name string, clients map[string]string, since int64, lg *slog.Logger) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: link threads: %w", err)
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM startup_steps WHERE name = ?`, name).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: link threads: %w", err)
	}
	if n > 0 {
		return -1, nil
	}
	unlinked, err := linkThreads(ctx, tx, clients, since)
	if err != nil {
		return 0, fmt.Errorf("store: link threads: %w", err)
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO startup_steps (name, done_at) VALUES (?, ?)`, name, now); err != nil {
		return 0, fmt.Errorf("store: link threads: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: link threads: %w", err)
	}
	for _, l := range unlinked {
		lg.Warn(l)
	}
	return now, nil
}

func linkThreads(ctx context.Context, tx *sql.Tx, clients map[string]string, since int64) (unlinked []string, err error) {
	type thread struct {
		id                    int64
		scope, key, startedBy string
		created               int64
	}
	var threads []thread
	// A thread that has a link was started by a daemon that stored it.
	rows, err := tx.QueryContext(ctx, `SELECT id, scope, key, started_by, created_at FROM threads
		WHERE created_at >= ? AND id NOT IN (SELECT thread_id FROM links) ORDER BY id`, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t thread
		if err := rows.Scan(&t.id, &t.scope, &t.key, &t.startedBy, &t.created); err != nil {
			rows.Close()
			return nil, err
		}
		threads = append(threads, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, t := range threads {
		dir, err := recordedWorkspace(ctx, tx, t.id)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE threads SET workspace = ? WHERE id = ? AND workspace = ''`, dir, t.id); err != nil {
			return nil, fmt.Errorf("thread %d: %w", t.id, err)
		}
		client, ok := clients[t.scope]
		if !ok {
			unlinked = append(unlinked, fmt.Sprintf("store: thread %d gets no link: its scope %q is not in the config", t.id, t.scope))
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO links (thread_id, client, channel, thread, made_by, created_at, opened) VALUES (?, ?, ?, ?, ?, ?, 1)`,
			t.id, client, t.scope, t.key, t.startedBy, t.created); err != nil {
			return nil, fmt.Errorf("thread %d: %w", t.id, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE events SET thread_id =
		(SELECT t.id FROM threads t WHERE t.scope = events.scope AND t.key = events.thread) WHERE thread_id IS NULL`); err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	// link is 0 for none since the outbox has rows per link.
	if _, err := tx.ExecContext(ctx, `UPDATE chat_outbox SET link = coalesce(
		(SELECT l.id FROM links l WHERE l.client = chat_outbox.client AND l.channel = chat_outbox.channel AND l.thread = chat_outbox.thread), 0)
		WHERE link IS NULL OR link = 0`); err != nil {
		return nil, fmt.Errorf("outbox: %w", err)
	}
	return unlinked, nil
}

// recordedWorkspace is the workspace path the thread's newest session that
// knows one recorded: in its first input while it starts, in its spec once
// its agent was launched. Empty when none did.
func recordedWorkspace(ctx context.Context, tx *sql.Tx, thread int64) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, first_input, spec FROM sessions WHERE thread_id = ? ORDER BY id DESC`, thread)
	if err != nil {
		return "", fmt.Errorf("thread %d: %w", thread, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			sid      int64
			fi, spec string
			first    struct {
				Dir string `json:"dir"`
			}
			specified struct {
				Spec struct {
					Dir string `json:"dir"`
				} `json:"spec"`
			}
		)
		if err := rows.Scan(&sid, &fi, &spec); err != nil {
			return "", fmt.Errorf("thread %d: %w", thread, err)
		}
		if fi != "" {
			if err := json.Unmarshal([]byte(fi), &first); err != nil {
				return "", fmt.Errorf("session %d: first input: %w", sid, err)
			}
		}
		if first.Dir != "" {
			return first.Dir, nil
		}
		if spec != "" {
			if err := json.Unmarshal([]byte(spec), &specified); err != nil {
				return "", fmt.Errorf("session %d: spec: %w", sid, err)
			}
		}
		if specified.Spec.Dir != "" {
			return specified.Spec.Dir, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("thread %d: %w", thread, err)
	}
	return "", nil
}
