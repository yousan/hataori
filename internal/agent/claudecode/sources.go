package claudecode

import (
	"encoding/json"
	"github.com/yousan/hataori/internal/agent"
)

func (a *Adapter) FollowSources(src agent.Source, line []byte) []string {
	if src != agent.Hook {
		return nil
	}
	var k call
	var p payload
	if json.Unmarshal(line, &k) != nil || k.Hook == "" || json.Unmarshal(k.Payload, &p) != nil || p.Event == mcpListedEvent || p.TranscriptPath == "" {
		return nil
	}
	return []string{p.TranscriptPath}
}
