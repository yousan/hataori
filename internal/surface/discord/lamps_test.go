package discord

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/surface"
)

// lampMessage is the message the lamp captures reacted to.
func lampMessage(t *testing.T) (surface.MessageRef, string) {
	p := capture(t, "lamps/rest-lamp-get-working.json")["request"].(map[string]any)["path"].(string)
	parts := strings.Split(p, "/") // /channels/C/messages/M
	ref := surface.MessageRef{Conv: surface.Conversation{Channel: parts[2]}, ID: parts[4]}
	return ref, p
}

func reaction(ref surface.MessageRef, emoji string) string {
	return "/channels/" + ref.Conv.Channel + "/messages/" + ref.ID + "/reactions/" + emoji + "/@me"
}

// changed lists the reaction calls made, in order.
func changed(r *fakeREST) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		if strings.Contains(c, "/reactions") {
			out = append(out, c)
		}
	}
	return out
}

// working -> done, and working -> error: the new lamp is added and the
// bot's working lamp removed, so exactly the new one is left; the service's
// view after working -> done (lamp-get-done) has only the done lamp.
func TestSetStatusReplacesLamp(t *testing.T) {
	for _, tc := range []struct {
		to    surface.Status
		emoji string
	}{{surface.Done, "✅"}, {surface.Failed, "❌"}} {
		ref, get := lampMessage(t)
		r := newFakeREST(t)
		r.serve(t, "GET "+get, "lamps/rest-lamp-get-working.json")
		r.serve(t, "PUT "+reaction(ref, tc.emoji), "lamps/rest-lamp-add-done.json")
		r.serve(t, "DELETE "+reaction(ref, "⏳"), "lamps/rest-lamp-remove-working.json")
		if err := testDriver(t, botID(t), r, nil).SetStatus(context.Background(), ref, tc.to); err != nil {
			t.Fatal(err)
		}
		want := []string{"PUT " + reaction(ref, tc.emoji), "DELETE " + reaction(ref, "⏳")}
		if got := changed(r); !reflect.DeepEqual(got, want) {
			t.Fatalf("%v: %v, want %v", tc.to, got, want)
		}
	}
	var after []any
	for _, x := range payload(t, "lamps/rest-lamp-get-done.json")["reactions"].([]any) {
		if x.(map[string]any)["me"] == true {
			after = append(after, x.(map[string]any)["emoji"].(map[string]any)["name"])
		}
	}
	if !reflect.DeepEqual(after, []any{"✅"}) {
		t.Fatalf("the service shows the bot's %v after working -> done", after)
	}
}

// Other people's reactions, also one with the same emoji as the bot's old
// lamp, are not removed: only the bot's own reaction is.
func TestSetStatusLeavesOthersReactions(t *testing.T) {
	ref, get := lampMessage(t)
	m := payload(t, "lamps/rest-lamp-get-working.json")
	rs := m["reactions"].([]any)
	rs[0].(map[string]any)["count"] = 2.0 // a person used the same emoji
	rs = append(rs,
		map[string]any{"emoji": map[string]any{"id": nil, "name": "\U0001f44d"}, "count": 1.0, "me": false},
		map[string]any{"emoji": map[string]any{"id": nil, "name": "❌"}, "count": 1.0, "me": false})
	m["reactions"] = rs
	r := newFakeREST(t)
	r.serveJSON(t, "GET "+get, 200, m)
	r.serve(t, "PUT "+reaction(ref, "✅"), "lamps/rest-lamp-add-done.json")
	r.serve(t, "DELETE "+reaction(ref, "⏳"), "lamps/rest-lamp-remove-working.json")
	if err := testDriver(t, botID(t), r, nil).SetStatus(context.Background(), ref, surface.Done); err != nil {
		t.Fatal(err)
	}
	want := []string{"PUT " + reaction(ref, "✅"), "DELETE " + reaction(ref, "⏳")}
	if got := changed(r); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
}

