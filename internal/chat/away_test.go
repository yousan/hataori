package chat

import (
	"github.com/yousan/hataori/internal/control"
	"strings"
	"testing"
	"time"
)

func TestAwayStatusReportsHistoricalRunningState(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	info := control.ThreadInfo{ID: 1, State: control.Running, Machine: "build", Availability: &control.MachineAvailability{State: "away", At: at, LastKnownState: control.Running, LastKnownAt: &at, Reason: "transport"}}
	for _, text := range []string{threadStatus(info), channelStatus([]control.ThreadInfo{info})} {
		if !strings.Contains(text, "away since") || !strings.Contains(text, "last known running") || strings.Contains(text, "#1: running") {
			t.Fatalf("stale running claim: %s", text)
		}
	}
}
