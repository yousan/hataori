package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

// outsideProposal is read for each call, rather than when a long-lived
// connection is bound. Approved proposals have no ProposeOutside policy:
// the person's decision authorizes exactly that stored call.
func (a *api) outsideProposal(ctx context.Context) (bool, error) {
	if a.caller.Propose {
		return true, nil
	}
	if !a.caller.ProposeOutside {
		return false, nil
	}
	marks, err := store.BoundMarks(ctx, a.c.Store.DB, a.caller.AsksIn, a.caller.ConversationKey)
	return len(marks) != 0, err
}

var errOutsideChanged = errors.New("the conversation became marked while the call was checked")

func (a *api) outsideCheck(ctx context.Context, tx *store.Tx) error {
	if !a.caller.ProposeOutside {
		return nil
	}
	marks, err := store.BoundMarks(ctx, tx, a.caller.AsksIn, a.caller.ConversationKey)
	if err != nil {
		return err
	}
	if len(marks) != 0 {
		return errOutsideChanged
	}
	return nil
}

// inputMark commits provenance with input acceptance, including input
// before an agent's first conversation ID and a tool read during a turn.
func inputMark(ctx context.Context, tx *store.Tx, id control.ThreadID, sid int64, sources []control.OutsideText) (string, error) {
	var key string
	if err := tx.QueryRowContext(ctx, `SELECT outside_key FROM sessions WHERE id = ?`, sid).Scan(&key); err != nil {
		return "", err
	}
	if err := store.MarkConversation(ctx, tx, id, key, sources); err != nil {
		return "", err
	}
	marks, err := store.ConversationMarks(ctx, tx, id, key)
	if err != nil || len(marks) == 0 {
		return "", err
	}
	return store.OutsideLine(marks) + "\n", nil
}

func (a *api) requestMarks(ctx context.Context, tx *store.Tx, id control.ThreadID, data map[string]any) error {
	if id == 0 {
		return nil
	}
	marks, err := store.BoundMarks(ctx, tx, id, a.caller.ConversationKey)
	if len(marks) != 0 {
		data["outside"] = marks
	}
	return err
}

func (a *api) markSummary(ctx context.Context, t *control.ThreadInfo) error {
	var err error
	if t.Outside, err = store.CurrentMarks(ctx, a.c.Store.DB, t.ID); err != nil {
		return err
	}
	var raw string
	err = a.c.Store.DB.QueryRowContext(ctx, `SELECT u.outside FROM turns u JOIN sessions s ON s.id = u.session_id
		WHERE s.thread_id = ? ORDER BY u.id DESC LIMIT 1`, int64(t.ID)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), &t.LastOutside)
}
