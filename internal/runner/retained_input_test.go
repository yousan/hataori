package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// A failed turn can leave text in the agent's editor. Keep both initial
// and pending written inputs, including their exact paste, until a prompt
// acknowledges them; another daemon must reconstruct the same protection.
func TestUnconfirmedInputsSurviveTheirTurnAndReconnect(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	pasted := "exact paste"
	first, _ := json.Marshal(firstInput{Text: "initial", Context: "context ", Sent: true, Written: true, Current: true, WrittenText: &pasted})
	for _, q := range []string{
		`INSERT INTO threads (id,scope,key,started_by,state,created_at,updated_at) VALUES (1,'s','t','owner','idle',0,0)`,
		`INSERT INTO sessions (id,thread_id,agent,state,created_at,updated_at) VALUES (1,1,'test','idle',0,0)`,
		`INSERT INTO pending_inputs (id,session_id,data,written,written_text) VALUES (42,1,'{"text":"later"}',2,'exact paste')`,
	} {
		if _, err := st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB.Exec(`UPDATE sessions SET first_input = ? WHERE id = 1`, string(first)); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: st, Machine: &session.Machine{Store: st}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c := &handed{got: map[string]agent.Input{}}
	lv := &live{id: 1, codec: c}
	for _, id := range []string{"42", firstInputID} {
		err := st.Update(ctx, func(tx *store.Tx) error {
			var sent *firstInput
			return r.firstInputStep(ctx, tx, lv, batch{events: []agent.Event{{Kind: agent.TurnEnded, Status: agent.Failed, Reason: agent.NotDelivered, Input: id, RetainInput: true}}}, &sent)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	r.handOverWritten(lv)
	for _, id := range []string{firstInputID, "42"} {
		in, ok := c.got[id]
		if !ok || !in.Written || in.WrittenText == nil || *in.WrittenText != pasted {
			t.Errorf("reconnected %s: %+v", id, in)
		}
	}
	// A positive prompt report completes both inputs normally.
	if err := st.Update(ctx, func(tx *store.Tx) error {
		var sent *firstInput
		return r.firstInputStep(ctx, tx, lv, batch{events: []agent.Event{{Kind: agent.TurnStarted, Input: firstInputID}, {Kind: agent.TurnStarted, Input: "42"}}}, &sent)
	}); err != nil {
		t.Fatal(err)
	}
	var fi string
	var n int
	st.DB.QueryRow(`SELECT first_input FROM sessions WHERE id = 1`).Scan(&fi)
	st.DB.QueryRow(`SELECT count(*) FROM pending_inputs WHERE session_id = 1`).Scan(&n)
	if fi != "" || n != 0 {
		t.Fatalf("acknowledged inputs retained: first %q, pending %d", fi, n)
	}
}
