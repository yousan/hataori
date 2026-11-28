package claudecode

import (
	"encoding/json"
	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/host"
	"os"
	"testing"
	"time"
)

func TestFollowSourcesUsesCapturedHook(t *testing.T) {
	b, err := os.ReadFile("testdata/2.1.322/interrupt/hooks/01-SessionStart.json")
	if err != nil {
		t.Fatal(err)
	}
	var p payload
	if err := json.Unmarshal(b, &p); err != nil || p.TranscriptPath == "" {
		t.Fatalf("capture lacks transcript: %v", err)
	}
	a := &Adapter{}
	line := host.CallLine("captured-hook", false, time.Unix(0, 0), b)
	paths := a.FollowSources(agent.Hook, line)
	if len(paths) != 1 || paths[0] != p.TranscriptPath {
		t.Fatalf("reported paths: %q", paths)
	}
	if got := a.FollowSources(agent.Output, line); len(got) != 0 {
		t.Fatalf("non-hook granted sources: %q", got)
	}
}
