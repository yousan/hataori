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

// These records are private provenance, not provider-supplied identities or
// public window fields. See docs/design/limit-observations.md.
type limitObservation struct {
	Window    control.LimitWindow
	At, Every int64 // milliseconds
}

type limitHistory struct {
	Machine, Agent, Directory string
	Windows                   []limitObservation
	LastReportAt              *int64
}

// recordLimits commits the sequence, session provenance and independent account
// windows together. Limits belong to no turn and write no control event.
func (m *Machine) recordLimits(ctx context.Context, tx *store.Tx, sid int64, name string, e agent.Event) error {
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET last_seq = ? WHERE id = ?`, e.Seq, sid); err != nil {
		return err
	}
	if e.Limits == nil || e.Limits.At.IsZero() {
		return nil
	}
	l := e.Limits
	login, err := m.login(ctx, tx, name, sid)
	if err != nil {
		return err
	}
	var machine, historyJSON string
	if err := tx.QueryRowContext(ctx, `SELECT t.machine,s.limit_observations FROM sessions s JOIN threads t ON t.id=s.thread_id WHERE s.id=?`, sid).Scan(&machine, &historyJSON); err != nil {
		return err
	}
	history := limitHistory{Machine: machine, Agent: name, Directory: login.Dir}
	if historyJSON != "" {
		var old limitHistory
		if err := json.Unmarshal([]byte(historyJSON), &old); err != nil {
			return fmt.Errorf("session: limit history: %w", err)
		}
		if old.Machine == machine && old.Agent == name && old.Directory == login.Dir {
			history = old
		}
	}
	if history.LastReportAt != nil && l.At.UnixMilli() < *history.LastReportAt {
		return nil // observation age is not a receipt-order watermark
	}
	received := l.At.UnixMilli()
	history.LastReportAt = &received
	var observations, windowsJSON string
	var oldAt, oldEvery, reportAt int64
	err = tx.QueryRowContext(ctx, `SELECT observations,windows,at,every,report_at FROM agent_limits WHERE machine=? AND agent=? AND config_dir=?`, machine, name, login.Dir).Scan(&observations, &windowsJSON, &oldAt, &oldEvery, &reportAt)
	kept := []limitObservation{}
	if err == nil {
		if err := json.Unmarshal([]byte(observations), &kept); err != nil {
			return fmt.Errorf("session: limit observations: %w", err)
		}
		if len(kept) == 0 {
			var legacy []control.LimitWindow
			if err := json.Unmarshal([]byte(windowsJSON), &legacy); err != nil {
				return err
			}
			for _, w := range legacy {
				kept = append(kept, limitObservation{w, oldAt, oldEvery})
			}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for _, w := range l.Windows {
		cw := control.LimitWindow{Name: w.Name, Minutes: w.Minutes, Used: w.Used}
		if !w.Resets.IsZero() {
			r := w.Resets
			cw.Resets = &r
		}
		candidate := limitObservation{cw, l.At.UnixMilli(), l.Every.Milliseconds()}
		hi, ki := observationIndex(history.Windows, w.Name), observationIndex(kept, w.Name)
		if hi >= 0 {
			before := history.Windows[hi]
			if candidate.At < before.At {
				continue
			}
			if l.Cached {
				if olderLimitReset(cw, before.Window) {
					continue
				}
				if sameLimitWindow(cw, before.Window) {
					candidate = before
					candidate.Every = min(candidate.Every, l.Every.Milliseconds())
				}
			}
		} else if l.Cached && ki >= 0 && sameLimitWindow(cw, kept[ki].Window) {
			// An upgrade or newly seen source cannot freshen an identical
			// legacy/account value whose earlier provenance is all we know.
			candidate.At = min(candidate.At, kept[ki].At)
			candidate.Every = min(candidate.Every, kept[ki].Every)
		}
		if hi < 0 {
			history.Windows = append(history.Windows, candidate)
		} else {
			history.Windows[hi] = candidate
		}
		if ki < 0 {
			kept = append(kept, candidate)
		} else if newerLimitObservation(candidate, kept[ki]) {
			kept[ki] = candidate
		} else if candidate.At == kept[ki].At && sameLimitWindow(candidate.Window, kept[ki].Window) {
			// The same observation may acquire a shorter expected cadence
			// without acquiring a newer age or changing an equal-time value.
			kept[ki].Every = min(kept[ki].Every, candidate.Every)
		}
	}
	ws := []control.LimitWindow{}
	at, every := l.At.UnixMilli(), l.Every.Milliseconds()
	for i, o := range kept {
		ws = append(ws, o.Window)
		if i == 0 {
			at, every = o.At, o.Every
		} else {
			at = min(at, o.At)
			every = min(every, o.Every)
		}
	}
	hb, err := json.Marshal(history)
	if err != nil {
		return fmt.Errorf("session: encode limit history: %w", err)
	}
	ob, err := json.Marshal(kept)
	if err != nil {
		return fmt.Errorf("session: encode limit observations: %w", err)
	}
	wb, err := json.Marshal(ws)
	if err != nil {
		return fmt.Errorf("session: encode limit windows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET limit_observations=? WHERE id=?`, string(hb), sid); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_limits (machine,agent,config_dir,at,every,windows,observations,report_at) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT (machine,agent,config_dir) DO UPDATE SET at=excluded.at,every=excluded.every,windows=excluded.windows,observations=excluded.observations,report_at=excluded.report_at`, machine, name, login.Dir, at, every, string(wb), string(ob), max(reportAt, l.At.UnixMilli()))
	if err != nil {
		return fmt.Errorf("session: limits: %w", err)
	}
	return nil
}

func observationIndex(ws []limitObservation, name string) int {
	for i, w := range ws {
		if w.Window.Name == name {
			return i
		}
	}
	return -1
}

// Captured reset values can jitter by one second between fresh reads.
const limitResetTolerance = time.Second

func sameLimitWindow(a, b control.LimitWindow) bool {
	if a.Name != b.Name || a.Minutes != b.Minutes || a.Used != b.Used {
		return false
	}
	if a.Resets == nil || b.Resets == nil {
		return a.Resets == nil && b.Resets == nil
	}
	d := a.Resets.Sub(*b.Resets)
	return d >= -limitResetTolerance && d <= limitResetTolerance
}

func olderLimitReset(a, b control.LimitWindow) bool {
	if a.Minutes != b.Minutes || b.Resets == nil {
		return false
	}
	return a.Resets == nil || a.Resets.Before(b.Resets.Add(-limitResetTolerance))
}

func newerLimitObservation(a, b limitObservation) bool {
	if olderLimitReset(a.Window, b.Window) {
		return false
	}
	if a.Window.Minutes == b.Window.Minutes && a.Window.Resets != nil && b.Window.Resets != nil && a.Window.Resets.After(b.Window.Resets.Add(limitResetTolerance)) {
		return true
	}
	return a.At > b.At
}
