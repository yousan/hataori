package fake

import (
	"encoding/json"
	"github.com/yousan/hataori/internal/agent"
)

func (a *Adapter) FollowSources(src agent.Source, line []byte) []string {
	if !a.c.Terminal || src != agent.Hook {
		return nil
	}
	var k call
	var p report
	if json.Unmarshal(line, &k) != nil || k.Hook == "" || json.Unmarshal(k.Payload, &p) != nil || p.Transcript == "" {
		return nil
	}
	return []string{p.Transcript}
}
