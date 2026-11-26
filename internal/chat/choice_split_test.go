package chat

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// splitChoiceDriver models a service that splits posts, returns the last
// part's reference and refuses edits over its UTF-16 character limit.
// Only the last part carries the buttons.
type splitChoiceDriver struct{ *fake.Driver }

func textUnits(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}
func (d *splitChoiceDriver) Post(ctx context.Context, conv surface.Conversation, key string, m surface.Message) (surface.MessageRef, error) {
	rest := m.Text
	var ref surface.MessageRef
	for part := 0; ; part++ {
		n, end := 0, len(rest)
		for i, r := range rest {
			if n+utf16.RuneLen(r) > d.Limits().MaxText {
				end = i
				break
			}
			n += utf16.RuneLen(r)
		}
		chunk := m
		chunk.Text = rest[:end]
		rest = rest[end:]
		if rest != "" {
			chunk.Choices = nil
		}
		var err error
		ref, err = d.Driver.Post(ctx, conv, fmt.Sprintf("%s.part%d", key, part), chunk)
		if err != nil || rest == "" {
			return ref, err
		}
	}
}
func (d *splitChoiceDriver) Edit(ctx context.Context, ref surface.MessageRef, m surface.Message) error {
	if textUnits(m.Text) > d.Limits().MaxText {
		return fmt.Errorf("edit too long")
	}
	return d.Driver.Edit(ctx, ref, m)
}

// A choice resolves both the original failure and its follow-up reply,
// including split Unicode posts and a post close to the service's limit.
// No earlier text is copied into the final fragment and buttons disappear.
func TestBlockedChoiceEditsOnlyTheResolution(t *testing.T) {
	for _, c := range []struct{ name, detail string }{
		{"short", "the dialog"},
		{"near the limit", strings.Repeat("a", 1300)},
		{"multiple parts", strings.Repeat("a", maxDetail+100)},
		{"UTF16 split", strings.Repeat("🧵", 800)},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFront(t, workspaces{}, func(fe *FrontEnd) {
				d := &splitChoiceDriver{Driver: fe.Driver.(*fake.Driver)}
				fe.Driver = d
				fe.Outbox.Driver = d
			})
			id, _ := f.started(t)
			f.showing(t)
			err := f.core.Machine.Apply(context.Background(), f.runner.all()[0].Session, session.Input{Kind: session.AgentEvent, Agent: agent.Event{Seq: 1, Kind: agent.SessionEnded, Reason: agent.BlockedAtStart, Text: c.detail, Trust: []string{".agent/settings.json"}}})
			if err != nil {
				t.Fatal(err)
			}
			th := f.conv(id)
			withButtons := func() []fake.Call {
				var out []fake.Call
				for _, call := range f.driver.Calls(th) {
					if call.Op == fake.OpPost && len(call.Message.Choices) == 3 {
						out = append(out, call)
					}
				}
				return out
			}
			waitFor(t, "the failure's buttons", func() bool { return len(withButtons()) == 1 })
			original := withButtons()[0]
			f.deliver(th, fake.Webhook(hook), "are you there?")
			waitFor(t, "the reply's buttons", func() bool { return len(withButtons()) == 2 })
			targets := withButtons()
			// The choice token names the exact failed-start event.
			_, at, _ := strings.Cut(original.Message.Choices[0].Token, "#")
			at, _, _ = strings.Cut(at, ":")
			pos, err := strconv.ParseUint(at, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			before := f.driver.Calls(th)
			f.event(t, id, "start_chosen", map[string]any{"session": 1, "event": pos, "choice": control.StartClosed, "principal": "ada", "via": "dashboard"})
			edits := func() []fake.Call {
				var out []fake.Call
				for _, call := range f.driver.Calls(th) {
					if call.Op == fake.OpEdit {
						out = append(out, call)
					}
				}
				return out
			}
			waitFor(t, "both messages to resolve", func() bool { return len(edits()) == 2 })
			for _, edit := range edits() {
				if len(edit.Message.Choices) != 0 || edit.Message.Text != "Not started: closed by ada." || textUnits(edit.Message.Text) > 2000 {
					t.Fatalf("bad edit %+v", edit)
				}
				found := false
				for _, target := range targets {
					found = found || target.Ref == edit.Ref
				}
				if !found {
					t.Fatalf("edited an earlier part: %+v", edit.Ref)
				}
			}
			posts := 0
			for _, call := range before {
				if call.Op == fake.OpPost {
					posts++
				}
			}
			if (c.name == "multiple parts" || c.name == "UTF16 split") && posts <= 3 {
				t.Fatalf("fixture never split: %d posts", posts)
			}
			// No new posts or copied failure text are introduced by resolving.
			afterPosts := 0
			for _, call := range f.driver.Calls(th) {
				if call.Op == fake.OpPost {
					afterPosts++
				}
			}
			if afterPosts != posts {
				t.Fatalf("choice posted extra text: before %d after %d", posts, afterPosts)
			}
		})
	}
}
