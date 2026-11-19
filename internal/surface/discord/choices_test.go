package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/yousan/hataori/internal/surface"
)

func choicesN(n int) []surface.Choice {
	var cs []surface.Choice
	for i := 1; i <= n; i++ {
		cs = append(cs, surface.Choice{Label: fmt.Sprintf("choice %d", i), Token: fmt.Sprintf("t%d", i)})
	}
	return cs
}

// postKey is the route the choices captures posted to.
func postKey(t *testing.T) (surface.Conversation, string) {
	ch := id(payload(t, "choices/rest-post-buttons.json")["channel_id"])
	return surface.Conversation{Channel: ch}, "POST /channels/" + ch + "/messages"
}

// tokens are the custom_ids of the buttons of a posted body, row by row.
func buttonTokens(t *testing.T, body map[string]any) []string {
	t.Helper()
	var out []string
	for _, row := range body["components"].([]any) {
		r := row.(map[string]any)
		if r["type"] != 1.0 {
			t.Fatalf("row %v", r)
		}
		bs := r["components"].([]any)
		if len(bs) > maxButtonsPerRow {
			t.Fatalf("%d buttons in a row", len(bs))
		}
		for _, b := range bs {
			out = append(out, b.(map[string]any)["custom_id"].(string))
		}
	}
	return out
}

// Every N up to Limits is posted as N buttons in the order given; one more
// is refused before anything is posted.
func TestChoicesUpToLimit(t *testing.T) {
	r := newFakeREST(t)
	conv, key := postKey(t)
	r.serve(t, key, "choices/rest-post-buttons.json")
	d := testDriver(t, botID(t), r, nil)
	max := d.Limits().MaxChoices
	if max != 25 {
		t.Fatalf("MaxChoices %d", max)
	}
	for n := 1; n <= max; n++ {
		cs := choicesN(n)
		if _, err := d.Post(context.Background(), conv, "", surface.Message{Text: "pick", Choices: cs}); err != nil {
			t.Fatalf("%d choices: %v", n, err)
		}
		reqs := r.requests(key)
		b, _ := content(t, reqs[len(reqs)-1])
		var want []string
		for _, c := range cs {
			want = append(want, c.Token)
		}
		if got := buttonTokens(t, b); !reflect.DeepEqual(got, want) {
			t.Fatalf("%d choices posted as %v", n, got)
		}
	}
	before := len(r.requests(key))
	for _, multi := range []bool{false, true} {
		if _, err := d.Post(context.Background(), conv, "", surface.Message{Text: "pick", Choices: choicesN(max + 1), Multi: multi}); err == nil {
			t.Fatalf("%d choices posted (multi %v)", max+1, multi)
		}
	}
	if len(r.requests(key)) != before {
		t.Fatal("something was posted")
	}
}

// The buttons the captures posted are what the driver posts for the same
// choices, apart from the style.
func TestButtonsLikeCapture(t *testing.T) {
	sent := capture(t, "choices/rest-post-buttons.json")["request"].(map[string]any)["body"].(map[string]any)
	var cs []surface.Choice
	for _, b := range sent["components"].([]any)[0].(map[string]any)["components"].([]any) {
		m := b.(map[string]any)
		cs = append(cs, surface.Choice{Label: m["label"].(string), Token: m["custom_id"].(string)})
	}
	got, err := components(cs, false)
	if err != nil {
		t.Fatal(err)
	}
	g := raw(t, got)
	var back []any
	if err := json.Unmarshal(g, &back); err != nil {
		t.Fatal(err)
	}
	for _, b := range back[0].(map[string]any)["components"].([]any) {
		b.(map[string]any)["style"] = 2.0 // the capture used grey buttons
	}
	if !reflect.DeepEqual(back, sent["components"]) {
		t.Fatalf("%v\nthe capture %v", back, sent["components"])
	}
}

