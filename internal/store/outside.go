package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/control"
)

// Rows is the read operation shared by a database and its transaction.
type Rows interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// ConversationMarks reads the mark of one logical conversation. The own
// conversation's key is empty; each own-thread wake uses its input key.
func ConversationMarks(ctx context.Context, q Rows, thread control.ThreadID, key string) ([]control.OutsideText, error) {
	rows, err := q.QueryContext(ctx, `SELECT source FROM conversation_marks WHERE thread_id = ? AND key = ? ORDER BY source`, int64(thread), key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []control.OutsideText
	for rows.Next() {
		var raw string
		var source control.OutsideText
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &source); err != nil {
			return nil, err
		}
		out = append(out, source)
	}
	return out, rows.Err()
}

// CurrentMarks reads the mark of the conversation the current session is
// using, even before the agent has reported a conversation ID.
func CurrentMarks(ctx context.Context, q Rows, thread control.ThreadID) ([]control.OutsideText, error) {
	rows, err := q.QueryContext(ctx, `SELECT outside_key FROM sessions WHERE thread_id = ? ORDER BY id DESC LIMIT 1`, int64(thread))
	if err != nil {
		return nil, err
	}
	key := ""
	if rows.Next() {
		err = rows.Scan(&key)
	}
	err = errorsClose(rows, err)
	if err != nil {
		return nil, err
	}
	return ConversationMarks(ctx, q, thread, key)
}

// BoundMarks is the launch's conversation when key is set; otherwise the
// thread's current one. A wake's tools must not inherit the restored main
// conversation's permissions while that wake's process is stopping.
func BoundMarks(ctx context.Context, q Rows, thread control.ThreadID, key *string) ([]control.OutsideText, error) {
	if key != nil {
		return ConversationMarks(ctx, q, thread, *key)
	}
	return CurrentMarks(ctx, q, thread)
}

// OutsideQuota checks the shared allowance inside the same transaction
// that stores a proposal or reminder. Positive limit is its allowance;
// negative permits none, zero means no such policy.
func OutsideQuota(ctx context.Context, tx *Tx, scope string, at time.Time, limit int) error {
	if limit == 0 {
		return nil
	}
	into := time.Duration(at.Minute())*time.Minute + time.Duration(at.Second())*time.Second + time.Duration(at.Nanosecond())
	hour := at.Add(-into)
	var used int
	err := tx.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM events WHERE scope = ? AND kind = 'proposal_made' AND time >= ? AND time < ?
		 AND json_array_length(json_extract(data, '$.outside')) > 0) +
		(SELECT count(*) FROM supervisor_reminders WHERE scope = ? AND made >= ? AND made < ? AND json_array_length(outside) > 0)`,
		scope, hour.UnixMilli(), hour.Add(time.Hour).UnixMilli(), scope, hour.UnixMilli(), hour.Add(time.Hour).UnixMilli()).Scan(&used)
	if err != nil {
		return err
	}
	if used >= max(0, limit) {
		return fmt.Errorf("marked conversations used %d of %d sends allowed this hour: %w", used, max(0, limit), control.ErrForbidden)
	}
	return nil
}

func errorsClose(rows *sql.Rows, err error) error {
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	return err
}

// MarkConversation is only for daemon components giving text. It must run
// inside the transaction that stores that text for delivery; rollback
// then gives neither text nor mark. Nothing ever removes these rows.
func MarkConversation(ctx context.Context, tx *Tx, thread control.ThreadID, key string, sources []control.OutsideText) error {
	for _, source := range sources {
		if source.Kind == "" || source.ID == "" {
			return fmt.Errorf("outside text needs a kind and an id")
		}
		b, err := json.Marshal(source)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_marks (thread_id, key, source) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, int64(thread), key, string(b)); err != nil {
			return err
		}
	}
	if len(sources) == 0 {
		return nil
	}
	marks, err := ConversationMarks(ctx, tx, thread, key)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(marks)
	// A tool read or an input in the middle of a turn marks that turn too.
	_, err = tx.ExecContext(ctx, `UPDATE turns SET outside = ? WHERE ended_at IS NULL AND session_id IN
		(SELECT id FROM sessions WHERE thread_id = ? AND outside_key = ?)`, string(b), int64(thread), key)
	return err
}

// OutsideLine is daemon-written context, and the notice shown with a
// proposal. Quote source identifiers so none can impersonate this notice.
func OutsideLine(sources []control.OutsideText) string {
	if len(sources) == 0 {
		return ""
	}
	var names []string
	for _, source := range sources {
		names = append(names, fmt.Sprintf("%q %q", source.Kind, source.ID))
	}
	return "hataori: this conversation is marked by outside text: " + strings.Join(names, ", ") + ". Starts, sends and answers require a person's approval, including under act."
}
