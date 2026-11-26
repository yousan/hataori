package discord

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/yousan/hataori/internal/surface"
)

// Use the real captured public thread response, including its parent and
// type. A channel response or a wrong parent cannot establish public access.
func TestThreadPrivacyUsesCapturedChannel(t *testing.T) {
	for _, file := range []string{"bot/rest-thread-from-message.json", "third/rest-get-channel.json"} {
		t.Run(file, func(t *testing.T) {
			r := newFakeREST(t)
			p := payload(t, file)
			r.serveJSON(t, "GET /channels/"+id(p["id"]), 200, p)
			d := testDriver(t, botID(t), r, nil)
			conv := surface.Conversation{Thread: id(p["id"])}
			if parent, ok := p["parent_id"].(string); ok {
				conv.Channel = parent
			}
			if file == "third/rest-get-channel.json" {
				conv.Channel = "not-the-parent"
			}
			private, err := d.PrivateThread(context.Background(), conv)
			if err != nil || private != (file == "third/rest-get-channel.json") {
				t.Fatalf("private: %v, %v", private, err)
			}
			conv.Channel = "a different parent"
			if private, err := d.PrivateThread(context.Background(), conv); err != nil || !private {
				t.Fatalf("mismatch: %v, %v", private, err)
			}
		})
	}
}

// These real automated posts carry their own account IDs and content.
// In particular the webhook capture has an empty mentions array: literal
// addressing is used only for a refusal log, never to set Call.
func TestRejectedAutomatedCallsFromCaptures(t *testing.T) {
	for _, file := range []string{"calls/rest-post-by-bot.json", "calls/rest-webhook.json"} {
		t.Run(file, func(t *testing.T) {
			p := payload(t, file)
			// The recipient is the first literal user mention in the capture.
			mentions := userMentions(p["content"].(string))
			if len(mentions) == 0 {
				t.Fatal("capture has no literal user mention")
			}
			d := testDriver(t, mentions[0].id, newFakeREST(t), nil)
			var m wireMessage
			if err := json.Unmarshal(raw(t, p), &m); err != nil {
				t.Fatal(err)
			}
			var in surface.Inbound
			d.call(m, &in)
			if in.Call || !in.RejectedCall {
				t.Fatalf("call=%v rejected=%v", in.Call, in.RejectedCall)
			}
		})
	}
}
