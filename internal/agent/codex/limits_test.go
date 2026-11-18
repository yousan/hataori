package codex

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// The captures of the research note (#191), codex-cli 0.163.0.
var captures = filepath.Join("..", "..", "..", "docs", "notes", "agent-usage-limits", "captures")

func TestDecodeRateLimitsRead(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(captures, "codex-ratelimits-read.json"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1794980000, 0)
	r, err := DecodeRateLimits(b, at)
	if err != nil {
		t.Fatal(err)
	}
	want := []agent.LimitWindow{
		{Name: "primary", Minutes: 300, Used: 1, Resets: time.Unix(1794989084, 0)},
		{Name: "secondary", Minutes: 10080, Used: 26, Resets: time.Unix(1794987561, 0)},
	}
	if !r.At.Equal(at) || len(r.Windows) != 2 || r.Windows[0] != want[0] || r.Windows[1] != want[1] {
		t.Fatalf("got %+v, want %+v", r, want)
	}
}

// Every read of the poll run, across the weekly reset: the windows as the
// note's table gives them, and the reset moving 7 days on.
func TestDecodeRateLimitsAcrossAReset(t *testing.T) {
	f, err := os.Open(filepath.Join(captures, "codex-poll.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	reads := map[int]bool{}
	var got []*agent.LimitReport
	for sc.Scan() {
		var l struct {
			Dir string `json:"dir"`
			Msg struct {
				ID     *int            `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
			} `json:"msg"`
		}
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		switch {
		case l.Dir == "out" && l.Msg.Method == RateLimitsRead:
			reads[*l.Msg.ID] = true
		case l.Dir == "in" && l.Msg.ID != nil && reads[*l.Msg.ID]:
			r, err := DecodeRateLimits(l.Msg.Result, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, r)
		}
	}
	if len(got) != 14 {
		t.Fatalf("%d reads", len(got))
	}
	first, last := got[0].Windows, got[len(got)-1].Windows
	if first[0].Used != 2 || first[1].Used != 26 || !first[1].Resets.Equal(time.Unix(1794987561, 0)) {
		t.Fatalf("first read %+v", first)
	}
	if last[0].Used != 6 || last[1].Used != 0 || !last[1].Resets.Equal(time.Unix(1795592364, 0)) {
		t.Fatalf("last read %+v", last)
	}
}

// A window that is null, or has no usedPercent, is left out; a result without rateLimits is an
// error.
func TestDecodeRateLimitsNull(t *testing.T) {
	if r, err := DecodeRateLimits([]byte(`{"rateLimits":{"primary":null,"secondary":null}}`), time.Time{}); err != nil || len(r.Windows) != 0 {
		t.Fatalf("%+v, %v", r, err)
	}
	r, err := DecodeRateLimits([]byte(`{"rateLimits":{"primary":{"usedPercent":null},"secondary":{"usedPercent":3,"windowDurationMins":null,"resetsAt":null}}}`), time.Time{})
	if err != nil || len(r.Windows) != 1 || r.Windows[0] != (agent.LimitWindow{Name: "secondary", Used: 3}) {
		t.Fatalf("%+v, %v", r, err)
	}
	if _, err := DecodeRateLimits([]byte(`{}`), time.Time{}); err == nil {
		t.Fatal("no rateLimits: no error")
	}
}