// Setting the lamp the message already has changes nothing.
func TestSetStatusSameLamp(t *testing.T) {
	ref, get := lampMessage(t)
	r := newFakeREST(t)
	r.serve(t, "GET "+get, "lamps/rest-lamp-get-working.json")
	if err := testDriver(t, botID(t), r, nil).SetStatus(context.Background(), ref, surface.Working); err != nil {
		t.Fatal(err)
	}
	if got := changed(r); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

// A refused SetStatus returns an Error that is not temporary, after one try.
func TestSetStatusRefused(t *testing.T) {
	c := capture(t, "lamps/rest-lamp-add-unknown-message.json")
	p := c["request"].(map[string]any)["path"].(string) // .../messages/M/reactions/<escaped>/@me
	parts := strings.Split(p, "/")
	ref := surface.MessageRef{Conv: surface.Conversation{Channel: parts[2]}, ID: parts[4]}
	r := newFakeREST(t)
	r.serveJSON(t, "GET /channels/"+parts[2]+"/messages/"+parts[4], 200, map[string]any{"id": parts[4]})
	r.serve(t, "PUT "+reaction(ref, "⏳"), "lamps/rest-lamp-add-unknown-message.json")
	err := testDriver(t, botID(t), r, nil).SetStatus(context.Background(), ref, surface.Working)
	var e *Error
	if !errors.As(err, &e) || e.Temporary() || e.Status != 404 {
		t.Fatalf("%v", err)
	}
	if n := r.called("PUT "); n != 1 {
		t.Fatalf("tried %d times", n)
	}
}

func TestLimitsSetStatus(t *testing.T) {
	if !testDriver(t, botID(t), newFakeREST(t), nil).Limits().SetStatus {
		t.Fatal("Limits does not report SetStatus")
	}
}

// A post with Notify mentions exactly those ids and allows no other mention,
// as the capture that pinged the bot itself; the service then reports only
// that id as mentioned.
func TestNotifyMentionsOnlyThose(t *testing.T) {
	c := capture(t, "lamps/rest-post-notify.json")
	sent := c["request"].(map[string]any)["body"].(map[string]any)
	who := sent["allowed_mentions"].(map[string]any)["users"].([]any)[0].(string)
	r := newFakeREST(t)
	conv := surface.Conversation{Channel: id(payload(t, "lamps/rest-post-notify.json")["channel_id"])}
	key := "POST /channels/" + conv.Channel + "/messages"
	r.serve(t, key, "lamps/rest-post-notify.json")
	if _, err := testDriver(t, botID(t), r, nil).Post(context.Background(), conv, "", surface.Message{Text: "@everyone hello", Notify: []string{who}}); err != nil {
		t.Fatal(err)
	}
	b, _ := content(t, r.requests(key)[0])
	if got := allowed(b); !reflect.DeepEqual(got, []string{"user:" + who}) {
		t.Fatalf("allowed %v (%v)", got, b["allowed_mentions"])
	}
	if !strings.HasPrefix(b["content"].(string), "<@"+who+">") {
		t.Fatalf("content %q", b["content"])
	}
	got := payload(t, "lamps/rest-post-notify.json")
	if ms := got["mentions"].([]any); len(ms) != 1 || ms[0].(map[string]any)["id"] != who || got["mention_everyone"] != false {
		t.Fatalf("the service reports %v", got["mentions"])
	}
}

// Text with @everyone and a role mention, posted without Notify, allows no
// mention, and the service reports that nobody was mentioned.
func TestEveryoneNotifiesNobody(t *testing.T) {
	c := capture(t, "lamps/rest-post-everyone.json")
	text := c["request"].(map[string]any)["body"].(map[string]any)["content"].(string)
	conv := surface.Conversation{Channel: id(payload(t, "lamps/rest-post-everyone.json")["channel_id"])}
	key := "POST /channels/" + conv.Channel + "/messages"
	r := newFakeREST(t)
	r.serve(t, key, "lamps/rest-post-everyone.json")
	if _, err := testDriver(t, botID(t), r, nil).Post(context.Background(), conv, "", surface.Message{Text: text}); err != nil {
		t.Fatal(err)
	}
	b, _ := content(t, r.requests(key)[0])
	if got := allowed(b); len(got) != 0 {
		t.Fatalf("allowed %v (%v)", got, b["allowed_mentions"])
	}
	got := payload(t, "lamps/rest-post-everyone.json")
	if got["mention_everyone"] != false || len(got["mention_roles"].([]any)) != 0 || len(got["mentions"].([]any)) != 0 {
		t.Fatalf("the service reports %v %v %v", got["mention_everyone"], got["mention_roles"], got["mentions"])
	}
}

// Quiet posts with the flag that suppresses notifications, as the capture.
func TestQuietPost(t *testing.T) {
	conv := surface.Conversation{Channel: id(payload(t, "lamps/rest-post-quiet.json")["channel_id"])}
	key := "POST /channels/" + conv.Channel + "/messages"
	r := newFakeREST(t)
	r.serve(t, key, "lamps/rest-post-quiet.json")
	if _, err := testDriver(t, botID(t), r, nil).Post(context.Background(), conv, "", surface.Message{Text: "x", Quiet: true}); err != nil {
		t.Fatal(err)
	}
	b, _ := content(t, r.requests(key)[0])
	if b["flags"] != 4096.0 || payload(t, "lamps/rest-post-quiet.json")["flags"] != 4096.0 {
		t.Fatalf("flags %v", b["flags"])
	}
}

// allowed lists what a post's allowed_mentions lets notify: each parse type,
// each user and role id, and a reply's author.
func allowed(body map[string]any) []string {
	am, ok := body["allowed_mentions"].(map[string]any)
	if !ok {
		return []string{"everything (no allowed_mentions)"}
	}
	var out []string
	if p, ok := am["parse"].([]any); !ok {
		out = append(out, "everything (no parse)")
	} else {
		for _, x := range p {
			out = append(out, "parse:"+x.(string))
		}
	}
	for _, k := range []string{"users", "roles"} {
		if l, ok := am[k].([]any); ok {
			for _, x := range l {
				out = append(out, strings.TrimSuffix(k, "s")+":"+x.(string))
			}
		}
	}
	if am["replied_user"] == true {
		out = append(out, "replied_user")
	}
	return out
}

// Two lamps set at once on one message leave one lamp: the service here
// keeps the bot's reactions and is slow to read them, so that two unordered
// changes would both read the old lamp and leave both new ones.
func TestSetStatusConcurrent(t *testing.T) {
	ref, _ := lampMessage(t)
	var mu sync.Mutex
	mine := map[string]bool{"⏳": true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			mu.Lock()
			var rs []map[string]any
			for e := range mine {
				rs = append(rs, map[string]any{"me": true, "count": 1, "emoji": map[string]any{"id": nil, "name": e}})
			}
			mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"id": ref.ID, "reactions": rs})
			return
		}
		e := strings.Split(r.URL.Path, "/")[6]
		mu.Lock()
		mine[e] = r.Method == "PUT"
		if r.Method == "DELETE" {
			delete(mine, e)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	d := newDriver(botID(t), "test-token", srv.URL, "ws://127.0.0.1:1", nil)
	d.identity.passed.Store(true)
	var wg sync.WaitGroup
	for _, s := range []surface.Status{surface.Done, surface.Failed} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.SetStatus(context.Background(), ref, s); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(mine) != 1 {
		t.Fatalf("the bot's reactions %v, want one lamp", mine)
	}
}

