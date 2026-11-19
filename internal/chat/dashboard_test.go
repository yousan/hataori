package chat

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

const testToken = "tok-0123456789abcdef0123456789abcdef"

// links makes sign-in links and counts them.
type links struct {
	mu sync.Mutex
	n  int
}

func (l *links) NewLink(context.Context, string, control.Way) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n++
	return "https://dash.example/signin#t=" + testToken, nil
}

func (l *links) made() int { l.mu.Lock(); defer l.mu.Unlock(); return l.n }

// echoing is a service whose refusal of a direct message repeats what was
// posted.
type echoing struct{ *fake.Driver }

func (e echoing) Post(ctx context.Context, conv surface.Conversation, key string, m surface.Message) (surface.MessageRef, error) {
	if strings.HasPrefix(conv.Channel, "direct") {
		return surface.MessageRef{}, fmt.Errorf("the service refused the content %q", m.Text)
	}
	return e.Driver.Post(ctx, conv, key, m)
}

// A direct message the service refuses: the error is logged without the
// link or its token.
func TestAFailedLinkMessageLogsNoToken(t *testing.T) {
	f := newFront(t, workspaces{}, func(fe *FrontEnd) { fe.Driver = echoing{fe.Driver.(*fake.Driver)} })
	f.core.SignIn = &links{}
	f.command(chanConv, fake.Person(owner), "d1", dashboardCommand)
	logs := strings.Join(f.log.lines(), "\n")
	if !strings.Contains(logs, "dashboard link: not sent") || strings.Contains(logs, testToken) {
		t.Fatalf("log:\n%s\nwant the failure, without the token", logs)
	}
}

// A message keeps what it was the first time: a text command edited into
// something else does nothing more, and input edited into a command is no
// command.
func TestAnEditedTextCommandStaysWhatItWas(t *testing.T) {
	f := newFront(t, workspaces{})
	l := &links{}
	f.core.SignIn = l
	id := f.runningThread(t) // a message in the thread is delivered into its turn
	th := f.conv(id)
	asked := f.deliver(th, fake.Person(owner), "!go dashboard")
	asked.Text = "please do it"
	f.fe.Message(asked)
	input := f.deliver(th, fake.Person(owner), "an input")
	input.Text = "!go dashboard"
	f.fe.Message(input)
	if n := l.made(); n != 1 {
		t.Fatalf("%d links, want the first command's only", n)
	}
	if got := strings.Join(f.runner.handed(), "; "); got != "deliver first input; deliver an input" {
		t.Fatalf("handed to the agent: %s", got)
	}
}
