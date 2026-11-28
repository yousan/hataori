package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/supervisor"
)

// The captured status-line shape is replayed with explicitly varied values.
// These are counterexamples, not additional provider captures. No subprocess,
// host, terminal, user configuration or provider is involved.
func TestTwoSessionWindowAge(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, q := range []string{
		`INSERT INTO threads (id,scope,key,started_by,state,created_at,updated_at) VALUES (1,'s','a','p','idle',0,0),(2,'s','b','p','idle',0,0)`,
		`INSERT INTO sessions (id,thread_id,agent,state,created_at,updated_at) VALUES (1,1,'claude-code','idle',0,0),(2,2,'claude-code','idle',0,0)`,
	} {
		if _, err := st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile("testdata/2.1.327/limits/statusline.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		RateLimits map[string]map[string]any `json:"rate_limits"`
	}
	if err := json.Unmarshal(b, &capture); err != nil {
		t.Fatal(err)
	}
	const reset = int64(1795161600)
	window := func(used int, resets int64) map[string]any {
		w := map[string]any{}
		for k, v := range capture.RateLimits["seven_day"] {
			w[k] = v
		}
		w["used_percentage"], w["resets_at"] = used, resets
		return w
	}
	api := (&core.Core{Store: st}).As(control.Caller{May: control.OpRead})
	m := &session.Machine{Store: st}
	codecs := map[int64]*codec{1: newTestCodec(), 2: newTestCodec()}
	seq := map[int64]uint64{}
	at := time.Unix(1794980000, 0)
	push := func(sid int64, minute int, five, seven int, resets int64) {
		t.Helper()
		ws := map[string]any{}
		if five >= 0 {
			ws["five_hour"] = window(five, reset)
		}
		if seven >= 0 {
			ws["seven_day"] = window(seven, resets)
		}
		payload, _ := json.Marshal(map[string]any{"hook_event_name": statusLineEvent, "rate_limits": ws})
		line, _ := json.Marshal(call{Hook: fmt.Sprint(minute), At: at.Add(time.Duration(minute) * time.Minute).UnixMilli(), Payload: payload})
		a := codecs[sid].Decode(agent.Hook, line)
		if len(a.Events) != 1 || a.Events[0].Kind != agent.Limits {
			t.Fatalf("decode: %+v", a.Events)
		}
		e := a.Events[0]
		seq[sid]++
		e.Seq = seq[sid] // daemon-bound session sequence, including after reconnect
		if err := m.Apply(ctx, sid, session.Input{Kind: session.AgentEvent, Agent: e}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(five, seven, minute int, resets int64) {
		t.Helper()
		ls, err := api.Limits(ctx)
		if err != nil || len(ls) != 1 {
			t.Fatalf("Limits: %+v, %v", ls, err)
		}
		l := ls[0]
		ws := map[string]control.LimitWindow{}
		for _, w := range l.Windows {
			ws[w.Name] = w
		}
		if len(ws) != 2 || ws["five_hour"].Used != five || ws["seven_day"].Used != seven ||
			ws["seven_day"].Resets == nil || ws["seven_day"].Resets.Unix() != resets ||
			!l.At.Equal(at.Add(time.Duration(minute)*time.Minute)) {
			t.Errorf("got %+v at %s; want five=%d seven=%d reset=%d oldest minute=%d", l.Windows, l.At, five, seven, resets, minute)
		}
		if text := supervisor.UsageWindows(ls, []string{"claude-code"}, at.Add(time.Hour)); !strings.Contains(text, "Reported ") {
			t.Errorf("old observations look fresh: %s", text)
		}
	}
	push(1, 0, 1, 10, reset)
	push(2, 1, 2, 20, reset)
	push(1, 2, 3, 10, reset) // only A's five-hour observation changed
	check(3, 20, 1, reset)
	push(1, 3, -1, 10, reset) // removing five-hour does not refresh seven-day
	check(3, 20, 1, reset)
	push(1, 4, 3, -1, reset) // omission does not erase another session's window
	push(1, 5, -1, -1, reset)
	check(3, 20, 1, reset)
	m = &session.Machine{Store: st}
	codecs[1] = newTestCodec() // persistent provenance, not codec memory
	push(1, 6, 3, 10, reset)   // reappearance of the same cached values
	check(3, 20, 1, reset)
	push(2, 7, -1, 3, reset+604800) // later reset accepts a lower value
	check(3, 3, 2, reset+604800)
	push(1, 8, 4, 11, reset) // an older reset cannot roll the account back
	check(4, 3, 7, reset+604800)
	push(1, 9, 4, 2, reset+604800) // genuine decrease, not a maximum-until-reset
	check(4, 2, 8, reset+604800)
	push(2, 10, -1, 3, reset+604800) // B still has its cached value
	check(4, 2, 8, reset+604800)
	push(1, 11, 5, 2, reset+604800)
	check(5, 2, 9, reset+604800)
}
