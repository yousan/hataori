package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// A turn typed at the agent's terminal through `hataori attach` shows its
// prompt in every chat link of the thread as "<owner>: <text>", then its
// final answer after "(typed at the terminal)" as before; its input event
// comes right after the turn's turn_started, with via terminal, the owner
// and no from and no link, and a caller that may not read text gets it
// without its text (#609).
func TestTypedPromptShowsInEveryLink(t *testing.T) {
	r := attachRoot(t, "1h", "1h")
	setConfig(t, r.root, func(cfg map[string]any) {
		cfg["scopes"].([]any)[0].(map[string]any)["show_in"] = []string{fakeSurface + ":c3"}
	})
	var c *core.Core
	coreHook = func(got *core.Core) { c = got }
	t.Cleanup(func() { coreHook = nil })
	startDaemon(t, r.root)
	r.connected(t)
	one := r.order(t, 0, "!go repo:"+bareRepo(t)+" screen on-the-screen")
	r.posted(t, one, "done")
	n := numberOf(t, r.root, one)
	c3 := surface.Conversation{Channel: "c3"}
	r.driver.InjectCommand(c3, fakesurface.Webhook(e2eHook), "attach", surface.Arg{Name: "number", Value: n})
	var here surface.Conversation
	eventually(t, "the link in c3", 15*time.Second, func() bool {
		o := opened(r.driver, c3)
		if len(o) == 1 {
			here = o[0]
		}
		return len(o) == 1 && len(linksOf(t, r.root, n)) == 2
	})

	a := attach(t, r.root, n)
	eventually(t, "the screen", 15*time.Second, func() bool { return strings.Contains(a.out.String(), "on-the-screen") })
	a.keys.Write([]byte("reply typed here\r"))
	for _, conv := range []surface.Conversation{one, here} {
		r.posted(t, conv, "(typed at the terminal)\ntyped here")
		if got := postsLike(r.driver, conv, "owner: reply typed here"); len(got) != 1 {
			t.Fatalf("%v shows the typed prompt %d times, want once", conv, len(got))
		}
	}
	a.detach(t)

	ctx := context.Background()
	id := threadOf(t, r.root).ID
	read := func(noText bool) []control.Event {
		t.Helper()
		evs, err := c.As(control.Caller{Principal: "owner", Scopes: []string{"c1"}, May: control.OpRead, NoText: noText}).ThreadEvents(ctx, id, 0, control.MaxEvents)
		if err != nil {
			t.Fatal(err)
		}
		return evs
	}
	evs := read(false)
	var typed, started int = -1, -1
	for i, e := range evs {
		if e.Kind == "turn_started" && strings.Contains(string(e.Data), `"by":"terminal"`) {
			started = i
		}
		if e.Kind == "input" && strings.Contains(string(e.Data), `"via":"terminal"`) {
			typed = i
		}
	}
	if started < 0 || typed != started+1 {
		t.Fatalf("the typed turn's turn_started at %d and its input at %d, want the input right after", started, typed)
	}
	var d map[string]any
	json.Unmarshal(evs[typed].Data, &d)
	if d["text"] != "reply typed here" || d["principal"] != "owner" || d["from"] != nil || d["link"] != nil {
		t.Fatalf("the typed prompt's input event %v", d)
	}
	var without map[string]any
	json.Unmarshal(read(true)[typed].Data, &without)
	if _, ok := without["text"]; ok || without["via"] != "terminal" {
		t.Fatalf("a caller that may not read text gets the typed prompt: %v", without)
	}
}
