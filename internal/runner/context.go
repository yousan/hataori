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
	rows, err := q.QueryContext(ctx, `SELECT kind, data, (SELECT confined FROM threads WHERE id = events.thread_id) FROM events WHERE thread_id = ?
		AND (kind IN ('input', 'answer') OR (kind = 'text' AND json_extract(data, '$.final')))
		AND NOT EXISTS (SELECT 1 FROM turns u WHERE u.aside = 1 AND u.id =
			(SELECT json_extract(st.data, '$.turn') FROM events st WHERE st.thread_id = events.thread_id
			 AND st.kind = 'turn_started' AND st.pos <= events.pos ORDER BY st.pos DESC LIMIT 1))
		ORDER BY pos DESC`, int64(thread))
	if err != nil {
		return "", fmt.Errorf("runner: %w", err)
	}
	max := r.contextBytes()
	var lines []string
	n := 0
	confined := false
	for n < max && rows.Next() {
		var kind string
		var data []byte
		if err := rows.Scan(&kind, &data, &confined); err != nil {
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
	text := strings.Join(lines, "")
	if !confined {
		return markContext(text, max), nil
	}
	// Quote every line, including alternative line separators. No line of
	// the old exchange can masquerade as the daemon's enclosing context.
	text = strings.TrimSuffix(strings.TrimPrefix(markContext(text, max), contextStart), contextEnd)
	if text == "" {
		return "", nil
	}
	text = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u0085", "\n", "\u2028", "\n", "\u2029", "\n").Replace(text)
	text = "> " + strings.ReplaceAll(strings.TrimSuffix(text, "\n"), "\n", "\n> ") + "\n"
	return contextStart + text + contextEnd, nil
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
