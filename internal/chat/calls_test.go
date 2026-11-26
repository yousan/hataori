package chat

import (
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// A message of somebody who may not act reaches no agent and starts
// nothing, called or not (#869): live and at a catch-up, in a thread and in
// its channel, as a trigger too. A person's after them does.
func TestACalledMessageOfSomebodyWhoMayNotActReachesNoAgent(t *testing.T) {
	const at = "<@" + fake.Self + ">"
	for _, called := range []bool{false, true} {
		for name, who := range map[string]surface.Author{
			"a guest":                    fake.Person("guest-9"),
			"a webhook that is a guest":  fake.Webhook("stranger"),
			"a person of no scope":       fake.Person(outsider),
			"a bot with a person's id":   fake.Bot(owner),
			"a bot with a webhook's id":  fake.Bot(hook),
			"a bot that nobody knows of": fake.Bot("bot-9"),
		} {
			if called {
				name += ", called"
			}
			t.Run(name, func(t *testing.T) {
				f := newFront(t, workspaces{})
				id := f.runningThread(t)
				say := func(what string) {
					for _, conv := range []surface.Conversation{f.conv(id), chanConv} {
						for _, text := range []string{at + " " + what, "!go repo:https://example.com/r.git " + at + " " + what} {
							f.driver.InjectWith(conv, who, text, func(in *surface.Inbound) {
								if called {
									i := strings.Index(text, at)
									in.Call, in.OwnMentions = true, []surface.Span{{Start: i, End: i + len(at)}}
								}
							})
						}
					}
				}
				say("caught up") // nobody is connected
				f.connect(t)
				say("live")
				f.deliver(f.conv(id), fake.Person(owner), "the last")
				const want = "deliver first input; deliver the last"
				waitFor(t, "the message after them", func() bool { return strings.HasSuffix(strings.Join(f.runner.handed(), "; "), "the last") })
				if got := strings.Join(f.runner.handed(), "; "); got != want {
					t.Errorf("handed to the agent: %s", got)
				}
				if n := len(f.runner.all()); n != 1 {
					t.Errorf("%d sessions started, want the thread's own", n)
				}
			})
		}
	}
}
