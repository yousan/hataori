package chat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/surface/fake"
)

// Git remotes and agent refusals may supply Markdown, mentions or fences.
// Both synchronous workspace failures and later start events quote the
// whole reason, with block delimiters on their own lines.
func TestFailedStartReasonsAreVerbatim(t *testing.T) {
	const reason = "remote: [a link](https://example.com)\n# a heading\n```\n@everyone <@1>"
	for _, later := range []bool{false, true} {
		t.Run(map[bool]string{false: "workspace", true: "agent refusal"}[later], func(t *testing.T) {
			ws := workspaces{}
			if !later {
				ws.err = errors.New(reason)
			}
			f := newFront(t, ws)
			if later {
				id, _ := f.started(t)
				f.showing(t)
				err := f.core.Machine.Apply(context.Background(), f.runner.all()[0].Session, session.Input{Kind: session.AgentEvent, Agent: agent.Event{Seq: 1, Kind: agent.SessionEnded, Reason: agent.Reason(reason)}})
				if err != nil {
					t.Fatal(err)
				}
				got := texts(f.postedUntil(t, id, couldNotStart+"\n"+f.driver.Verbatim(reason)))
				if got != "the prompt|"+couldNotStart+"\n"+f.driver.Verbatim(reason) {
					t.Fatalf("posts %q", got)
				}
			} else {
				f.deliver(chanConv, fake.Webhook(hook), "!go repo:https://example.com/r.git first")
				th := f.threads()[0].Opened
				waitFor(t, "the failure", func() bool { return len(postsIn(f.driver, th)) == 2 })
				got := postsIn(f.driver, th)[1]
				if got != couldNotStart+"\n"+f.driver.Verbatim(reason) || strings.Count(got, "[verbatim]") != 1 {
					t.Fatalf("workspace reason %q", got)
				}
			}
		})
	}
}
