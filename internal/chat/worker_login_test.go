package chat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/relay"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// Exercise the real front end with the fake surface's message references.
// The relay recorder makes rejection observable without spending a code.
func TestWorkerLoginDirectReplyBoundary(t *testing.T) {
	for _, kind := range []string{"unknown", "wrong-channel", "wrong-thread", "other-outage", "guest", "exact", "bare", "followup-exact", "followup-wrong-thread"} {
		t.Run(kind, func(t *testing.T) {
			r := &fakeRelay{}
			f := newFront(t, workspaces{}, withRelay(r))
			a, _ := f.twoThreads(t)
			f.running(t)
			f.lost(t, a, 1)
			f.waitFirst(t, "the outage alert", func(m surface.Message) bool { return m.Text != "" })
			f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 1, Link: link, Expires: time.Now().Add(time.Minute)})
			f.waitFirst(t, "the login link", func(m surface.Message) bool { return strings.Contains(m.Text, link) })
			posts, _ := f.dmPosts()
			ref := posts[0].Ref
			boundary := kind
			if strings.HasPrefix(kind, "followup-") {
				f.fe.LoginUpdate(relay.Update{Agent: "agent", Outage: 1, Refused: "Fixture refusal."})
				posts, _ = f.dmPosts()
				ref = posts[len(posts)-1].Ref
				boundary = strings.TrimPrefix(kind, "followup-")
			}
			in := f.driver.InjectDirect(owner, "marker-secret")
			switch boundary {
			case "unknown":
				ref.ID = "unrelated-message"
			case "wrong-channel":
				ref.Conv.Channel = "another-direct-conversation"
			case "wrong-thread":
				ref.Conv.Thread = "another-thread"
			case "other-outage":
				ref.ID = "another-outage-message"
				_, err := f.fe.Outbox.Store.DB.Exec(`INSERT INTO chat_logins(client,outage,agent,threads,dm_channel,dm_ref) VALUES('chat',2,'agent','[]',?,?)`, ref.Conv.Channel, ref.ID)
				if err != nil {
					t.Fatal(err)
				}
			case "guest":
				in.Author = fake.Person("someone")
			}
			if boundary != "bare" {
				in.ReplyTo = &ref
			}
			f.fe.Message(in)
			_, codes := r.got()
			if boundary == "exact" || boundary == "bare" {
				want := fmt.Sprintf("chat:%s agent#1 marker-secret", owner)
				if len(codes) != 1 || codes[0] != want {
					t.Fatalf("selected outage did not receive the owner's code: %v", codes)
				}
			} else if len(codes) != 0 {
				t.Fatal("unrelated reply reached the waiting login")
			}
		})
	}
}
