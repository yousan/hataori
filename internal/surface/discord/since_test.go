package discord

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/surface"
)

func body(t *testing.T, name string) []any {
	return capture(t, name)["body"].([]any)
}

func after(t *testing.T, name string) string {
	path := capture(t, name)["request"].(map[string]any)["path"].(string)
	_, q, _ := strings.Cut(path, "?")
	v, _ := url.ParseQuery(q)
	return v.Get("after")
}

func ids(in []surface.Inbound) []string {
	var out []string
	for _, m := range in {
		out = append(out, m.Ref.ID)
	}
	return out
}

func TestSinceAcrossPages(t *testing.T) {
	r := newFakeREST(t)
	page1 := capture(t, "unbroken/rest-history-page-1.json")
	th := strings.Split(page1["request"].(map[string]any)["path"].(string), "/")[2]
	key := "GET /channels/" + th + "/messages"
	for _, p := range []string{"1", "2", "3"} {
		r.serve(t, key, "unbroken/rest-history-page-"+p+".json")
	}
	d := testDriver(t, 1, r, nil) // not the captures' bot, so its messages count
	d.pageSize = 2                // the size the pages were captured with
	conv := surface.Conversation{Channel: "1", Thread: th}
	got, err := d.Since(context.Background(), conv, after(t, "unbroken/rest-history-page-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, p := range []string{"1", "2", "3"} {
		for _, m := range body(t, "unbroken/rest-history-page-"+p+".json") {
			want = append(want, id(m.(map[string]any)["id"]))
		}
	}
	slices.Sort(want) // oldest first; the pages came newest first
	if !slices.Equal(ids(got), want) {
		t.Fatalf("got %v, want %v", ids(got), want)
	}
	for _, m := range got {
		if m.Cursor != m.Ref.ID || m.Ref.Conv != conv || m.Direct {
			t.Errorf("%+v", m)
		}
	}
	// Each page starts after the newest message of the one before, as the
	// pages were captured.
	q := r.requests(key)
	if len(q) != 3 { // the third page is short: no fourth request
		t.Fatalf("%d requests", len(q))
	}
	for i, p := range []string{"1", "2", "3"} {
		v, _ := url.ParseQuery(q[i].query)
		if v.Get("after") != after(t, "unbroken/rest-history-page-"+p+".json") {
			t.Errorf("page %s asked after %s", p, v.Get("after"))
		}
	}
}

// Webhook posts are included, the bot's own messages are not, and a direct
// conversation is marked as one.
func TestSinceWebhookOwnAndDirect(t *testing.T) {
	r := newFakeREST(t)
	hist := capture(t, "person/rest-history-channel.json")
	ch := strings.Split(hist["request"].(map[string]any)["path"].(string), "/")[2]
	r.serve(t, "GET /channels/"+ch, "third/rest-get-channel.json")
	r.serve(t, "GET /channels/"+ch+"/messages", "person/rest-history-channel.json")
	dm := capture(t, "person/rest-history-direct.json")
	dmc := strings.Split(dm["request"].(map[string]any)["path"].(string), "/")[2]
	r.serve(t, "GET /channels/"+dmc, "third/rest-get-direct-channel.json")
	r.serve(t, "GET /channels/"+dmc+"/messages", "person/rest-history-direct.json")
	d := testDriver(t, botID(t), r, nil)

	got, err := d.Since(context.Background(), surface.Conversation{Channel: ch}, "")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []surface.AuthorKind
	for _, m := range got {
		if m.Author.ID == botID(t).String() {
			t.Errorf("the bot's own message %s was returned", m.Ref.ID)
		}
		if m.Direct {
			t.Errorf("%s marked direct", m.Ref.ID)
		}
		kinds = append(kinds, m.Author.Kind)
	}
	if !slices.Contains(kinds, surface.Webhook) || !slices.Contains(kinds, surface.Person) || len(got) != 3 {
		t.Fatalf("kinds %v (%d messages)", kinds, len(got))
	}

	got, err = d.Since(context.Background(), surface.Conversation{Channel: dmc}, "")
	if err != nil || len(got) != 1 || !got[0].Direct || got[0].Author.Kind != surface.Person {
		t.Fatalf("direct: %+v %v", got, err)
	}
	if n := r.called("GET /channels/" + dmc); n != 2 { // the lookup, then the page
		t.Fatalf("%d requests", n)
	}
}

func TestSinceCursorErrors(t *testing.T) {
	r := newFakeREST(t)
	d := testDriver(t, botID(t), r, nil)
	_, err := d.Since(context.Background(), surface.Conversation{Channel: "1", Thread: "2"}, "not-a-snowflake")
	if !errors.Is(err, ErrCursor) || r.called("GET") != 0 {
		t.Fatalf("a cursor that is no id: %v", err)
	}

	// Nothing after the cursor (the last page) is no error.
	last := capture(t, "bot/rest-history-page-3.json")
	r.serve(t, "GET "+strings.Split(last["request"].(map[string]any)["path"].(string), "?")[0], "bot/rest-history-page-3.json")
	th := strings.Split(last["request"].(map[string]any)["path"].(string), "/")[2]
	r.serve(t, "GET /channels/"+th+"/messages/"+after(t, "bot/rest-history-page-3.json"), "bot/rest-post-thread-4.json")
	got, err := d.Since(context.Background(), surface.Conversation{Channel: "1", Thread: th}, after(t, "bot/rest-history-page-3.json"))
	if err != nil || len(got) != 0 {
		t.Fatalf("no messages: %v %v", got, err)
	}
	// The service refusing the cursor (the capture) is a cursor error too.
	bad := capture(t, "bot/rest-history-bad-cursor.json")
	r.serve(t, "GET "+strings.Split(bad["request"].(map[string]any)["path"].(string), "?")[0], "bot/rest-history-bad-cursor.json")
	_, err = d.Since(context.Background(), surface.Conversation{Channel: "1", Thread: th}, "1000000000000000002")
	if !errors.Is(err, ErrCursor) {
		t.Fatalf("a refused cursor: %v", err)
	}
}

// A conversation that no longer exists (the capture: 404 Unknown Channel,
// code 10003) is gone, not a cursor the service cannot serve: when reading
// the history, and when looking a channel up first.
func TestSinceUnknownChannelIsGone(t *testing.T) {
	const name = "bot/rest-history-unknown-channel.json"
	ch := strings.Split(capture(t, name)["request"].(map[string]any)["path"].(string), "/")[2]
	for _, c := range []struct {
		conv  surface.Conversation
		route string
	}{
		{surface.Conversation{Channel: "1000000000000000002", Thread: ch}, "GET /channels/" + ch + "/messages"},
		{surface.Conversation{Channel: ch}, "GET /channels/" + ch},
	} {
		r := newFakeREST(t)
		r.serve(t, c.route, name)
		_, err := testDriver(t, botID(t), r, nil).Since(context.Background(), c.conv, "1000000000000000001")
		var e *Error
		if !errors.Is(err, surface.ErrGone) || errors.Is(err, ErrCursor) || !errors.As(err, &e) || e.Code != 10003 {
			t.Errorf("%s: %v, want surface.ErrGone and not ErrCursor", c.route, err)
		}
	}
}

// Without Read Message History the service answers an empty page; reading
// the cursor's message then fails, and that is a cursor error. A cursor
// whose message is gone, or readable, is just "no messages".
func TestSinceTellsUnreadableFromEmpty(t *testing.T) {
	last := capture(t, "unbroken/rest-history-page-4.json") // the empty page
	th := strings.Split(last["request"].(map[string]any)["path"].(string), "/")[2]
	cursor := after(t, "unbroken/rest-history-page-4.json")
	conv := surface.Conversation{Channel: "1", Thread: th}
	for _, c := range []struct {
		reply string // the answer to reading the cursor's message
		gone  bool
	}{
		// 403 refusal; the capture is of a refused thread deletion, the
		// same shape as a refused read.
		{"second/rest-delete-thread-refused.json", true},
		{"bot/rest-delete-again.json", false}, // 404 Unknown Message
		{"bot/rest-post-thread-1.json", false},
	} {
		r := newFakeREST(t)
		r.serve(t, "GET /channels/"+th+"/messages", "unbroken/rest-history-page-4.json")
		r.serve(t, "GET /channels/"+th+"/messages/"+cursor, c.reply)
		got, err := testDriver(t, botID(t), r, nil).Since(context.Background(), conv, cursor)
		if errors.Is(err, ErrCursor) != c.gone || (!c.gone && (err != nil || len(got) != 0)) {
			t.Errorf("%s: %v %v", c.reply, got, err)
		}
	}
}
