package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

// PendingInput keeps the input's caller beside what the agent receives.
// The outer fields remain readable as control.Input by older runners.
type PendingInput struct {
	control.Input
	Files  []control.InputFile `json:"files,omitempty"`
	Origin *InputOrigin        `json:"origin,omitempty"`
}

// InputOrigin identifies the accepted call. Turn is nonzero only when the
// call itself started a turn, rather than joining an already running one.
type InputOrigin struct {
	Principal control.Principal `json:"principal,omitempty"`
	From      string            `json:"from,omitempty"`
	Key       string            `json:"key,omitempty"`
	Via       string            `json:"via,omitempty"`
	Turn      int64             `json:"turn,omitempty"`
	Text      string            `json:"text,omitempty"`
}

const KindInputFailed control.EventKind = "input_failed"

// pendingOrigin finds the accepted caller of the input the agent reported.
// First inputs and legacy pending rows have no origin here; an initial
// caller turn is already recorded before its input is handed over.
func pendingOrigin(ctx context.Context, tx *store.Tx, sid int64, input string) (*InputOrigin, error) {
	row, err := strconv.ParseInt(input, 10, 64)
	if err != nil {
		return nil, nil
	}
	var data string
	err = tx.QueryRowContext(ctx, `SELECT data FROM pending_inputs WHERE id = ? AND session_id = ? AND request = ''`, row, sid).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p PendingInput
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		return nil, err
	}
	return p.Origin, nil
}

// pendingFailure recognizes an input that never reached the agent. Only
// the turn started for that input may be ended by its failure. A held
// input instead has an event of its own, leaving any other turn alone.
func pendingFailure(ctx context.Context, tx *store.Tx, sid, turn int64, e agent.Event) (*Out, error) {
	if e.Kind != agent.TurnEnded || e.Input == "" ||
		!(e.Reason == agent.NotDelivered || e.Reason == agent.NotTaken || e.Status == agent.Interrupted) {
		return nil, nil
	}
	row, err := strconv.ParseInt(e.Input, 10, 64)
	if err != nil {
		return nil, nil
	}
	var data string
	err = tx.QueryRowContext(ctx, `SELECT data FROM pending_inputs WHERE id = ? AND session_id = ? AND request = ''`, row, sid).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		// A late failure of an input already reported is no end of the
		// turn that happens to run now.
		return &Out{}, nil
	}
	if err != nil {
		return nil, err
	}
	var p PendingInput
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		return nil, err
	}
	if p.Origin != nil && p.Origin.Turn != 0 && p.Origin.Turn == turn {
		return nil, nil
	}
	d := map[string]any{"input": e.Input, "reason": string(e.Reason)}
	if e.Status == agent.Interrupted {
		d["reason"] = ReasonInterrupted
	}
	if p.Text != "" {
		d["text"] = p.Text
	}
	if p.Link != nil {
		d["link"] = p.Link
	}
	if o := p.Origin; o != nil {
		d["text"] = o.Text
		for k, v := range map[string]string{"principal": string(o.Principal), "from": o.From, "key": o.Key, "via": o.Via} {
			if v != "" {
				d[k] = v
			}
		}
	}
	return &Out{Kind: KindInputFailed, Data: d}, nil
}

// PendingAnswer keeps the file receipts with a question answer until the
// host takes it. Its outer fields remain readable as control.Answer.
type PendingAnswer struct {
	control.Answer
	Files []control.InputFile `json:"files,omitempty"`
}
