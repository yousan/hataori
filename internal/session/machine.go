package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

// Machine applies Step to sessions in a store.
type Machine struct {
	Store *store.Store
	// Now is the clock; time.Now when nil. Zone is the time zone whose
	// calendar day the tokens of a thread are counted in; time.Local when
	// nil.
	Now  func() time.Time
	Zone *time.Location
	// Typist is the person a turn typed at the agent's terminal is
	// recorded as: the owner, since only the owner can reach a session's
	// terminal (`hataori attach`, the dashboard, a window taken in; design,
	// section 5.2). Empty: the typed prompt's input event names nobody.
	Typist control.Principal
	// LoginDir gives the configuration directory that holds the login of a
	// session of the agent started from spec (agent.LoginDir); the spec's
	// own when nil.
	LoginDir func(agentName string, spec agent.Spec) string

	// beforeCommit, when set, runs inside the transaction after everything
	// is written; an error rolls it all back. For tests.
	beforeCommit func() error
}

// Apply feeds one input to a session and commits the new state, the turn
// records and the events together. An agent event whose sequence number
// the session has already seen is ignored and writes nothing. A refused
// input returns an error wrapping ErrRefused and writes nothing.
func (m *Machine) Apply(ctx context.Context, sessionID int64, in Input) error {
	return m.Store.Update(ctx, func(tx *store.Tx) error {
		if err := m.ApplyTx(ctx, tx, sessionID, in); err != nil {
			return err
		}
		if m.beforeCommit != nil {
			return m.beforeCommit()
		}
		return nil
	})
}

