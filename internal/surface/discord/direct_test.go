package discord

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/disgoorg/disgo/gateway"

	"github.com/yousan/hataori/internal/surface"
)

func TestOpenDirectPostEdit(t *testing.T) {
	r := newFakeREST(t)
	r.serve(t, "POST /users/@me/channels", "person/rest-open-direct.json")
	dm := payload(t, "person/rest-open-direct.json")
	owner := id(capture(t, "person/rest-open-direct.json")["request"].(map[string]any)["body"].(map[string]any)["recipient_id"])
	posted := payload(t, "person/rest-post-button.json")
	r.serve(t, "POST /channels/"+id(dm["id"])+"/messages", "person/rest-post-button.json")
	r.serve(t, "PATCH /channels/"+id(dm["id"])+"/messages/"+id(posted["id"]), "person/rest-edit-direct.json")
	r.serve(t, "PATCH /channels/"+id(dm["id"])+"/messages/"+id(posted["id"]), "person/rest-edit-direct.json")
	r.serve(t, "GET /channels/"+id(dm["id"])+"/messages", "person/rest-history-direct.json")
	d := testDriver(t, botID(t), r, nil)
	ctx := context.Background()

	conv, err := d.OpenDirect(ctx, owner)
	if err != nil || conv != (surface.Conversation{Channel: id(dm["id"])}) {
		t.Fatalf("OpenDirect: %+v %v", conv, err)
	}
	b, _ := content(t, r.requests("POST /users/@me/channels")[0])
	if b["recipient_id"] != owner {
		t.Fatalf("opened with %v", b)
	}
	ref, err := d.Post(ctx, conv, "login-1", surface.Message{Text: "Log in again", Choices: []surface.Choice{{Label: "Log in", Token: "relay:7"}}})
	if err != nil || ref.ID != id(posted["id"]) {
		t.Fatalf("Post: %+v %v", ref, err)
	}
	b, _ = content(t, r.requests("POST /channels/" + id(dm["id"]) + "/messages")[0])
	rows, _ := b["components"].([]any)
	if len(rows) != 1 {
		t.Fatalf("components %v", b["components"])
	}
	btn := rows[0].(map[string]any)["components"].([]any)[0].(map[string]any)
	if btn["custom_id"] != "relay:7" || btn["label"] != "Log in" || btn["type"] != 2.0 {
		t.Fatalf("button %v", btn)
	}

	// An edit keeps or removes the button, as the new message says.
	editKey := "PATCH /channels/" + id(dm["id"]) + "/messages/" + id(posted["id"])
	if err := d.Edit(ctx, ref, surface.Message{Text: "Still waiting", Choices: []surface.Choice{{Label: "Log in", Token: "relay:7"}}}); err != nil {
		t.Fatal(err)
	}
	if err := d.Edit(ctx, ref, surface.Message{Text: "Logged in"}); err != nil {
		t.Fatal(err)
	}
	e1, _ := content(t, r.requests(editKey)[0])
	e2, _ := content(t, r.requests(editKey)[1])
	if len(e1["components"].([]any)) != 1 || len(e2["components"].([]any)) != 0 || e2["content"] != "Logged in" {
		t.Fatalf("edits %v / %v", e1, e2)
	}

	// The conversation is known to be direct: Since needs no lookup.
	got, err := d.Since(ctx, conv, "")
	if err != nil || len(got) != 1 || !got[0].Direct || len(r.requests("GET /channels/"+id(dm["id"]))) != 0 {
		t.Fatalf("Since: %+v %v", got, err)
	}
	if d.Limits().MaxChoices < 1 {
		t.Fatal("Limits does not offer one choice")
	}
}

func TestDirectMessageInbound(t *testing.T) {
	d := testDriver(t, botID(t), newFakeREST(t), nil)
	s := newSink()
	deliver(t, d, s, "person/gw-07-MESSAGE_CREATE.json")
	p := payload(t, "person/gw-07-MESSAGE_CREATE.json")
	if len(s.messages) != 1 {
		t.Fatalf("%d delivered", len(s.messages))
	}
	in := s.messages[0]
	if !in.Direct || in.Author.Kind != surface.Person || in.Author.ID != id(p["author"].(map[string]any)["id"]) ||
		in.Ref.Conv != (surface.Conversation{Channel: id(p["channel_id"])}) || in.Text != p["content"] {
		t.Fatalf("%+v", in)
	}
}

// slowSink takes its time over a press.
type slowSink struct {
	*sink
	delay time.Duration
}

func (s slowSink) Choice(c surface.Selection) {
	time.Sleep(s.delay)
	s.sink.Choice(c)
}

