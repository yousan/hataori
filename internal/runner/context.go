package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/control"
)

// Marks around the earlier context a new conversation of a thread starts
// with: one that replaces a conversation the agent lost, or that a reopen
// starts because none could be resumed.
const (
	contextStart = "[This thread's earlier conversation is not there, so a new one starts here. The chat thread holds the earlier exchange; its last part follows, oldest first, the oldest part may be cut]\n"
	contextEnd   = "[End of the earlier context. The new message follows.]\n\n"
)

func (r *Runner) contextBytes() int {
	if r.ContextBytes > 0 {
		return r.ContextBytes
	}
	return 8192
}

// earlier returns the thread's inputs and final answers, oldest first, cut
// to the context cap and marked as earlier context; "" when there are none.
// They are read newest first, and no more than the cap needs.
// Earlier is earlier, for the core's session that starts a woken thread
// again (core.EarlierRunner).
func (r *Runner) Earlier(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, thread control.ThreadID) (string, error) {
	return r.earlier(ctx, q, thread)
}

func (r *Runner) earlier(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, thread control.ThreadID) (string, error) {
	rows, err := q.QueryContext(ctx, `SELECT kind, data FROM events WHERE thread_id = ?
		AND (kind IN ('input', 'answer') OR (kind = 'text' AND json_extract(data, '$.final'))) ORDER BY pos DESC`, int64(thread))
	if err != nil {
		return "", fmt.Errorf("runner: %w", err)
	}
	max := r.contextBytes()
	var lines []string
	n := 0
	for n < max && rows.Next() {
		var kind string
		var data []byte
		if err := rows.Scan(&kind, &data); err != nil {
			rows.Close()
			return "", fmt.Errorf("runner: %w", err)
		}
		var d struct {
			Text   string `json:"text"`
			Before string `json:"before"`
			After  string `json:"after"`
		}
		json.Unmarshal(data, &d)
		who := "user"
		if kind == "text" {
			who = "agent"
		}
		// An input as the agent got it, with the text plugins added (#351).
		l := who + ": " + control.Input{Before: d.Before, Text: d.Text, After: d.After}.ForAgent() + "\n"
		lines = append(lines, l)
		n += len(l)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", fmt.Errorf("runner: %w", err)
	}
	slices.Reverse(lines)
	return markContext(strings.Join(lines, ""), max), nil
}

// markContext keeps the last max bytes of s, starting at a whole
// character, between the context marks; "" when s is empty.
func markContext(s string, max int) string {
	if s == "" {
		return ""
	}
	if len(s) > max {
		s = s[len(s)-max:]
		for len(s) > 0 && !utf8.RuneStart(s[0]) {
			s = s[1:]
		}
	}
	return contextStart + s + contextEnd
}
