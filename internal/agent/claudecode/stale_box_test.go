package claudecode

import (
	"bytes"
	"slices"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
)

// Replay the captured box and hook from first_enter_lost: the CLI can hold
// a paste without reporting it. No key may append, submit or clear a
// person's text after a partial paste, or after a daemon reconnect.
func TestAnUnconfirmedPasteBlocksTheNextMessage(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial paste", true: "reconnected"}[resumed], func(t *testing.T) {
			c := firstStart(t)
			after := readScreen(t, firstEnter+"screen-after.raw")
			frame := []byte("\x1b[?25l\x1b[H")
			first := bytes.Index(after, frame)
			second := first + len(frame) + bytes.Index(after[first+len(frame):], frame)
			if first < 0 || second <= first {
				t.Fatal("capture has no empty first frame")
			}
			c.Decode(agent.Screen, after[:second])
			oldText := prompt(t, "plain/hooks/02-UserPromptSubmit.json")
			if oldText == firstText {
				t.Fatal("capture must show another person's text")
			}
			in := agent.Input{ID: "old", Text: oldText, Written: resumed}
			a, err := c.Input(in)
			if err != nil {
				t.Fatal(err)
			}
			if resumed {
				if len(a.Keys) != 0 {
					t.Fatalf("reconnected input written: %q", a.Keys)
				}
			} else {
				if len(a.Keys) != 1 || a.Keys[0].Paste != oldText || a.KeysFailed == nil {
					t.Fatalf("paste: %+v", a)
				}
				a.KeysFailed(false) // the holder cannot say how much reached the box
			}
			c.Decode(agent.Screen, after[second:])
			// The capture now belongs to a person's editor. Whether it contains
			// all or part of our paste, it is never cleared or submitted by us.
			now := time.Now()
			var ended []agent.Event
			for i := 0; i <= int(DefaultInputTimeout/time.Second)+1; i++ {
				a := c.Tick(now.Add(time.Duration(i) * time.Second))
				if len(a.Keys) > 0 {
					t.Fatalf("unconfirmed box got keys: %q", a.Keys)
				}
				ended = append(ended, a.Events...)
			}
			if len(ended) != 1 || ended[0].Kind != agent.TurnEnded || ended[0].Reason != agent.NotDelivered || ended[0].Input != "" {
				t.Errorf("old turn: %+v; want failed turn with input retained for another reconnect", ended)
			}
			for _, codec := range []*codec{c, started(t)} {
				if codec != c {
					if _, err := codec.Input(agent.Input{ID: "old", Text: oldText, Written: true}); err != nil {
						t.Fatal(err)
					}
				}
				a, err := codec.Input(agent.Input{ID: "new", Text: "a separate message"})
				if err != nil || len(a.Keys) != 0 || len(a.Events) != 1 || a.Events[0].Input != "new" || a.Events[0].Reason != agent.NotDelivered {
					t.Fatalf("next message: keys %q events %+v, %v", a.Keys, a.Events, err)
				}
				for i := 0; i < 100; i++ {
					a := codec.Tick(now.Add(time.Duration(i) * time.Second))
					if len(a.Keys) > 0 {
						t.Fatalf("later keys: %q", a.Keys)
					}
				}
				if a := codec.Interrupt(); len(a.Keys) > 0 || !a.NotInterrupted {
					t.Fatalf("idle Stop modified the editor: %+v", a)
				}
				if !slices.Contains(codec.leftover, "old") {
					t.Fatalf("unconfirmed paste forgotten: %q", codec.leftover)
				}
			}
		})
	}
}

// A turn triggered by a background notification does not consume the
// editor's text. Reconnecting while it runs never submits that text when
// the notification's turn ends.
func TestAReconnectedPasteWaitsThroughOtherTurns(t *testing.T) {
	c := newTestCodec()
	for _, f := range []string{"01-SessionStart", "02-UserPromptSubmit", "05-Stop", "07-UserPromptSubmit"} {
		decodeFile(t, c, f, held+f+".json")
	}
	c.Input(agent.Input{ID: "old", Text: heldText, Written: true})
	decodeFile(t, c, "11", held+"11-Stop.json")
	now := time.Now()
	for i := 0; i < 100; i++ {
		if a := c.Tick(now.Add(time.Duration(i) * time.Second)); len(a.Keys) > 0 {
			t.Fatalf("keys after unrelated turn: %q", a.Keys)
		}
	}
	a, _ := c.Input(agent.Input{ID: "new", Text: "alone"})
	if len(a.Keys) > 0 || len(a.Events) != 1 || a.Events[0].Reason != agent.NotDelivered {
		t.Fatalf("next input: %+v", a)
	}
	// The CLI reporting the original prompt is positive evidence that it
	// consumed the box. Its matching input then releases the fence.
	if a := decodeFile(t, c, "12", held+"12-UserPromptSubmit.json"); summary(a) != "turn(caller)" || a.Events[0].Input != "old" {
		t.Fatalf("original prompt: %+v", a.Events)
	}
	decodeFile(t, c, "13", held+"13-Stop.json")
	if a, _ := c.Input(agent.Input{ID: "after", Text: "alone"}); len(a.Keys) != 1 || a.Keys[0].Paste != "alone" {
		t.Fatalf("after confirmed prompt: %+v", a)
	}
}
