package chat

import (
	"testing"

	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// What waited for the final answer is posted once the turn is given a
// message that is not to others only, not with the next thing it says.
func TestSupervisorTurnPostsWhatWaitedWithAMessageToIt(t *testing.T) {
	f := newFront(t, workspaces{})
	id, _ := f.started(t)
	if _, err := f.fe.Outbox.Store.DB.Exec(`UPDATE threads SET confined = 1 WHERE id = ?`, int64(id)); err != nil {
		t.Fatal(err)
	}
	conv := f.conv(id)
	link := map[string]any{"client": f.fe.Name, "channel": conv.Channel, "thread": conv.Thread}
	stop := f.showing(t)
	turn(f, t, id, 1, "m1")
	f.event(t, id, "input", map[string]any{"turn": 1, "text": "<@bob> look", "link": link})
	f.event(t, id, "text", map[string]any{"text": "a", "final": false})
	stop()
	f.showing(t)
	f.event(t, id, "input", map[string]any{"turn": 1, "text": "look", "link": link})
	f.postedUntil(t, id, "a")
}

// What a supervisor's thread posts of a turn that answers messages to
// others only (#799), also when the daemon starts again between any two
// events of the turn.
func TestSupervisorTurnThatAnswersMessagesToOthers(t *testing.T) {
	type ev struct {
		kind string
		data map[string]any
	}
	here := map[string]any{"client": "", "channel": "", "thread": ""} // filled in below
	in := func(text string) ev { return ev{"input", map[string]any{"turn": 1, "text": text, "link": here}} }
	elsewhere := func(client, text string) ev {
		return ev{"input", map[string]any{"turn": 1, "text": text, "link": map[string]any{"client": client, "channel": "c", "thread": "t"}}}
	}
	say := func(text string) ev { return ev{"text", map[string]any{"text": text, "final": false}} }
	final := func(text string) ev { return ev{"text", map[string]any{"text": text, "final": true}} }
	end := func(outcome string) ev {
		return ev{"turn_ended", map[string]any{"turn": 1, "by": "caller", "outcome": outcome, "background": 0, "reason": "it broke"}}
	}
	start := ev{"turn_started", map[string]any{"turn": 1, "by": "caller", "key": "m1"}}
	ask := ev{"request", map[string]any{"request": "q1", "approval": false, "text": "Which?", "choices": []string{}}}
	allowed := ev{"request", map[string]any{"request": "q2", "text": "run it", "approval": true}}
	const other, own, both, plain = "<@bob> look", "<@" + fake.Self + "> look", "<@bob> <@" + fake.Self + "> look", "look"
	for _, c := range []struct {
		name   string
		events []ev
		want   string
	}{
		{"quiet", []ev{start, in(other), say("a"), say("b"), final(" (quiet)\n"), end("completed")}, ""},
		{"an answer", []ev{start, in(other), say("a"), say("b"), final("c"), end("completed")}, "a|b|c"},
		{"mentions both", []ev{start, in(both), say("a"), final("(quiet)"), end("completed")}, "a|(quiet)"},
		{"mentions hataori", []ev{start, in(own), say("a"), final("(quiet)"), end("completed")}, "a|(quiet)"},
		{"mentions nobody", []ev{start, in(plain), say("a"), final("(quiet)"), end("completed")}, "a|(quiet)"},
		{"two to others", []ev{start, in(other), say("a"), in(other), say("b"), final("(quiet)"), end("completed")}, ""},
		{"then one that is not", []ev{start, in(other), say("a"), in(plain), say("b"), final("(quiet)"), end("completed")}, "a|b|(quiet)"},
		{"first one that is not", []ev{start, in(plain), say("a"), in(other), say("b"), final("(quiet)"), end("completed")}, "a|b|(quiet)"},
		{"a question", []ev{start, in(other), say("a"), ask, end("blocked")}, "a|Which?"},
		{"an approval is no question", []ev{start, in(other), say("a"), allowed, final("(quiet)"), end("completed")}, ""},
		{"a failure after blank text", []ev{start, in(other), say("a"), final(" "), end("failure")}, "a|" + failureLine("it broke")},
		{"a failure", []ev{start, in(other), say("a"), end("failure")}, "a|" + failureLine("it broke")},
		{"the dashboard", []ev{start, {"input", map[string]any{"turn": 1, "text": other}}, say("a"), final("(quiet)"), end("completed")}, "a|(quiet)"},
		{"another client that reads mentions", []ev{start, elsewhere("other", other), say("a"), final("(quiet)"), end("completed")}, "someone: " + other},
		{"a client there is not", []ev{start, elsewhere("gone", other), say("a"), final("(quiet)"), end("completed")}, "someone: " + other + "|a|(quiet)"},
		{"the next turn", []ev{start, in(other), final("(quiet)"), end("completed"),
			{"turn_started", map[string]any{"turn": 2, "by": "caller", "key": "m2"}}, in(plain), final("(quiet)"), end("completed")}, "(quiet)"},
	} {
		for _, restarts := range []bool{false, true} {
			name := c.name
			if restarts {
				name += ", with restarts"
			}
			t.Run(name, func(t *testing.T) {
				f := newFront(t, workspaces{}, func(fe *FrontEnd) {
					fe.Drivers = func(client string) surface.Driver {
						if client == "other" {
							return fe.Driver
						}
						return nil
					}
				})
				id, _ := f.started(t)
				if _, err := f.fe.Outbox.Store.DB.Exec(`UPDATE threads SET confined = 1 WHERE id = ?`, int64(id)); err != nil {
					t.Fatal(err)
				}
				conv := f.conv(id)
				here["client"], here["channel"], here["thread"] = f.fe.Name, conv.Channel, conv.Thread
				stop := f.showing(t)
				f.event(t, id, "turn_started", map[string]any{"turn": 8, "by": "caller", "key": "m8"})
				f.event(t, id, "text", map[string]any{"text": "before", "final": true})
				ended(f, t, id, 8, "completed", "")
				base := len(f.postedUntil(t, id, "before"))
				for _, e := range c.events {
					pos := f.event(t, id, e.kind, e.data)
					if restarts {
						waitFor(t, "the event to be read", func() bool {
							var cursor uint64
							f.fe.Outbox.Store.DB.QueryRow(`SELECT coalesce(max(pos), 0) FROM chat_cursors`).Scan(&cursor)
							return cursor >= pos
						})
						stop()
						stop = f.showing(t)
					}
				}
				f.event(t, id, "turn_started", map[string]any{"turn": 9, "by": "caller", "key": "m9"})
				f.event(t, id, "text", map[string]any{"text": "after", "final": true})
				got := f.postedUntil(t, id, "after")
				if s := texts(got[base : len(got)-1]); s != c.want {
					t.Errorf("posted %q, want %q", s, c.want)
				}
			})
		}
	}
}
