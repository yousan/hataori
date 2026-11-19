package codex

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/agent"
)

type wireLine struct {
	Dir    string          `json:"dir"`
	Server int             `json:"server"`
	Msg    json.RawMessage `json:"msg"`
}

func replay(t *testing.T, scenario string) []wireLine {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata/0.163.0", scenario, "wire.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []wireLine
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 4<<20)
	for s.Scan() {
		var l wireLine
		if err := json.Unmarshal(s.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, l)
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}
func feed(t *testing.T, c agent.Codec, lines []wireLine, server int) []agent.Event {
	t.Helper()
	var events []agent.Event
	for _, l := range lines {
		if l.Dir != "recv" || (server != 0 && l.Server != server) {
			continue
		}
		var v any
		if err := json.Unmarshal(l.Msg, &v); err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, c.Decode(agent.Output, b).Events...)
	}
	return events
}