// A multi-select message is posted as one menu with every choice, any number
// of which may be chosen.
func TestMenuPosted(t *testing.T) {
	r := newFakeREST(t)
	conv, key := postKey(t)
	r.serve(t, key, "choices/rest-post-menu.json")
	d := testDriver(t, botID(t), r, nil)
	if !d.Limits().MultiSelect {
		t.Fatal("Limits does not report multi-select")
	}
	if _, err := d.Post(context.Background(), conv, "", surface.Message{Text: "pick", Choices: choicesN(3), Multi: true}); err != nil {
		t.Fatal(err)
	}
	b, _ := content(t, r.requests(key)[0])
	rows := b["components"].([]any)
	menu := rows[0].(map[string]any)["components"].([]any)[0].(map[string]any)
	if len(rows) != 1 || menu["type"] != 3.0 || menu["min_values"] != 1.0 || menu["max_values"] != 3.0 {
		t.Fatalf("%v", rows)
	}
	var values []string
	for _, o := range menu["options"].([]any) {
		values = append(values, o.(map[string]any)["value"].(string))
	}
	if !reflect.DeepEqual(values, []string{"t1", "t2", "t3"}) {
		t.Fatalf("options %v", values)
	}
	// The menu has the shape the service accepted in the capture.
	posted := payload(t, "choices/rest-post-menu.json")["components"].([]any)[0].(map[string]any)["components"].([]any)[0].(map[string]any)
	if posted["type"] != menu["type"] || len(posted["options"].([]any)) != 3 {
		t.Fatalf("the capture's menu %v", posted)
	}
}

func TestButtonPressCapture(t *testing.T) {
	in := payload(t, "choices/gw-01-INTERACTION_CREATE.json")
	r := newFakeREST(t)
	r.serve(t, ackKey(in), "choices/rest-button-ack.json")
	s := newSink()
	if err := testDriver(t, botID(t), r, nil).dispatch(context.Background(), "INTERACTION_CREATE", bytes.NewReader(raw(t, in)), s); err != nil {
		t.Fatal(err)
	}
	user := in["member"].(map[string]any)["user"].(map[string]any)
	want := surface.Selection{
		Ref:    surface.MessageRef{Conv: surface.Conversation{Channel: id(in["channel_id"])}, ID: id(payload(t, "choices/rest-post-buttons.json")["id"])},
		Author: surface.Author{Kind: surface.Person, ID: id(user["id"]), Name: user["global_name"].(string)},
		Token:  "hataori-test-button-2",
		Tokens: []string{"hataori-test-button-2"},
	}
	if len(s.choices) != 1 || !reflect.DeepEqual(s.choices[0], want) {
		t.Fatalf("%+v\nwant %+v", s.choices, want)
	}
	if b, _ := content(t, r.requests(ackKey(in))[0]); b["type"] != 6.0 {
		t.Fatalf("acknowledged with %v", b)
	}
}

func TestMenuSelectionCapture(t *testing.T) {
	in := payload(t, "choices/gw-02-INTERACTION_CREATE.json")
	r := newFakeREST(t)
	r.serve(t, ackKey(in), "choices/rest-menu-ack.json")
	s := newSink()
	if err := testDriver(t, botID(t), r, nil).dispatch(context.Background(), "INTERACTION_CREATE", bytes.NewReader(raw(t, in)), s); err != nil {
		t.Fatal(err)
	}
	if len(s.choices) != 1 {
		t.Fatalf("%d selections", len(s.choices))
	}
	c := s.choices[0]
	user := in["member"].(map[string]any)["user"].(map[string]any)
	if !reflect.DeepEqual(c.Tokens, []string{"hataori-test-option-1", "hataori-test-option-3"}) || c.Token != "hataori-test-option-1" ||
		c.Author.ID != id(user["id"]) || c.Ref.ID != id(payload(t, "choices/rest-post-menu.json")["id"]) {
		t.Fatalf("%+v", c)
	}
}

// Editing with no choices removes the buttons and the menu; the service
// then reports none (the captures).
func TestEditRemovesChoices(t *testing.T) {
	for _, name := range []string{"buttons", "menu"} {
		posted := payload(t, "choices/rest-post-"+name+".json")
		ch, msg := id(posted["channel_id"]), id(posted["id"])
		key := "PATCH /channels/" + ch + "/messages/" + msg
		r := newFakeREST(t)
		r.serve(t, key, "choices/rest-edit-remove-"+name+".json")
		ref := surface.MessageRef{Conv: surface.Conversation{Channel: ch}, ID: msg}
		if err := testDriver(t, botID(t), r, nil).Edit(context.Background(), ref, surface.Message{Text: "[hataori test] answered"}); err != nil {
			t.Fatal(err)
		}
		b, _ := content(t, r.requests(key)[0])
		if c, ok := b["components"].([]any); !ok || len(c) != 0 {
			t.Fatalf("%s: edited with %v", name, b)
		}
		if after := payload(t, "choices/rest-edit-remove-"+name+".json")["components"].([]any); len(after) != 0 {
			t.Fatalf("%s: the service kept %v", name, after)
		}
	}
}
