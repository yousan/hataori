package discord

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"testing"

	"github.com/yousan/hataori/internal/surface"
)

// These are the original response envelopes, including their rate headers,
// rather than new pages assembled from individually captured messages.
func TestHistoryReplaysConsecutiveAfterCaptures(t *testing.T) {
	thread := payload(t, "unbroken/rest-thread-for-pages.json")
	conv := surface.Conversation{Channel: id(thread["parent_id"]), Thread: id(thread["id"])}
	r := newFakeREST(t)
	d := testDriver(t, 1, r, nil)
	r.serve(t, "GET /channels/"+conv.Thread, "unbroken/rest-thread-for-pages.json")
	key := "GET /channels/" + conv.Thread + "/messages"
	for _, n := range []string{"1", "2", "3", "4"} {
		r.serve(t, key, "unbroken/rest-history-page-"+n+".json")
	}
	cursor := after(t, "unbroken/rest-history-page-1.json")
	var got, want []string
	for _, n := range []string{"1", "2", "3"} {
		name := "unbroken/rest-history-page-" + n + ".json"
		if cursor != after(t, name) {
			t.Fatalf("page %s: continuation %q differs from captured request", n, cursor)
		}
		page, err := d.ReadMessages(context.Background(), conv, surface.MessageQuery{After: cursor, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range page.Messages {
			got = append(got, m.Ref.ID)
			if m.Ref.Conv != conv || m.Type != surface.PublicThread || m.Author.Kind != surface.Bot || m.Own {
				t.Fatalf("lost conversation/author metadata: %+v", m)
			}
		}
		for _, m := range body(t, name) {
			want = append(want, id(m.(map[string]any)["id"]))
		}
		cursor = page.After
		if n == "3" && (page.Before != "" || cursor != "") {
			t.Fatalf("short captured page did not end traversal: %+v", page)
		}
	}
	slices.Sort(want)
	if !slices.Equal(got, want) || len(got) != 5 {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, request := range r.requests(key) {
		q, _ := url.ParseQuery(request.query)
		if q.Get("after") != after(t, "unbroken/rest-history-page-"+[]string{"1", "2", "3"}[i]+".json") || q.Get("limit") != "2" {
			t.Fatalf("request differs from captured traversal: %v", q)
		}
	}
	// The captured fourth response is empty. Without a successful permission
	// probe it must be an error, even after earlier pages were readable.
	_, err := d.ReadMessages(context.Background(), conv, surface.MessageQuery{After: after(t, "unbroken/rest-history-page-4.json"), Limit: 2})
	if err == nil {
		t.Fatal("empty captured page accepted without a permission proof")
	}
}

func TestHistoryReplaysLatestCaptures(t *testing.T) {
	for _, tc := range []struct {
		lookup, page string
		count        int
	}{
		{"third/rest-get-channel.json", "person/rest-history-channel.json", 2},
		{"third/rest-get-direct-channel.json", "person/rest-history-direct.json", 2},
	} {
		t.Run(tc.page, func(t *testing.T) {
			channel := payload(t, tc.lookup)
			conv := surface.Conversation{Channel: id(channel["id"])}
			r := newFakeREST(t)
			d := testDriver(t, botID(t), r, nil)
			r.serve(t, "GET /channels/"+conv.Channel, tc.lookup)
			key := "GET /channels/" + conv.Channel + "/messages"
			r.serve(t, key, tc.page)
			got, err := d.ReadMessages(context.Background(), conv, surface.MessageQuery{Limit: 10})
			if err != nil || len(got.Messages) != tc.count || got.Before != "" || got.After != "" {
				t.Fatalf("latest: %+v %v", got, err)
			}
			var ids []string
			for _, m := range got.Messages {
				ids = append(ids, m.Ref.ID)
				if m.Ref.Conv != conv || m.Author.ID == "" || m.Own != (m.Author.ID == botID(t).String()) {
					t.Fatalf("lost metadata: %+v", m)
				}
			}
			if !slices.IsSorted(ids) {
				t.Fatalf("latest not chronological: %v", ids)
			}
			q, _ := url.ParseQuery(r.requests(key)[0].query)
			if q.Get("limit") != "10" || q.Get("before") != "" || q.Get("after") != "" || r.called("") != 2 {
				t.Fatalf("unexpected requests: %v", q)
			}
		})
	}
}

func TestHistoryRefusesPrivateAndUnknownBeforeReading(t *testing.T) {
	for _, typ := range []int{12, 99} {
		for _, one := range []bool{false, true} {
			// These deliberate channel-type mutations test refusal only. They
			// do not supply the missing real private-thread capture evidence.
			thread := payload(t, "unbroken/rest-thread-for-pages.json")
			thread["type"] = typ
			conv := surface.Conversation{Channel: id(thread["parent_id"]), Thread: id(thread["id"])}
			if typ == 99 {
				conv = surface.Conversation{Channel: conv.Thread}
			}
			r := newFakeREST(t)
			d := testDriver(t, 1, r, nil)
			r.serveJSON(t, "GET /channels/"+id(thread["id"]), 200, thread)
			var err error
			if one {
				_, err = d.ReadMessage(context.Background(), surface.MessageRef{Conv: conv, ID: "1000000000000000043"})
			} else {
				_, err = d.ReadMessages(context.Background(), conv, surface.MessageQuery{Limit: 2})
			}
			if !errors.Is(err, surface.ErrRefused) || r.called("") != 1 {
				t.Fatalf("type %d single %v: refusal must precede message GET; err=%v requests=%d", typ, one, err, r.called(""))
			}
		}
	}
}
