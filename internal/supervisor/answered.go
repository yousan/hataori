package supervisor

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

// answerMax caps, in bytes, the part of a wake's answer that its line
// quotes.
const answerMax = 200

// quietAnswer is the final answer of a wake that needs nobody: nothing is posted
// for it (design, section 7.1, "What it posts").
const quietAnswer = "(quiet)"

// Answered is what a person's input that resumes the conversation of
// supervisor thread id is given before it (core.Core.Preface): one line per
// wake that the thread answered in a conversation of its own since that
// conversation's last turn, oldest first, each saying when the wake came,
// the threads it named, and how its answer began, or that it gave none or
// that it was quiet (#676). Which conversation a turn ran in is recorded
// with the turn (turns.aside). A last turn whose input was never delivered
// does not count: the conversation never got the lines it was given with.
// It is read in the Send's transaction, so a later input, or the same one
// given again after a restart, finds the lines it should. An ordinary
// thread gets nothing.
func Answered(ctx context.Context, tx *store.Tx, id control.ThreadID) (string, error) {
	fail := func(err error) (string, error) {
		return "", fmt.Errorf("supervisor: the wakes answered: %w", err)
	}
	var confined bool
	if err := tx.QueryRowContext(ctx, `SELECT confined FROM threads WHERE id = ?`, int64(id)).Scan(&confined); err != nil {
		return fail(err)
	}
	if !confined {
		return "", nil
	}
	var since int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(t.id), 0) FROM turns t JOIN sessions s ON s.id = t.session_id
		WHERE s.thread_id = ? AND t.aside = 0 AND coalesce(t.reason, '') NOT IN (?, ?)`, int64(id), string(agent.NotDelivered), string(agent.NotTaken)).Scan(&since); err != nil {
		return fail(err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.id, t.started_at, coalesce(t.status, ''), coalesce(t.reason, '') FROM turns t
		JOIN sessions s ON s.id = t.session_id WHERE s.thread_id = ? AND t.aside = 1 AND t.id > ?
		AND substr(t.trigger, 1, ?) = ? ORDER BY t.id`, int64(id), since, len(WakeKeyPrefix), WakeKeyPrefix)
	if err != nil {
		return fail(err)
	}
	type turn struct {
		id, ms         int64
		status, reason string
	}
	var turns []turn
	for rows.Next() {
		var t turn
		if err := rows.Scan(&t.id, &t.ms, &t.status, &t.reason); err != nil {
			rows.Close()
			return fail(err)
		}
		turns = append(turns, t)
	}
	if err := rows.Close(); err != nil {
		return fail(err)
	}
	var b strings.Builder
	for _, t := range turns {
		a := WakeAnswered{At: time.UnixMilli(t.ms), Failed: t.status == "failure", Reason: t.reason}
		var text sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT json_extract(data, '$.text') FROM events WHERE thread_id = ? AND kind = 'input'
			AND json_extract(data, '$.turn') = ? ORDER BY pos LIMIT 1`, int64(id), t.id).Scan(&text)
		if err != nil && err != sql.ErrNoRows {
			return fail(err)
		}
		a.Threads, a.Reminders = Named(text.String), RemindersNamed(text.String)
		a.Decided = strings.Contains(text.String, "\nDecided by a person:\n")
		// The last final text between the turn's start and its end.
		var from, to sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT min(CASE WHEN kind = 'turn_started' THEN pos END), max(CASE WHEN kind = 'turn_ended' THEN pos END)
			FROM events WHERE thread_id = ? AND kind IN ('turn_started', 'turn_ended') AND json_extract(data, '$.turn') = ?`,
			int64(id), t.id).Scan(&from, &to); err != nil {
			return fail(err)
		}
		if !to.Valid {
			to = sql.NullInt64{Int64: math.MaxInt64, Valid: true}
		}
		var answer sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT json_extract(data, '$.text') FROM events WHERE thread_id = ? AND kind = 'text'
			AND pos > ? AND pos < ? AND json_extract(data, '$.final') ORDER BY pos DESC LIMIT 1`, int64(id), from.Int64, to.Int64).Scan(&answer)
		if err != nil && err != sql.ErrNoRows {
			return fail(err)
		}
		a.Answer = answer.String
		b.WriteString(a.Line() + "\n")
	}
	return b.String(), nil
}

// WakeAnswered is one wake that a supervisor answered in a conversation of
// its own.
type WakeAnswered struct {
	At      time.Time
	Threads []control.ThreadID // the threads its digest named
	Decided bool               // it gave decisions of the supervisor's requests
	// Reminders are the reminders it gave (#649).
	Reminders []int64
	Answer    string // its final answer
	Failed    bool   // its turn failed, for Reason
	Reason    string
}

// Line is the wake as the line its conversation's next turn is given.
func (a WakeAnswered) Line() string {
	var on []string
	switch n := len(a.Threads); n {
	case 0:
	case 1:
		on = append(on, fmt.Sprintf("thread %d", a.Threads[0]))
	default:
		ns := make([]string, n)
		for i, t := range a.Threads {
			ns[i] = strconv.FormatInt(int64(t), 10)
		}
		on = append(on, "threads "+strings.Join(ns[:n-1], ", ")+" and "+ns[n-1])
	}
	for _, n := range a.Reminders {
		on = append(on, fmt.Sprintf("reminder %d", n))
	}
	if a.Decided {
		on = append(on, "decisions of your requests")
	}
	what := ""
	if len(on) > 0 {
		what = " on " + strings.Join(on, " and ")
	}
	first, _, _ := strings.Cut(strings.TrimSpace(a.Answer), "\n")
	first = strings.TrimSpace(first)
	var how string
	switch {
	case a.Failed:
		how = "its turn failed"
		if a.Reason != "" {
			how += " (" + a.Reason + ")"
		}
	case strings.TrimSpace(a.Answer) == quietAnswer:
		how = "it answered " + quietAnswer + ", and nothing was posted"
	case first == "":
		how = "it gave no answer"
	default:
		if len(first) > answerMax {
			cut := answerMax
			for cut > 0 && !utf8.RuneStart(first[cut]) {
				cut--
			}
			first = first[:cut] + "…"
		}
		how = fmt.Sprintf("its answer began: %q", first)
	}
	return fmt.Sprintf("hataori, meanwhile: the wake of %s%s was answered in a conversation of its own; %s.",
		a.At.Format("2006-01-02 15:04 MST"), what, how)
}
