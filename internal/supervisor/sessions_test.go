package supervisor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
)

// A plugin's reason, which may quote the input, is left out of what a log
// line shows of a stopped input; a failure, in hataori's words, is shown.
func TestErrTextLeavesOutAPluginsReason(t *testing.T) {
	stop := fmt.Errorf("core: Send: %w", &control.Stopped{Point: "before_agent_run", Plugin: "scan", Reason: "MARKER-reason"})
	if s := ErrText(stop); strings.Contains(s, "MARKER") || !strings.Contains(s, "scan") || !stoppedByPlugin(stop) || !final(stop) {
		t.Fatalf("a stop: %q", s)
	}
	failed := fmt.Errorf("core: Send: %w", &control.Stopped{Point: "before_agent_run", Plugin: "scan", Reason: "it is not connected", Failed: true})
	if s := ErrText(failed); !strings.Contains(s, "it is not connected") || stoppedByPlugin(failed) || final(failed) {
		t.Fatalf("a failure: %q", s)
	}
}
