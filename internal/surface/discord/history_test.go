package discord

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/disgoorg/disgo/rest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/surface"
)

func TestHistoryCapturedMessages(t *testing.T) {
	for _, name := range []string{
		"calls/rest-reply.json", "calls/rest-reply-silent.json", "calls/rest-forward.json",
		"commands/rest-command-answer-channel.json", "commands/rest-command-answer-thread.json",
		"person/rest-get-webhook-post.json", "bot/rest-post-attachment.json", "bot/rest-post-channel.json",
	} {
		t.Run(name, func(t *testing.T) {
			m := payload(t, name)
			var w historyWireMessage
			if err := json.Unmarshal(raw(t, m), &w); err != nil {
				t.Fatal(err)
			}
			r := newFakeREST(t)
			self := w.Author.ID
			if strings.Contains(name, "webhook") {
				self = botID(t)
			}
			d := testDriver(t, self, r, nil)
			conv := surface.Conversation{Channel: w.ChannelID.String()}
			got, err := d.historyMessage(w, conv, "")
			if err != nil {
				t.Fatal(err)
			}
			if got.Ref.ID != w.ID.String() || got.Ref.Conv != conv || got.Text != w.Content || got.Time != w.Timestamp {
				t.Fatalf("lost message fields: %+v", got)
			}
			if strings.Contains(name, "webhook") {
				if got.Author.Kind != surface.Webhook || got.Own {
					t.Fatalf("webhook: %+v", got)
				}
			} else if !got.Own {
				t.Fatalf("own post: %+v", got)
			}
			if w.Type == typeReply && (got.ReplyTo == nil || got.ReplyTo.ID != w.MessageReference.MessageID.String()) {
				t.Fatalf("reply: %+v", got)
			}
			if strings.Contains(name, "forward") && (len(got.Forwarded) != 1 || got.Forwarded[0].Text == "") {
				t.Fatalf("forward: %+v", got)
			}
			if strings.Contains(name, "attachment") {
				if len(got.Files) != 1 || got.Files[0].URL == "" || got.Files[0].Name == "" || got.Files[0].Size == 0 {
					t.Fatalf("files: %+v", got)
				}
			}
			if r.called("") != 0 {
				t.Fatal("metadata conversion made a request")
			}
		})
	}
}

func TestHistoryPagingCapturedMessages(t *testing.T) {
	thread := payload(t, "unbroken/rest-thread-for-pages.json")
	conv := surface.Conversation{Channel: id(thread["parent_id"]), Thread: id(thread["id"])}
	var messages []any
	for _, n := range []string{"1", "2", "3"} {
		messages = append(messages, body(t, "unbroken/rest-history-page-"+n+".json")...)
	}
	slices.SortFunc(messages, func(a, b any) int { return strings.Compare(id(a.(map[string]any)["id"]), id(b.(map[string]any)["id"])) })
	for _, direction := range []string{"before", "after", "last"} {
		t.Run(direction, func(t *testing.T) {
			r := newFakeREST(t)
			d := testDriver(t, 1, r, nil)
			r.serveJSON(t, "GET /channels/"+conv.Thread, 200, thread)
			key := "GET /channels/" + conv.Thread + "/messages"
			// Both directions replay captured message objects. These are composed
			// pages, not a claim to a capture of a before= response.
			page := slices.Clone(messages[:2])
			slices.Reverse(page)
			r.serveJSON(t, key, 200, page)
			q := surface.MessageQuery{Limit: 2}
			switch direction {
			case "before":
				q.Before = id(messages[2].(map[string]any)["id"])
			case "after":
				q.After = after(t, "unbroken/rest-history-page-1.json")
			}
			got, err := d.ReadMessages(context.Background(), conv, q)
			if err != nil {
				t.Fatal(err)
			}
			if got.Type != surface.PublicThread || len(got.Messages) != 2 || got.Messages[0].Ref.ID != id(messages[0].(map[string]any)["id"]) || got.Before == "" || got.After == "" {
				t.Fatalf("%+v", got)
			}
			v, _ := url.ParseQuery(r.requests(key)[0].query)
			if v.Get("before") != q.Before || v.Get("after") != q.After || v.Get("limit") != "2" {
				t.Fatal(v)
			}
		})
	}
}

