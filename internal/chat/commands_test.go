package chat

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// command hands the use of a command to the front end, as the driver's Run
// would, with the reply a service that waits for an answer gives.
func (f *front) command(conv surface.Conversation, author surface.Author, id, name string, args ...surface.Arg) surface.Command {
	c := surface.Command{Conv: conv, Author: author, ID: id, Name: name, Args: args,
		Reply: &surface.MessageRef{Conv: conv, ID: id}}
	f.fe.Command(c)
	return c
}

// settled writes a marker row in each of convs and waits until it is
// posted, so that everything written before it is posted too.
func (f *front) settled(t *testing.T, convs ...surface.Conversation) {
	t.Helper()
	for i, conv := range convs {
		ev := uint64(1<<40 + i)
		if err := f.fe.Outbox.Write(context.Background(), row(ev, 0, conv, "marker")); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the marker", func() bool { _, ok, _ := f.fe.Outbox.Sent(context.Background(), ev, 0); return ok })
	}
}

func newArgs(prompt string) []surface.Arg {
	return []surface.Arg{{Name: "repository", Value: "https://example.com/r.git"}, {Name: "prompt", Value: prompt}}
}

// A guest, and a person not allowed in the scope: a start, stop and status
// command, a message and a button press each show nothing, reach no agent,
// and write one log line.
func TestCommandsMessagesAndButtonsOfOthersDoNothingAndLogOneLine(t *testing.T) {
	f := newFront(t, workspaces{})
	f.deliver(chanConv, fake.Webhook(hook), "!go repo:https://example.com/r.git first")
	th := f.threads()[0].Opened
	waitFor(t, "the prompt", func() bool { return len(postsIn(f.driver, th)) == 1 })
	n := 0
	for _, who := range []string{"guest-9", outsider} {
		author := fake.Person(who)
		for _, act := range []struct {
			name string
			do   func(id string)
		}{
			{"start command", func(id string) { f.command(chanConv, author, id, newCommand, newArgs("reply hi")...) }},
			{"stop command", func(id string) { f.command(th, author, id, stopCommand) }},
			{"status command in the thread", func(id string) { f.command(th, author, id, statusCommand) }},
			{"status command in the channel", func(id string) { f.command(chanConv, author, id, statusCommand) }},
			{"message", func(string) { f.deliver(th, author, "hello") }},
			{"button press", func(string) {
				f.fe.Choice(surface.Selection{Ref: surface.MessageRef{Conv: th, ID: "m-1"}, Author: author, Token: "login:agent"})
			}},
		} {
			n++
			before := len(f.log.lines())
			act.do("i" + strconv.Itoa(n))
			lines := f.log.lines()[before:]
			if len(lines) != 1 || !strings.Contains(lines[0], "id="+who) {
				t.Errorf("%s's %s: log %q, want one line with the id", who, act.name, lines)
			}
		}
	}
	f.settled(t, th, chanConv)
	if got := strings.Join(postsIn(f.driver, th), "|"); got != "first|marker" {
		t.Errorf("the thread shows %q", got)
	}
	if got := strings.Join(postsIn(f.driver, chanConv), "|"); got != "marker" {
		t.Errorf("the channel shows %q", got)
	}
	if o, l, h := len(f.threads()), len(f.runner.all()), f.runner.handed(); o != 1 || l != 1 || len(h) != 0 {
		t.Errorf("%d threads opened, %d started, handed %q; want only the first", o, l, h)
	}
}