func TestPressAcknowledgedFirst(t *testing.T) {
	ev := capture(t, "person/gw-06-INTERACTION_CREATE.json")
	in := ev["d"].(map[string]any)
	r := newFakeREST(t)
	ack := "POST /interactions/" + id(in["id"]) + "/" + in["token"].(string) + "/callback"
	if want := "POST " + capture(t, "person/rest-interaction-ack.json")["request"].(map[string]any)["path"].(string); want != ack {
		t.Fatalf("the capture acknowledged %s, the press is %s", want, ack)
	}
	r.serve(t, ack, "person/rest-interaction-ack.json")
	d := testDriver(t, botID(t), r, nil)
	s := slowSink{newSink(), 4 * time.Second}

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- d.dispatch(context.Background(), "INTERACTION_CREATE", bytes.NewReader(raw(t, in)), s)
	}()
	for r.called(ack) == 0 {
		if time.Since(start) > 2500*time.Millisecond {
			t.Fatal("not acknowledged within the deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	b, _ := content(t, r.requests(ack)[0])
	if b["type"] != 6.0 {
		t.Fatalf("acknowledged with %v", b)
	}
	if len(s.choices) != 1 {
		t.Fatalf("%d choices", len(s.choices))
	}
	c := s.choices[0]
	user := in["user"].(map[string]any)
	msg := in["message"].(map[string]any)
	if c.Token != in["data"].(map[string]any)["custom_id"] || c.Author.ID != id(user["id"]) || c.Author.Kind != surface.Person ||
		c.Ref != (surface.MessageRef{Conv: surface.Conversation{Channel: id(in["channel_id"])}, ID: id(msg["id"])}) {
		t.Fatalf("%+v", c)
	}
}

// A button posted before a restart: a new driver, which posted nothing,
// delivers its press with the choice's token.
func TestPressAfterRestart(t *testing.T) {
	ev := capture(t, "person/gw-05-INTERACTION_CREATE.json")
	in := ev["d"].(map[string]any)
	r := newFakeREST(t)
	r.serve(t, "POST /interactions/"+id(in["id"])+"/"+in["token"].(string)+"/callback", "person/rest-interaction-ack.json")
	s := newSink()
	if err := testDriver(t, botID(t), r, nil).dispatch(context.Background(), "INTERACTION_CREATE", bytes.NewReader(raw(t, in)), s); err != nil {
		t.Fatal(err)
	}
	posted := capture(t, "person/rest-post-button.json")["request"].(map[string]any)["body"].(map[string]any)
	token := posted["components"].([]any)[0].(map[string]any)["components"].([]any)[0].(map[string]any)["custom_id"]
	if len(s.choices) != 1 || s.choices[0].Token != token {
		t.Fatalf("%+v, want the token %v the button was posted with", s.choices, token)
	}
}

func TestIntents(t *testing.T) {
	want := gateway.IntentGuilds | gateway.IntentGuildMessages | gateway.IntentMessageContent | gateway.IntentDirectMessages
	if Intents != want {
		t.Fatalf("intents %d, want exactly %d", Intents, want)
	}
}

// Through Run, a slow sink does not hold up the next press's
// acknowledgement: the sink is called off the gateway's receive loop.
func TestPressesNotHeldBySlowSink(t *testing.T) {
	r := newFakeREST(t)
	r.serve(t, "GET /users/@me", "bot/rest-users-me.json")
	var acks []string
	for _, f := range []string{"person/gw-05-INTERACTION_CREATE.json", "person/gw-06-INTERACTION_CREATE.json"} {
		in := payload(t, f)
		k := "POST /interactions/" + id(in["id"]) + "/" + in["token"].(string) + "/callback"
		r.serve(t, k, "person/rest-interaction-ack.json")
		acks = append(acks, k)
	}
	g := newFakeGateway(t, payload(t, "bot/gw-01-READY.json"))
	d := testDriver(t, botID(t), r, g)
	s := slowSink{newSink(), 4 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, s) }()
	select {
	case <-g.connected:
	case <-time.After(20 * time.Second):
		t.Fatal("no session")
	}
	start := time.Now()
	g.event("INTERACTION_CREATE", payload(t, "person/gw-05-INTERACTION_CREATE.json"))
	g.event("INTERACTION_CREATE", payload(t, "person/gw-06-INTERACTION_CREATE.json"))
	for len(r.requests(acks[0])) == 0 || len(r.requests(acks[1])) == 0 {
		if time.Since(start) > 2500*time.Millisecond {
			t.Fatalf("acknowledged %d and %d within the deadline", len(r.requests(acks[0])), len(r.requests(acks[1])))
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-s.got:
		case <-time.After(15 * time.Second):
			t.Fatal("a press was not delivered")
		}
	}
	cancel()
	<-done
	if s.choices[0].Ref.ID != s.choices[1].Ref.ID || len(s.choices) != 2 {
		t.Fatalf("%+v", s.choices)
	}
}

// A press in a server thread names the thread and its channel, from the
// channel object the press carries.
func TestPressInThread(t *testing.T) {
	in := payload(t, "person/gw-06-INTERACTION_CREATE.json")
	th := payload(t, "bot/gw-05-THREAD_CREATE.json")
	in["channel_id"], in["guild_id"] = th["id"], th["guild_id"]
	in["channel"] = map[string]any{"id": th["id"], "type": 11.0, "parent_id": th["parent_id"]}
	r := newFakeREST(t)
	r.serve(t, "POST /interactions/"+id(in["id"])+"/"+in["token"].(string)+"/callback", "person/rest-interaction-ack.json")
	s := newSink()
	if err := testDriver(t, botID(t), r, nil).dispatch(context.Background(), "INTERACTION_CREATE", bytes.NewReader(raw(t, in)), s); err != nil {
		t.Fatal(err)
	}
	if len(s.choices) != 1 || s.choices[0].Ref.Conv != (surface.Conversation{Channel: id(th["parent_id"]), Thread: id(th["id"])}) {
		t.Fatalf("%+v", s.choices)
	}
}