func TestHistoryNoticeAdvancesAndSingleRead(t *testing.T) {
	notice := payload(t, "bot/gw-07-MESSAGE_CREATE.json")
	thread := payload(t, "bot/rest-thread-from-message.json")
	conv := surface.Conversation{Channel: id(thread["parent_id"]), Thread: id(thread["id"])}
	r := newFakeREST(t)
	d := testDriver(t, 1, r, nil)
	r.serveJSON(t, "GET /channels/"+conv.Thread, 200, thread)
	key := "GET /channels/" + conv.Thread + "/messages"
	r.serveJSON(t, key, 200, []any{notice})
	page, err := d.ReadMessages(context.Background(), conv, surface.MessageQuery{Limit: 1})
	if err != nil || len(page.Messages) != 0 || page.Before != id(notice["id"]) || page.After != id(notice["id"]) {
		t.Fatalf("%+v %v", page, err)
	}
	r.serveJSON(t, key+"/"+id(notice["id"]), 200, notice)
	if _, err := d.ReadMessage(context.Background(), surface.MessageRef{Conv: conv, ID: id(notice["id"])}); !errors.Is(err, surface.ErrRefused) {
		t.Fatal(err)
	}
	m := payload(t, "bot/rest-post-thread-1.json")
	r.serveJSON(t, key+"/"+id(m["id"]), 200, m)
	got, err := d.ReadMessage(context.Background(), surface.MessageRef{Conv: conv, ID: id(m["id"])})
	if err != nil || got.Ref.ID != id(m["id"]) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestHistoryReadErrorsAndEmpty(t *testing.T) {
	thread := payload(t, "unbroken/rest-thread-for-pages.json")
	conv := surface.Conversation{Channel: id(thread["parent_id"]), Thread: id(thread["id"])}
	key := "GET /channels/" + conv.Thread + "/messages"
	for _, name := range []string{"bot/rest-history-unknown-channel.json", "second/rest-delete-thread-refused.json", "bot/rest-history-bad-cursor.json", "second/rest-rate-limited.json"} {
		t.Run(name, func(t *testing.T) {
			r := newFakeREST(t)
			d := testDriver(t, 1, r, nil)
			r.serveJSON(t, "GET /channels/"+conv.Thread, 200, thread)
			r.serve(t, key, name)
			d.client, d.identity = newClient(1, "test-token", rest.WithURL(r.URL), rest.WithRateLimiterConfigOpts(rest.WithMaxRetries(0)))
			d.identity.passed.Store(true)
			_, err := d.ReadMessages(context.Background(), conv, surface.MessageQuery{Limit: 2})
			var service *Error
			want := capture(t, name)
			if !errors.As(err, &service) || service.Status != int(want["status"].(float64)) {
				t.Fatalf("lost service reason: %v", err)
			}
			if code, ok := want["body"].(map[string]any)["code"].(float64); ok && service.Code != int(code) {
				t.Fatalf("lost code: %v", err)
			}
		})
	}
	r := newFakeREST(t)
	d := testDriver(t, 1, r, nil)
	r.serveJSON(t, "GET /channels/"+conv.Thread, 200, thread)
	r.serveJSON(t, key, 200, []any{})
	if _, err := d.ReadMessages(context.Background(), conv, surface.MessageQuery{Limit: 2}); !errors.Is(err, surface.ErrRefused) {
		t.Fatalf("unverifiable empty history: %v", err)
	}
	cursor := after(t, "unbroken/rest-history-page-1.json")
	r.serve(t, key+"/"+cursor, "second/rest-delete-thread-refused.json")
	if _, err := d.ReadMessages(context.Background(), conv, surface.MessageQuery{After: cursor, Limit: 2}); err == nil {
		t.Fatal("missing history access became empty history")
	}
}

func TestHistoryListsCapturedThreadObjects(t *testing.T) {
	channel := payload(t, "third/rest-get-channel.json")
	active := payload(t, "bot/rest-thread-from-message.json")
	archived := payload(t, "second/rest-set-thread-archived.json")
	// Existing captures contain individual thread objects, not collection
	// envelopes. These composed responses exercise their conversion; real
	// active/archive collection and private-thread captures are still needed.
	for _, a := range []bool{false, true} {
		r := newFakeREST(t)
		d := testDriver(t, 1, r, nil)
		r.serveJSON(t, "GET /channels/"+id(channel["id"]), 200, channel)
		path := "GET /guilds/" + id(channel["guild_id"]) + "/threads/active"
		th := active
		if a {
			path = "GET /channels/" + id(channel["id"]) + "/threads/archived/public"
			th = archived
		}
		r.serveJSON(t, path, 200, map[string]any{"threads": []any{th}, "has_more": a})
		got, err := d.ListThreads(context.Background(), id(channel["id"]), surface.ThreadQuery{Archived: a, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Threads) != 1 || got.Threads[0].Type != surface.PublicThread || got.Threads[0].Archived != a {
			t.Fatalf("%+v", got)
		}
		if a && got.Next == "" {
			t.Fatalf("no archive cursor: %+v", got)
		}
	}
}

func TestHistoryOwnCommandResponses(t *testing.T) {
	for _, name := range []string{"commands/rest-command-answer-channel.json", "commands/rest-command-answer-thread.json"} {
		t.Run(name, func(t *testing.T) {
			m := payload(t, name)
			r := newFakeREST(t)
			var w historyWireMessage
			if err := json.Unmarshal(raw(t, m), &w); err != nil {
				t.Fatal(err)
			}
			d := testDriver(t, w.Author.ID, r, nil)
			conv := surface.Conversation{Channel: w.ChannelID.String()}
			// The channel lookup is only used for identity/type here. The message
			// under test is the unmodified captured application response.
			c := payload(t, "third/rest-get-channel.json")
			c["id"] = conv.Channel
			r.serveJSON(t, "GET /channels/"+conv.Channel, 200, c)
			path := "GET /channels/" + conv.Channel + "/messages"
			r.serveJSON(t, path, 200, []any{m})
			r.serveJSON(t, path+"/"+w.ID.String(), 200, m)
			got, err := d.ReadMessages(context.Background(), conv, surface.MessageQuery{Limit: 2})
			if err != nil || len(got.Messages) != 1 || !got.Messages[0].Own {
				t.Fatalf("%+v %v", got, err)
			}
			one, err := d.ReadMessage(context.Background(), surface.MessageRef{Conv: conv, ID: w.ID.String()})
			if err != nil || !one.Own {
				t.Fatalf("%+v %v", one, err)
			}
		})
	}
}

func TestHistoryEmptyPrefersValidCursor(t *testing.T) {
	thread := payload(t, "unbroken/rest-thread-for-pages.json")
	m := body(t, "unbroken/rest-history-page-1.json")[0].(map[string]any)
	thread["last_message_id"] = "1000000000000000999" // deleted final post; never fetched
	conv := surface.Conversation{Channel: id(thread["parent_id"]), Thread: id(thread["id"])}
	r := newFakeREST(t)
	d := testDriver(t, 1, r, nil)
	r.serveJSON(t, "GET /channels/"+conv.Thread, 200, thread)
	key := "GET /channels/" + conv.Thread + "/messages"
	r.serveJSON(t, key, 200, []any{})
	r.serveJSON(t, key+"/"+id(m["id"]), 200, m)
	got, err := d.ReadMessages(context.Background(), conv, surface.MessageQuery{After: id(m["id"]), Limit: 2})
	if err != nil || len(got.Messages) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	if r.called(key+"/1000000000000000999") != 0 {
		t.Fatal("probed stale last message")
	}
}

func TestHistoryThreadContinuationAndPrivateExclusion(t *testing.T) {
	channel := payload(t, "third/rest-get-channel.json")
	a := payload(t, "bot/rest-thread-from-message.json")
	b := payload(t, "second/rest-thread-without-message.json")
	private := payload(t, "bot/rest-thread-from-message.json")
	// A deliberate private-type variation supplements the captures. It does
	// not replace the missing real private-thread collection capture.
	private["type"] = 12
	private["id"] = "1000000000000000999"
	r := newFakeREST(t)
	d := testDriver(t, 1, r, nil)
	ch := id(channel["id"])
	r.serveJSON(t, "GET /channels/"+ch, 200, channel)
	key := "GET /guilds/" + id(channel["guild_id"]) + "/threads/active"
	r.serveJSON(t, key, 200, map[string]any{"threads": []any{a, private, b}})
	var got []string
	cursor := ""
	for i := 0; i < 3; i++ {
		page, err := d.ListThreads(context.Background(), ch, surface.ThreadQuery{Limit: 1, Before: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, th := range page.Threads {
			got = append(got, th.Conv.Thread)
		}
		cursor = page.Next
		if cursor == "" {
			break
		}
	}
	want := []string{id(a["id"]), id(b["id"])}
	slices.Sort(want)
	slices.Reverse(want)
	if !slices.Equal(got, want) || cursor != "" {
		t.Fatalf("got %v cursor %q want %v", got, cursor, want)
	}
	archived := payload(t, "second/rest-set-thread-archived.json")
	key = "GET /channels/" + ch + "/threads/archived/public"
	r.serveJSON(t, key, 200, map[string]any{"threads": []any{archived}, "has_more": true})
	r.serveJSON(t, key, 200, map[string]any{"threads": []any{}, "has_more": false})
	page, err := d.ListThreads(context.Background(), ch, surface.ThreadQuery{Archived: true, Limit: 1})
	if err != nil || page.Next == "" {
		t.Fatalf("%+v %v", page, err)
	}
	last, err := d.ListThreads(context.Background(), ch, surface.ThreadQuery{Archived: true, Limit: 1, Before: page.Next})
	if err != nil || last.Next != "" || len(last.Threads) != 0 {
		t.Fatalf("%+v %v", last, err)
	}
	query, _ := url.ParseQuery(r.requests(key)[1].query)
	if query.Get("before") != page.Next {
		t.Fatal(query)
	}
}
