package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/yousan/hataori/internal/surface"
)

// testSpecs are the commands the captures registered (commands/rest-commands-set.json).
var testSpecs = []surface.CommandSpec{{
	Name:        "hataori-test",
	Description: "[hataori test] a command for captures",
	Args: []surface.ArgSpec{
		{Name: "hataori-test-text", Description: "[hataori test] required text", Required: true},
		{Name: "hataori-test-note", Description: "[hataori test] optional text"},
	},
}}

// commandSink is a sink that handles commands.
type commandSink struct {
	*sink
	specs []surface.CommandSpec
}

func (s commandSink) Commands() []surface.CommandSpec { return s.specs }

func appID(t *testing.T) string { return id(payload(t, "commands/rest-application.json")["id"]) }

func commandsKey(t *testing.T) string { return "/applications/" + appID(t) + "/commands" }

// runOnce runs d with s until the session is ready, then stops it.
func runOnce(t *testing.T, d *Driver, g *fakeGateway, s surface.Sink) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, s) }()
	select {
	case <-g.connected:
	case err := <-done:
		cancel()
		t.Fatalf("Run: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("no session")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The commands are registered when the driver starts, with what the service
// accepted in the capture; a start with the commands the service already
// has (as it reported them in the capture) registers nothing.
func TestCommandsRegisteredOnce(t *testing.T) {
	r := newFakeREST(t)
	r.serve(t, "GET /users/@me", "bot/rest-users-me.json")
	r.serve(t, "GET /applications/@me", "commands/rest-application.json")
	r.serveJSON(t, "GET "+commandsKey(t), 200, []any{})
	r.serve(t, "GET "+commandsKey(t), "commands/rest-commands-after.json")
	r.serve(t, "PUT "+commandsKey(t), "commands/rest-commands-set.json")
	g := newFakeGateway(t, payload(t, "bot/gw-01-READY.json"))
	s := commandSink{newSink(), testSpecs}

	runOnce(t, testDriver(t, botID(t), r, g), g, s)
	puts := r.requests("PUT " + commandsKey(t))
	if len(puts) != 1 {
		t.Fatalf("%d registrations, want 1", len(puts))
	}
	var got, want []wireCommand
	if err := json.Unmarshal(puts[0].body, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw(t, capture(t, "commands/rest-commands-set.json")["request"].(map[string]any)["body"]), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("registered %+v, the capture %+v", got, want)
	}

	runOnce(t, testDriver(t, botID(t), r, g), g, s)
	if n := len(r.requests("PUT " + commandsKey(t))); n != 1 {
		t.Fatalf("%d registrations after starting again with the same commands, want 1", n)
	}
	if n := len(r.requests("GET " + commandsKey(t))); n != 2 {
		t.Fatalf("read the commands %d times, want 2", n)
	}
}

// A changed description is registered again.
func TestChangedCommandsRegistered(t *testing.T) {
	r := newFakeREST(t)
	r.serve(t, "GET /applications/@me", "commands/rest-application.json")
	r.serve(t, "GET "+commandsKey(t), "commands/rest-commands-after.json")
	r.serve(t, "PUT "+commandsKey(t), "commands/rest-commands-set.json")
	specs := []surface.CommandSpec{testSpecs[0]}
	specs[0].Description = "[hataori test] changed"
	if err := testDriver(t, botID(t), r, nil).register(context.Background(), specs); err != nil {
		t.Fatal(err)
	}
	if n := len(r.requests("PUT " + commandsKey(t))); n != 1 {
		t.Fatalf("%d registrations, want 1", n)
	}
}

func TestCommandSpecsRefused(t *testing.T) {
	for _, specs := range [][]surface.CommandSpec{
		{{Name: "New", Description: "upper case"}},
		{{Name: "new"}},
		{{Name: "new", Description: "d"}, {Name: "new", Description: "d"}},
		{{Name: "new", Description: "d", Args: []surface.ArgSpec{{Name: "a", Description: "d"}, {Name: "b", Description: "d", Required: true}}}},
	} {
		r := newFakeREST(t)
		if err := testDriver(t, botID(t), r, nil).register(context.Background(), specs); err == nil {
			t.Errorf("%+v accepted", specs)
		}
		if r.called("") != 0 {
			t.Errorf("%+v: the service was called", specs)
		}
	}
}

func ackKey(in map[string]any) string {
	return "POST /interactions/" + id(in["id"]) + "/" + in["token"].(string) + "/callback"
}

func answerKey(in map[string]any) string {
	return "/webhooks/" + id(in["application_id"]) + "/" + in["token"].(string) + "/messages/@original"
}

// command delivers a captured command through dispatch and returns it.
func command(t *testing.T, d *Driver, r *fakeREST, name, ack string) (map[string]any, surface.Command) {
	t.Helper()
	in := payload(t, name)
	r.serve(t, ackKey(in), ack)
	s := newSink()
	if err := d.dispatch(context.Background(), "INTERACTION_CREATE", bytes.NewReader(raw(t, in)), s); err != nil {
		t.Fatal(err)
	}
	if len(s.commands) != 1 {
		t.Fatalf("%d commands delivered", len(s.commands))
	}
	return in, s.commands[0]
}

func TestCommandInChannel(t *testing.T) {
	r := newFakeREST(t)
	d := testDriver(t, botID(t), r, nil)
	in, c := command(t, d, r, "commands/gw-01-INTERACTION_CREATE.json", "commands/rest-command-ack-channel.json")
	user := in["member"].(map[string]any)["user"].(map[string]any)
	want := surface.Command{
		Conv:   surface.Conversation{Channel: id(in["channel_id"])},
		Author: surface.Author{Kind: surface.Person, ID: id(user["id"]), Name: user["global_name"].(string)},
		ID:     id(in["id"]),
		Name:   "hataori-test",
		Args:   []surface.Arg{{Name: "hataori-test-text", Value: "channel"}},
	}
	if c.Reply == nil {
		t.Fatal("no Reply")
	}
	reply := *c.Reply
	c.Reply = nil
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("%+v\nwant %+v", c, want)
	}
	b, _ := content(t, r.requests(ackKey(in))[0])
	if b["type"] != 5.0 || b["data"] != nil {
		t.Fatalf("acknowledged with %v, want a public deferred answer", b)
	}

	// The front end answers with a Post that replies to the command.
	r.serve(t, "PATCH "+answerKey(in), "commands/rest-command-answer-channel.json")
	ref, err := d.Post(context.Background(), c.Conv, "k1", surface.Message{Text: "[hataori test] answered in the channel", ReplyTo: &reply})
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != id(payload(t, "commands/rest-command-answer-channel.json")["id"]) || ref.Conv != c.Conv {
		t.Fatalf("%+v", ref)
	}
	if n := r.called("POST /channels/"); n != 0 {
		t.Fatalf("%d posts into the channel; the answer replaces the deferred one", n)
	}
	b, _ = content(t, r.requests("PATCH " + answerKey(in))[0])
	if b["content"] != "[hataori test] answered in the channel" || !reflect.DeepEqual(b["allowed_mentions"], map[string]any{"parse": []any{}}) {
		t.Fatalf("answered with %v", b)
	}
	// The same key again does not answer twice.
	if again, err := d.Post(context.Background(), c.Conv, "k1", surface.Message{Text: "x", ReplyTo: &reply}); err != nil || again != ref {
		t.Fatalf("%+v %v", again, err)
	}
	if n := r.called("PATCH " + answerKey(in)); n != 1 {
		t.Fatalf("answered %d times", n)
	}
}

// In a thread the command names the thread and its channel; a command whose
// spec is private is acknowledged so that only its user sees the answer.
func TestCommandInThread(t *testing.T) {
	r := newFakeREST(t)
	d := testDriver(t, botID(t), r, nil)
	specs := []surface.CommandSpec{testSpecs[0]}
	specs[0].Private = true
	d.commands = map[string]surface.CommandSpec{"hataori-test": specs[0]}
	in, c := command(t, d, r, "commands/gw-02-INTERACTION_CREATE.json", "commands/rest-command-ack-thread.json")
	ch := in["channel"].(map[string]any)
	if c.Conv != (surface.Conversation{Channel: id(ch["parent_id"]), Thread: id(in["channel_id"])}) {
		t.Fatalf("%+v", c.Conv)
	}
	user := in["member"].(map[string]any)["user"].(map[string]any)
	if c.Author != (surface.Author{Kind: surface.Person, ID: id(user["id"]), Name: user["global_name"].(string)}) {
		t.Fatalf("%+v", c.Author)
	}
	if want := []surface.Arg{{Name: "hataori-test-text", Value: "thread"}, {Name: "hataori-test-note", Value: "note"}}; !reflect.DeepEqual(c.Args, want) {
		t.Fatalf("%+v", c.Args)
	}
	b, _ := content(t, r.requests(ackKey(in))[0])
	if want := capture(t, "commands/rest-command-ack-thread.json")["request"].(map[string]any)["body"]; !reflect.DeepEqual(b, want) {
		t.Fatalf("acknowledged with %v, want %v", b, want)
	}
	r.serve(t, "PATCH "+answerKey(in), "commands/rest-command-answer-thread.json")
	if _, err := d.Post(context.Background(), c.Conv, "", surface.Message{Text: "[hataori test] answered in the thread", ReplyTo: c.Reply}); err != nil {
		t.Fatal(err)
	}
	// A second answer is a follow-up, private as well.
	follow := "POST /webhooks/" + id(in["application_id"]) + "/" + in["token"].(string)
	r.serve(t, follow, "commands/rest-command-answer-thread.json")
	if _, err := d.Post(context.Background(), c.Conv, "", surface.Message{Text: "more", ReplyTo: c.Reply}); err != nil {
		t.Fatal(err)
	}
	b, _ = content(t, r.requests(follow)[0])
	if b["flags"] != 64.0 {
		t.Fatalf("follow-up %v is not private", b)
	}
}

// A command that can no longer be answered, here after a restart, fails
// with an error that is not temporary, and nothing is sent.
func TestAnswerAfterRestart(t *testing.T) {
	r := newFakeREST(t)
	in, c := command(t, testDriver(t, botID(t), r, nil), r, "commands/gw-01-INTERACTION_CREATE.json", "commands/rest-command-ack-channel.json")
	_, err := testDriver(t, botID(t), r, nil).Post(context.Background(), c.Conv, "k", surface.Message{Text: "late", ReplyTo: c.Reply})
	var e *Error
	if !errors.As(err, &e) || e.Temporary() {
		t.Fatalf("%v", err)
	}
	if r.called("PATCH "+answerKey(in)) != 0 {
		t.Fatal("answered")
	}
}

func TestCommandFromBotNotDelivered(t *testing.T) {
	in := payload(t, "commands/gw-01-INTERACTION_CREATE.json")
	in["member"].(map[string]any)["user"].(map[string]any)["bot"] = true
	r := newFakeREST(t)
	r.serve(t, ackKey(in), "commands/rest-command-ack-channel.json")
	s := newSink()
	if err := testDriver(t, botID(t), r, nil).dispatch(context.Background(), "INTERACTION_CREATE", bytes.NewReader(raw(t, in)), s); err != nil {
		t.Fatal(err)
	}
	if len(s.commands) != 0 {
		t.Fatalf("%+v", s.commands)
	}
}

// blockingSink never returns from Command until released.
type blockingSink struct {
	commandSink
	release chan struct{}
}

func (s blockingSink) Command(c surface.Command) {
	<-s.release
	s.sink.Command(c)
}

// Through Run, every command is acknowledged before the deadline while the
// sink is blocked on the one before.
func TestCommandsAcknowledgedWhileSinkBlocks(t *testing.T) {
	r := newFakeREST(t)
	r.serve(t, "GET /users/@me", "bot/rest-users-me.json")
	r.serve(t, "GET /applications/@me", "commands/rest-application.json")
	r.serve(t, "GET "+commandsKey(t), "commands/rest-commands-after.json")
	ins := []map[string]any{payload(t, "commands/gw-01-INTERACTION_CREATE.json"), payload(t, "commands/gw-02-INTERACTION_CREATE.json")}
	r.serve(t, ackKey(ins[0]), "commands/rest-command-ack-channel.json")
	r.serve(t, ackKey(ins[1]), "commands/rest-command-ack-thread.json")
	g := newFakeGateway(t, payload(t, "bot/gw-01-READY.json"))
	d := testDriver(t, botID(t), r, g)
	s := blockingSink{commandSink{newSink(), testSpecs}, make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, s) }()
	select {
	case <-g.connected:
	case err := <-done:
		t.Fatalf("Run: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("no session")
	}
	start := time.Now()
	g.event("INTERACTION_CREATE", ins[0])
	g.event("INTERACTION_CREATE", ins[1])
	for len(r.requests(ackKey(ins[0]))) == 0 || len(r.requests(ackKey(ins[1]))) == 0 {
		if time.Since(start) > ackWithin {
			t.Fatalf("acknowledged %d and %d within the deadline", len(r.requests(ackKey(ins[0]))), len(r.requests(ackKey(ins[1]))))
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(s.release)
	for i := 0; i < 2; i++ {
		select {
		case <-s.got:
		case <-time.After(15 * time.Second):
			t.Fatal("a command was not delivered")
		}
	}
	cancel()
	<-done
	if s.commands[0].ID != id(ins[0]["id"]) || s.commands[1].ID != id(ins[1]["id"]) {
		t.Fatalf("%+v", s.commands)
	}
}

func TestLimitsCommands(t *testing.T) {
	if !testDriver(t, snowflake.ID(1), newFakeREST(t), nil).Limits().Commands {
		t.Fatal("Limits does not report commands")
	}
}

// A long answer whose second part fails is retried with the same key: only
// the missing part is sent.
func TestAnswerRetryResumes(t *testing.T) {
	r := newFakeREST(t)
	d := testDriver(t, botID(t), r, nil)
	in, c := command(t, d, r, "commands/gw-01-INTERACTION_CREATE.json", "commands/rest-command-ack-channel.json")
	follow := "POST /webhooks/" + id(in["application_id"]) + "/" + in["token"].(string)
	r.serve(t, "PATCH "+answerKey(in), "commands/rest-command-answer-channel.json")
	r.serveJSON(t, follow, 400, map[string]any{"message": "refused", "code": 50035})
	r.serve(t, follow, "commands/rest-command-answer-channel.json")
	long := strings.Repeat("a", maxText) + "b"
	m := surface.Message{Text: long, ReplyTo: c.Reply}
	if _, err := d.Post(context.Background(), c.Conv, "k", m); err == nil {
		t.Fatal("the failed part did not fail the Post")
	}
	ref, err := d.Post(context.Background(), c.Conv, "k", m)
	if err != nil {
		t.Fatal(err)
	}
	if n := r.called("PATCH " + answerKey(in)); n != 1 {
		t.Fatalf("the first part was sent %d times", n)
	}
	if n := r.called(follow); n != 2 {
		t.Fatalf("%d follow-ups, want the failed one and its retry", n)
	}
	if ref.ID != id(payload(t, "commands/rest-command-answer-channel.json")["id"]) {
		t.Fatalf("%+v", ref)
	}
}

// Two answers at once: one replaces the deferred response, the other is a
// follow-up.
func TestConcurrentAnswers(t *testing.T) {
	r := newFakeREST(t)
	d := testDriver(t, botID(t), r, nil)
	in, c := command(t, d, r, "commands/gw-01-INTERACTION_CREATE.json", "commands/rest-command-ack-channel.json")
	follow := "POST /webhooks/" + id(in["application_id"]) + "/" + in["token"].(string)
	r.serve(t, "PATCH "+answerKey(in), "commands/rest-command-answer-channel.json")
	r.serve(t, follow, "commands/rest-command-answer-channel.json")
	var wg sync.WaitGroup
	for _, k := range []string{"a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.Post(context.Background(), c.Conv, k, surface.Message{Text: k, ReplyTo: c.Reply}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if p, f := r.called("PATCH "+answerKey(in)), r.called(follow); p != 1 || f != 1 {
		t.Fatalf("%d replacements and %d follow-ups, want 1 and 1", p, f)
	}
}
