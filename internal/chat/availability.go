package chat

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/yousan/hataori/internal/control"
)

// supervisorAvailability reports provider outages of the supervisor's own
// thread. The event log is the source of truth so restarts retain both
// deduplication and recovery. Nothing here interprets a provider's message.
func (f *FrontEnd) supervisorAvailability(ctx context.Context, r *reader, e control.Event, d eventData) (line string, handled bool, err error) {
	unavailable := d.Outcome == "failure" && d.Reason == "unavailable"
	if !unavailable && d.Outcome != "completion" {
		return "", false, nil
	}
	t, err := f.svThread(ctx, r, e.Thread)
	if err != nil || !t.own {
		return "", false, err
	}
	rows, err := f.Outbox.Store.DB.QueryContext(ctx, `SELECT data FROM events
  WHERE thread_id = ? AND kind = 'turn_ended' AND pos < ? ORDER BY pos DESC`, int64(e.Thread), e.Pos)
	if err != nil {
		return "", false, fmt.Errorf("chat: reading supervisor availability: %w", err)
	}
	defer rows.Close()
	var previous *eventData
	lost := false
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return "", false, err
		}
		var before eventData
		if err := json.Unmarshal(raw, &before); err != nil {
			return "", false, fmt.Errorf("chat: reading supervisor turn end: %w", err)
		}
		if before.Outcome == "completion" {
			break
		}
		if before.Outcome == "failure" {
			if previous == nil {
				previous = &before
			}
			lost = lost || before.Reason == "unavailable"
			if unavailable {
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	if unavailable {
		if previous != nil && previous.Reason == d.Reason && previous.Detail == d.Detail {
			return "", true, nil
		}
		return failureDetail("The supervisor is unavailable; worker management is unavailable.", d.Detail), true, nil
	}
	if lost {
		return "The supervisor is available again; worker management has resumed.", true, nil
	}
	return "", false, nil
}

func failureDetail(line, detail string) string {
	if detail != "" {
		return line + "\n" + detail
	}
	return line
}