// A long post pings only in its first part, where the mentions are; a later
// part that names the same person pings nobody.
func TestNotifyOnlyFirstPart(t *testing.T) {
	who := capture(t, "lamps/rest-post-notify.json")["request"].(map[string]any)["body"].(map[string]any)["allowed_mentions"].(map[string]any)["users"].([]any)[0].(string)
	conv := surface.Conversation{Channel: id(payload(t, "lamps/rest-post-notify.json")["channel_id"])}
	key := "POST /channels/" + conv.Channel + "/messages"
	r := newFakeREST(t)
	r.serve(t, key, "lamps/rest-post-notify.json")
	text := strings.Repeat("a", maxText) + " <@" + who + ">"
	if _, err := testDriver(t, botID(t), r, nil).Post(context.Background(), conv, "", surface.Message{Text: text, Notify: []string{who}}); err != nil {
		t.Fatal(err)
	}
	reqs := r.requests(key)
	if len(reqs) != 2 {
		t.Fatalf("%d parts", len(reqs))
	}
	b1, _ := content(t, reqs[0])
	b2, _ := content(t, reqs[1])
	if !reflect.DeepEqual(allowed(b1), []string{"user:" + who}) || len(allowed(b2)) != 0 {
		t.Fatalf("allowed %v, then %v", allowed(b1), allowed(b2))
	}
}