// ApplyTx is Apply inside a transaction the caller owns, so that a caller
// can commit other rows, such as the inbox entry of the call, with it.
func (m *Machine) ApplyTx(ctx context.Context, tx *store.Tx, sessionID int64, in Input) error {
	var (
		state     string
		lastSeq   uint64
		agentName string
		thread    int64
		id        control.ThreadID
		aside     sql.NullString
	)
	err := tx.QueryRowContext(ctx, `
		SELECT s.state, s.last_seq, s.agent, s.thread_id, s.set_aside
		FROM sessions s WHERE s.id = ?`, sessionID).Scan(&state, &lastSeq, &agentName, &thread, &aside)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("session: no session %d", sessionID)
	}
	if err != nil {
		return fmt.Errorf("session: load %d: %w", sessionID, err)
	}
	id = control.ThreadID(thread)
	if in.Kind == AgentEvent && in.Agent.Kind == agent.TurnStarted && in.Agent.StartedBy == agent.ByTerminal {
		in.Principal = m.Typist // the person at the terminal
	}
	if in.Kind == AgentEvent && in.Agent.Seq <= lastSeq {
		return nil // seen before
	}
	if in.Kind == AgentEvent && in.Agent.Kind == agent.Limits {
		return recordLimits(ctx, tx, sessionID, agentName, in.Agent)
	}

	snap := Snapshot{State: control.State(state), Aside: aside.Valid}
	var turnID int64
	var t OpenTurn
	err = tx.QueryRowContext(ctx, `SELECT id, started_by, principal, from_id FROM turns WHERE session_id = ? AND ended_at IS NULL`,
		sessionID).Scan(&turnID, &t.By, &t.Principal, &t.From)
	switch {
	case err == nil:
		snap.Turn = &t
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("session: load turn: %w", err)
	}

	r, err := Step(snap, in)
	if err != nil {
		return err
	}
	// The turn a Usage event's counts are of: the open one, or for a Late
	// report the turn that ended last, whose last message the agent
	// recorded after its end (#675).
	usageTurn := turnID
	if in.Kind == AgentEvent && in.Agent.Kind == agent.Usage && in.Agent.Late {
		usageTurn = 0
		err := tx.QueryRowContext(ctx, `SELECT id FROM turns WHERE session_id = ? AND ended_at IS NOT NULL ORDER BY id DESC LIMIT 1`,
			sessionID).Scan(&usageTurn)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("session: load turn: %w", err)
		}
	}
	clock := time.Now
	if m.Now != nil {
		clock = m.Now
	}
	at := clock()
	now := at.UnixMilli()

	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET state = ?, updated_at = ? WHERE id = ?`,
		string(r.State), now, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET state = ?, updated_at = ? WHERE id = ?`,
		string(r.State), now, thread); err != nil {
		return err
	}
	switch r.State {
	case control.Asleep, control.Failed, control.Closed:
		// A conversation set aside for one input's own (sessions.set_aside)
		// is the session's again: the next wake resumes it.
		if aside.Valid {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET agent_session_id = set_aside, set_aside = NULL WHERE id = ?`,
				sessionID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM session_files WHERE session_id = ? AND aside = 1`, sessionID); err != nil {
				return err
			}
		}
	}
	if in.Kind == AgentEvent {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET last_seq = ? WHERE id = ?`, in.Agent.Seq, sessionID); err != nil {
			return err
		}
		if e := in.Agent; e.Kind == agent.SessionReady {
			// A resume or a clear reports no model: the one reported at
			// the start stays.
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET agent_session_id = ?,
				model = coalesce(nullif(?, ''), model), effort = coalesce(nullif(?, ''), effort) WHERE id = ?`,
				e.SessionID, e.Model, e.Effort, sessionID); err != nil {
				return err
			}
		}
	}
	if r.End != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE turns SET ended_at = ?, status = ?, reason = ?, background = ? WHERE id = ?`,
			now, r.End.Outcome, r.End.Reason, r.End.Background, turnID); err != nil {
			return err
		}
	}
	endedTurn := turnID
	if r.Start != nil {
		// Whether it runs in the conversation set aside's place is known
		// from before: r.State may have ended that conversation.
		res, err := tx.ExecContext(ctx, `INSERT INTO turns (session_id, started_by, principal, from_id, trigger, started_at, aside) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			sessionID, r.Start.By, string(r.Start.Principal), r.Start.From, r.Key, now, aside.Valid)
		if err != nil {
			return err
		}
		if turnID, err = res.LastInsertId(); err != nil {
			return err
		}
	}

	for _, o := range r.Out {
		data := map[string]any{"session": sessionID}
		for k, v := range o.Data {
			data[k] = v
		}
		if req, ok := data["request"].(string); ok {
			// Clients name a request by its handle; the agent's id is
			// unique only in its session.
			data["request"] = string(RequestHandle(sessionID, req))
		}
		switch o.Kind {
		case KindTurnEnded:
			data["turn"] = endedTurn
		case KindTurnStarted, KindInput, KindAnswer:
			data["turn"] = turnID
		case KindUsage:
			if usageTurn != 0 {
				data["turn"] = usageTurn
			}
		}
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		if _, err := tx.Append(ctx, control.Event{Thread: id, Time: at, Kind: o.Kind, Data: b}); err != nil {
			return err
		}
	}
	if in.Kind == AgentEvent && in.Agent.Kind == agent.Usage && in.Agent.Usage != nil {
		if err := recordUsage(ctx, tx, thread, usageTurn, *in.Agent.Usage, DayOf(at, m.Zone)); err != nil {
			return err
		}
	}
	if err := recordRequest(ctx, tx, sessionID, in, r, now); err != nil {
		return err
	}
	if r.End != nil {
		return m.recordLogin(ctx, tx, agentName, sessionID, thread, id, *r.End)
	}
	return nil
}

// DayOf is the calendar day of t in zone (time.Local when nil), as the
// threads table keeps it.
func DayOf(t time.Time, zone *time.Location) string {
	if zone == nil {
		zone = time.Local
	}
	return t.In(zone).Format(time.DateOnly)
}

// recordUsage keeps the thread's token counts. The agent reports each
// count as the total of the turn so far, so what a report adds is its
// total less the turn's highest before it, and that is counted in the day
// of the report: a turn that runs past midnight counts in both days. A
// count that goes down adds nothing, and the turn keeps its highest. A
// report outside a turn belongs to no turn's total: only its context use
// is kept.
func recordUsage(ctx context.Context, tx *store.Tx, thread, turn int64, u agent.UsageCount, day string) error {
	if u.ContextSize > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE threads SET context_used = ?, context_size = ? WHERE id = ?`,
			u.ContextUsed, u.ContextSize, thread); err != nil {
			return fmt.Errorf("session: usage: %w", err)
		}
	}
	if turn == 0 {
		return nil
	}
	total := u.Input + u.Output + u.CacheRead + u.CacheWrite
	var before int64
	if err := tx.QueryRowContext(ctx, `SELECT tokens FROM turns WHERE id = ?`, turn).Scan(&before); err != nil {
		return fmt.Errorf("session: usage: %w", err)
	}
	highest := max(total, before)
	if _, err := tx.ExecContext(ctx, `UPDATE turns SET tokens = ? WHERE id = ?`, highest, turn); err != nil {
		return fmt.Errorf("session: usage: %w", err)
	}
	added := highest - before
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET tokens_turn = ?,
		tokens_today = CASE WHEN tokens_day = ? THEN tokens_today + ? ELSE ? END, tokens_day = ? WHERE id = ?`,
		highest, day, added, added, day, thread); err != nil {
		return fmt.Errorf("session: usage: %w", err)
	}
	return nil
}

