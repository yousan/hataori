package chat

import (
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/surface/fake"
)

// The edit of an answered question fits one message, however long the
// question and the answer, and what follows it is still posted.
func TestAnsweredQuestionEditFitsOneMessage(t *testing.T) {
	f := newFront(t, workspaces{})
	l := fake.DefaultLimits
	l.MaxText = 80
	f.driver.SetLimits(l)
	id, trigger := f.started(t)
	f.showing(t)
	turn(f, t, id, 1, trigger.Ref.ID)
	long := strings.Repeat("Which colour should it be? ", 3)
	f.event(t, id, "request", map[string]any{"request": "1:q1", "approval": false, "text": long, "choices": []string{"red", "blue"}})
	f.event(t, id, "answer", map[string]any{"turn": 2, "text": strings.Repeat("blue, because ", 30), "request": "1:q1"})
	f.event(t, id, "text", map[string]any{"text": "after", "final": false})
	f.postedUntil(t, id, "after")
	th := f.threads()[0].Opened
	edits := 0
	for _, c := range f.driver.Calls(th) {
		if c.Op == fake.OpEdit {
			edits++
			if n := len([]rune(c.Message.Text)); n > l.MaxText || len(c.Message.Choices) != 0 ||
				!strings.Contains(c.Message.Text, "Answered: blue") {
				t.Errorf("edit %q (%d characters, choices %v), want at most %d without choices", c.Message.Text, n, c.Message.Choices, l.MaxText)
			}
		}
	}
	if edits != 1 {
		t.Fatalf("%d edits, want 1", edits)
	}
}