// A thread of another scope shown in the channel is not in the channel's
// status.
func TestStatusInAChannelShowsNoThreadOfAnotherScope(t *testing.T) {
	f := newFront(t, workspaces{}, func(fe *FrontEnd) {
		fe.Scopes["c2"] = Scope{Enabled: true}
		fe.ShowIn = map[string][]string{channel: {"c2"}}
	})
	f.core.Scopes["c2"] = core.Scope{Enabled: true, Owner: "chat", ShowIn: []string{"chat:" + channel}}
	c2 := surface.Conversation{Channel: "c2"}
	f.deliver(chanConv, fake.Person(owner), "!go repo:https://example.com/r.git own work")
	f.deliver(c2, fake.Person(owner), "!go repo:https://example.com/r.git other work")
	var own, other int64
	db := f.fe.Outbox.Store.DB
	db.QueryRow(`SELECT id FROM threads WHERE scope = ?`, channel).Scan(&own)
	db.QueryRow(`SELECT id FROM threads WHERE scope = 'c2'`).Scan(&other)
	f.command(chanConv, fake.Person(owner), "a1", attachCommand, surface.Arg{Name: "number", Value: strconv.FormatInt(other, 10)})
	if len(f.threads()) != 2 {
		t.Fatalf("opened %+v in the channel, want its own thread and the attached one", f.threads())
	}
	f.command(chanConv, fake.Person(owner), "s1", statusCommand)
	f.settled(t, chanConv)
	var status string
	for _, p := range postsIn(f.driver, chanConv) {
		if strings.HasPrefix(p, "Threads here:") {
			status = p
		}
	}
	if !strings.Contains(status, "#"+strconv.FormatInt(own, 10)+" creating: own work") ||
		strings.Contains(status, "#"+strconv.FormatInt(other, 10)) || strings.Contains(status, "other work") {
		t.Fatalf("status %q, want thread #%d and not #%d", status, own, other)
	}
}

// A start command delivered again with the same id starts one thread.
func TestRepeatedStartCommandStartsOneThread(t *testing.T) {
	f := newFront(t, workspaces{})
	c := f.command(chanConv, fake.Webhook(hook), "i1", newCommand, newArgs("do it")...)
	f.fe.Command(c)
	th := f.threads()
	if len(th) != 1 || th[0].From != "" || th[0].Title != "do it" {
		t.Fatalf("opened %+v, want one thread in the channel", th)
	}
	if l := f.runner.all(); len(l) != 1 || l[0].Input.Text != "do it" || l[0].Principal != "hook" {
		t.Fatalf("started %+v", l)
	}
	f.settled(t, th[0].Opened, chanConv)
	if got := strings.Join(postsIn(f.driver, th[0].Opened), "|"); got != "do it|marker" {
		t.Errorf("the thread shows %q", got)
	}
	if got := strings.Join(postsIn(f.driver, chanConv), "|"); got != "Started thread #1.|marker" {
		t.Errorf("the channel shows %q", got)
	}
}

// A stop command delivered again stops nothing more: not a turn started
// since the first.
func TestRepeatedStopCommandStopsOnce(t *testing.T) {
	f := newFront(t, workspaces{})
	f.deliver(chanConv, fake.Webhook(hook), "!go repo:https://example.com/r.git first")
	th := f.threads()[0].Opened
	f.fe.Outbox.Store.DB.Exec(`UPDATE sessions SET state = 'running'`)
	c := f.command(th, fake.Webhook(hook), "s1", stopCommand)
	f.fe.Command(c)
	f.runner.mu.Lock()
	stops := f.runner.stops
	f.runner.mu.Unlock()
	if stops != 1 {
		t.Fatalf("%d interrupts, want 1", stops)
	}
}

func TestAgentOptionIsNotACommand(t *testing.T) {
	f := &FrontEnd{Driver: fake.New(), Trigger: "!go"}
	for _, word := range []string{"stop", "status"} {
		if _, ok := f.textCommand(surface.Inbound{Text: "!go agent:other " + word}); ok {
			t.Errorf("agent option was treated as %s", word)
		}
	}
	o, _ := parseTrigger("!go", "!go agent:other new repo:https://example.com/r.git prompt")
	if got := f.newWords(o); got != o {
		t.Errorf("agent option changed the order: %+v", got)
	}
	o, _ = parseTrigger("!go", "!go new agent:other repo:https://example.com/r.git prompt")
	got := f.newWords(o)
	if got.Agent != "other" || got.Repo != "https://example.com/r.git" || got.Prompt != "prompt" {
		t.Errorf("new words parsed as %+v", got)
	}
}