// recordLimits keeps an agent's remaining usage, unless a report made
// later, by another of its sessions, is kept already. It belongs to no turn
// and changes no state, so it writes no event.
func recordLimits(ctx context.Context, tx *store.Tx, sessionID int64, agentName string, e agent.Event) error {
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET last_seq = ? WHERE id = ?`, e.Seq, sessionID); err != nil {
		return err
	}
	if e.Limits == nil {
		return nil
	}
	l := *e.Limits
	ws := []control.LimitWindow{}
	for _, w := range l.Windows {
		cw := control.LimitWindow{Name: w.Name, Minutes: w.Minutes, Used: w.Used}
		if !w.Resets.IsZero() {
			r := w.Resets
			cw.Resets = &r
		}
		ws = append(ws, cw)
	}
	b, err := json.Marshal(ws)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_limits (agent, at, every, windows) VALUES (?, ?, ?, ?)
		ON CONFLICT (agent) DO UPDATE SET at = excluded.at, every = excluded.every, windows = excluded.windows
		WHERE excluded.at >= agent_limits.at`,
		agentName, l.At.UnixMilli(), l.Every.Milliseconds(), string(b)); err != nil {
		return fmt.Errorf("session: limits: %w", err)
	}
	return nil
}

// Request states, in the requests table.
const (
	RequestOpen      = "open"
	RequestAnswered  = "answered"
	RequestWithdrawn = "withdrawn" // by the agent
	RequestClosed    = "closed"    // the agent's process ended while it was open
)

// RequestHandle is how the control API names the request agentID of a
// session.
func RequestHandle(sessionID int64, agentID string) control.RequestID {
	return control.RequestID(fmt.Sprintf("%d:%s", sessionID, agentID))
}

// recordRequest keeps the session's requests: a question or approval from
// the agent opens one, an answer or the agent's withdrawal closes it, and
// so does the end of the agent's process. An answer to a request that is
// not open is refused.
func recordRequest(ctx context.Context, tx *store.Tx, sessionID int64, in Input, r Result, now int64) error {
	closeOpen := func(state, req string) (int64, error) {
		q := `UPDATE requests SET state = ?, closed_at = ? WHERE session_id = ? AND state = 'open'`
		args := []any{state, now, sessionID}
		if req != "" {
			q += ` AND request = ?`
			args = append(args, req)
		}
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return 0, fmt.Errorf("session: request: %w", err)
		}
		return res.RowsAffected()
	}
	switch {
	case in.Kind == AgentEvent && in.Agent.Kind == agent.Request:
		q := in.Agent.Request
		choices, _ := json.Marshal(append([]string{}, q.Choices...))
		_, err := tx.ExecContext(ctx, `INSERT INTO requests (session_id, request, handle, approval, text, choices, state, opened_at)
			VALUES (?, ?, ?, ?, ?, ?, 'open', ?)
			ON CONFLICT (session_id, request) DO UPDATE SET approval = excluded.approval, text = excluded.text,
				choices = excluded.choices, state = 'open', opened_at = excluded.opened_at, closed_at = NULL`,
			sessionID, q.ID, string(RequestHandle(sessionID, q.ID)), q.Approval, q.Text, string(choices), now)
		if err != nil {
			return fmt.Errorf("session: request: %w", err)
		}
	case in.Kind == AgentEvent && in.Agent.Kind == agent.RequestWithdrawn:
		id := ""
		if in.Agent.Request != nil {
			id = in.Agent.Request.ID
		}
		_, err := closeOpen(RequestWithdrawn, id)
		return err
	case in.Kind == Answer:
		n := int64(0)
		if in.Request != "" {
			var err error
			if n, err = closeOpen(RequestAnswered, in.Request); err != nil {
				return err
			}
		}
		if n == 0 {
			return fmt.Errorf("session: answer to %q, which is not open: %w", in.Request, ErrRefused)
		}
	case in.Kind == Exit:
		_, err := closeOpen(RequestClosed, "")
		return err
	}
	return nil
}
