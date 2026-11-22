package core

import (
	"context"
	"reflect"
	"testing"

	"github.com/yousan/hataori/internal/control"
)

// A chat's layout does not widen who sees what: the owner sees every
// channel, anyone else only the channels of the scopes they may see and
// the groups that hold them; a thread names its number only to a caller
// that may see its scope; names are text. Recording a layout writes one
// event, which every reader gets, and the same layout again writes none.
func TestLayoutWhoSeesWhat(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeOwner{key: "new"})
	id, _ := f.running(t, "t1") // linked to chat s/t1
	front := f.core.As(control.Caller{Principal: "owner", Scopes: []string{"s", "off"}, May: control.OpName})
	recorded := []control.LayoutGroup{
		{Channels: []control.LayoutChannel{{ID: "u", Name: "#lobby", URL: "https://chat.example/u"}}},
		{ID: "g1", Name: "Work", Channels: []control.LayoutChannel{
			{ID: "s", Name: "#builds", URL: "https://chat.example/s", Scope: "forged", Threads: []control.LayoutThread{
				{ID: "t1", Name: "Fix it", URL: "https://chat.example/t1"},
				{ID: "p1", Name: "Lunch", URL: "https://chat.example/p1", Thread: 99}, // forged numbers are dropped
			}},
		}},
		{ID: "g2", Name: "Old", Channels: []control.LayoutChannel{{ID: "off", Name: "#off"}}},
	}
	events := func() int { return count(t, f.core.Store, "events") }
	before := events()
	if err := front.SetLayout(ctx, "chat", recorded); err != nil {
		t.Fatal(err)
	}
	if err := front.SetLayout(ctx, "chat", recorded); err != nil {
		t.Fatal(err)
	}
	if n := events() - before; n != 1 {
		t.Fatalf("two SetLayout calls with one layout wrote %d events, want 1", n)
	}
	if err := front.SetLayout(ctx, "another", recorded); err == nil {
		t.Fatal("a layout of a client that owns none of the caller's scopes was taken")
	}

	lobby := control.LayoutChannel{ID: "u", Name: "#lobby", URL: "https://chat.example/u", Threads: []control.LayoutThread{}}
	builds := control.LayoutChannel{ID: "s", Name: "#builds", URL: "https://chat.example/s", Scope: "s", Threads: []control.LayoutThread{
		{ID: "t1", Name: "Fix it", URL: "https://chat.example/t1", Thread: id},
		{ID: "p1", Name: "Lunch", URL: "https://chat.example/p1"},
	}}
	off := control.LayoutChannel{ID: "off", Name: "#off", Scope: "off", Threads: []control.LayoutThread{}}
	for _, c := range []struct {
		name   string
		caller control.Caller
		want   []control.LayoutGroup
	}{
		{"the owner", control.Caller{Principal: "owner", Scopes: []string{"s", "off"}, May: control.OpRead}, []control.LayoutGroup{
			{Channels: []control.LayoutChannel{lobby}},
			{ID: "g1", Name: "Work", Channels: []control.LayoutChannel{builds}},
			{ID: "g2", Name: "Old", Channels: []control.LayoutChannel{off}},
		}},
		{"the owner without a scope", control.Caller{Principal: "owner", Scopes: []string{"off"}, May: control.OpRead}, []control.LayoutGroup{
			{Channels: []control.LayoutChannel{lobby}},
			{ID: "g1", Name: "Work", Channels: []control.LayoutChannel{{ID: "s", Name: "#builds", URL: "https://chat.example/s",
				Threads: []control.LayoutThread{{ID: "t1", Name: "Fix it", URL: "https://chat.example/t1"}, {ID: "p1", Name: "Lunch", URL: "https://chat.example/p1"}}}}},
			{ID: "g2", Name: "Old", Channels: []control.LayoutChannel{off}},
		}},
		{"a person of s", control.Caller{Principal: "ada", Scopes: []string{"s"}, May: control.OpRead}, []control.LayoutGroup{
			{ID: "g1", Name: "Work", Channels: []control.LayoutChannel{builds}},
		}},
		{"a person of off", control.Caller{Principal: "ada", Scopes: []string{"off"}, May: control.OpRead}, []control.LayoutGroup{
			{ID: "g2", Name: "Old", Channels: []control.LayoutChannel{off}},
		}},
		{"a person of no scope", control.Caller{Principal: "ada", May: control.OpRead}, []control.LayoutGroup{}},
		{"a reader of no text", control.Caller{Principal: "ada", Scopes: []string{"s"}, May: control.OpRead, NoText: true}, []control.LayoutGroup{
			{ID: "g1", Channels: []control.LayoutChannel{{ID: "s", URL: "https://chat.example/s", Scope: "s", Threads: []control.LayoutThread{
				{ID: "t1", URL: "https://chat.example/t1", Thread: id}, {ID: "p1", URL: "https://chat.example/p1"}}}}},
		}},
	} {
		got, err := f.core.As(c.caller).Layout(ctx)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		want := []control.ChatLayout{{Client: "chat", Groups: c.want}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s sees\n%+v\nwant\n%+v", c.name, got, want)
		}
	}

	// A reader of another scope gets the event too: it says only that the
	// layout changed.
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := f.core.As(control.Caller{Principal: "ada", Scopes: []string{"elsewhere"}, May: control.OpRead}).Events(rctx, control.Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []control.EventKind
	for e := range ch {
		if e.Kind == control.CaughtUp {
			break
		}
		kinds = append(kinds, e.Kind)
	}
	if !reflect.DeepEqual(kinds, []control.EventKind{KindLayout}) {
		t.Errorf("a reader of another scope got %v, want one layout event", kinds)
	}
}
