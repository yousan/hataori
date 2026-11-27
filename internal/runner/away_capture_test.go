package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/claudecode"
	"github.com/yousan/hataori/internal/agent/codex"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/machineio"
	"github.com/yousan/hataori/internal/store"
)

type awayCapturedAdapter struct{ agent.Adapter }

func (awayCapturedAdapter) NewCodec(s agent.Spec) agent.Codec {
	return codex.NewCodec(s, codex.Options{})
}

type awayCapturedIO struct {
	machineio.Session
	data []byte
}

func (awayCapturedIO) Source(context.Context, string) (machineio.Source, error) {
	return machineio.Source{Name: "", Handle: "capture", Generation: "original"}, nil
}
func (s awayCapturedIO) Read(_ context.Context, src machineio.Source, at int64, n int) (machineio.Range, error) {
	end := min(int64(len(s.data)), at+int64(n))
	return machineio.Range{Source: src, Offset: at, Data: append([]byte{}, s.data[at:end]...)}, nil
}

// This crosses the runner's warm/read/commit boundary with the real native
// codec and its captured app-server output. The fake agent's explicit input
// identifiers cannot prove the native prompt-to-accepted-input correlation.
func TestAwayNativeWrittenReportCorrelatesBeforeCatchup(t *testing.T) {
	capture, err := os.Open("../agent/codex/testdata/0.163.0/start/wire.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	var data []byte
	var prefix int
	var text string
	scanner := bufio.NewScanner(capture)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for scanner.Scan() {
		var line struct {
			Dir string          `json:"dir"`
			Msg json.RawMessage `json:"msg"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		var msg struct {
			Method string `json:"method"`
			Params struct {
				Input []struct {
					Text string `json:"text"`
				} `json:"input"`
			} `json:"params"`
			Result struct {
				Thread *json.RawMessage `json:"thread"`
			} `json:"result"`
		}
		if err := json.Unmarshal(line.Msg, &msg); err != nil {
			t.Fatal(err)
		}
		if line.Dir == "send" && msg.Method == "turn/start" && len(msg.Params.Input) > 0 {
			text = msg.Params.Input[0].Text
		}
		if line.Dir != "recv" {
			continue
		}
		data = append(data, bytes.TrimSpace(line.Msg)...)
		data = append(data, '\n')
		if prefix == 0 && msg.Result.Thread != nil {
			prefix = len(data)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if prefix == 0 || text == "" {
		t.Fatal("capture lacks startup/input boundary")
	}
	for _, kind := range []string{"first", "pending"} {
		t.Run(kind, func(t *testing.T) {
			f := placementRecordFixture(t)
			ctx := context.Background()
			if err := f.st.SyncMachines(ctx, []store.MachineBinding{{Key: "build", SSH: "fixture", Root: "/fixture"}}); err != nil {
				t.Fatal(err)
			}
			for _, q := range []string{
				`INSERT INTO threads(id,scope,key,started_by,state,created_at,updated_at,machine) VALUES(1,'s','native','owner','running',1,1,'build')`,
				`INSERT INTO sessions(id,thread_id,agent,state,spec,created_at,updated_at,launch) VALUES(1,1,'fake','running','{}',1,1,0)`,
				`INSERT INTO turns(id,session_id,started_by,principal,started_at) VALUES(1,1,'person','owner',1)`,
			} {
				if _, err := f.st.DB.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "first" {
				raw, _ := json.Marshal(core.FirstInput{Principal: "owner", Text: text, Sent: true, Written: true, Current: true, TurnID: 1, WrittenAt: time.Now().UnixMilli()})
				if _, err := f.st.DB.Exec(`UPDATE sessions SET first_input=? WHERE id=1`, string(raw)); err != nil {
					t.Fatal(err)
				}
			} else {
				raw, _ := json.Marshal(map[string]any{"text": text, "origin": map[string]any{"turn": 1, "principal": "owner"}})
				if _, err := f.st.DB.Exec(`INSERT INTO pending_inputs(id,session_id,request,data,context,written,written_at) VALUES(1,1,'',?,'',2,?)`, string(raw), time.Now().UnixMilli()); err != nil {
					t.Fatal(err)
				}
			}
			f.run.AwayEnabled = true
			f.run.Remotes = map[string]RemoteMachine{"build": {Connection: func() Connection { return Connection{Connected: true, Token: "native-catchup"} }}}
			ad := awayCapturedAdapter{}
			lv := &live{id: 1, dir: StateDir(f.root, 1), remote: true, adapter: ad, codec: ad.NewCodec(agent.Spec{}), spec: agent.Spec{}, io: awayCapturedIO{data: data}, files: map[string]int64{}, sources: map[string]machineio.Source{}, warmPending: true, offset: int64(prefix)}
			if !f.run.step(lv) {
				t.Fatal("captured live worker unexpectedly ended")
			}
			var first string
			var pending int
			f.st.DB.QueryRow(`SELECT first_input FROM sessions WHERE id=1`).Scan(&first)
			f.st.DB.QueryRow(`SELECT count(*) FROM pending_inputs`).Scan(&pending)
			if first != "" || pending != 0 {
				t.Fatalf("acknowledged written input retained: %q pending=%d", first, pending)
			}
			for _, e := range lv.codec.Tick(f.run.codecNow(lv).Add(time.Hour)).Events {
				if e.Reason == agent.NotDelivered {
					t.Fatalf("delivered input falsely timed out: %+v", e)
				}
			}
			if got := counts(t, f.st); got["input"] != 0 || got["turn_ended"] != 1 {
				t.Fatalf("native report became another caller: %v", got)
			}
		})
	}
}

func TestAwayNewNativeHookUsesWallClockAfterPause(t *testing.T) {
	f := placementRecordFixture(t)
	ctx := context.Background()
	if err := f.st.SyncMachines(ctx, []store.MachineBinding{{Key: "build", SSH: "fixture", Root: "/fixture"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.DB.Exec(`INSERT INTO threads(id,scope,key,started_by,state,created_at,updated_at,machine) VALUES(1,'s','clock','owner','running',1,1,'build'); INSERT INTO sessions(id,thread_id,agent,state,created_at,updated_at) VALUES(1,1,'fake','running',1,1)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := f.st.DB.Exec(`INSERT INTO session_pauses(session_id,began_at,ended_at) VALUES(1,?,?)`, now.Add(-2*time.Hour).UnixMilli(), now.Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	ad, err := claudecode.New([]byte(`{"hook_timeout":"1m"}`))
	if err != nil {
		t.Fatal(err)
	}
	codec := ad.NewCodec(agent.Spec{})
	payload, err := os.ReadFile("../agent/claudecode/testdata/2.1.322/question/hooks/03-PreToolUse.json")
	if err != nil {
		t.Fatal(err)
	}
	codec.Decode(agent.Hook, host.CallLine("after-recovery", true, now.Add(-2*time.Minute), payload))
	f.run.AwayEnabled = true
	lv := &live{id: 1, remote: true, codec: codec}
	a := f.run.tickCodec(lv)
	if len(a.Events) != 1 || a.Events[0].Kind != agent.RequestWithdrawn {
		t.Fatalf("paused delivery clock delayed a new expired worker hook: %+v", a.Events)
	}
}
