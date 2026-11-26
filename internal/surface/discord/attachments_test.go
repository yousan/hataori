package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/surface"
)

func TestCapturedMessageAttachments(t *testing.T) {
	d := testDriver(t, 1, newFakeREST(t), nil)
	s := newSink()
	deliver(t, d, s, "bot/gw-05-THREAD_CREATE.json")
	deliver(t, d, s, "bot/gw-12-MESSAGE_CREATE.json")
	in := s.messages[len(s.messages)-1]
	listed := payload(t, "bot/gw-12-MESSAGE_CREATE.json")["attachments"].([]any)[0].(map[string]any)
	if len(in.Files) != 1 || in.Files[0].Name != listed["filename"] || in.Files[0].URL != listed["url"] || in.Files[0].Path != "" {
		t.Fatalf("attachments: %+v", in.Files)
	}
	// The decoder maps only service-listed attachments. Existing captures have
	// no files for these posts, including ones with references and rich fields.
	for _, f := range []string{"person/gw-08-MESSAGE_CREATE.json", "person/gw-11-MESSAGE_CREATE.json"} {
		var m wireMessage
		if err := json.Unmarshal(raw(t, payload(t, f)), &m); err != nil {
			t.Fatal(err)
		}
		got := d.inbound(m, surface.Conversation{Channel: "test"}, false)
		if len(got.Files) != 0 {
			t.Fatal("fetched a non-attachment")
		}
	}
}

type attachmentTransport func(*http.Request) (*http.Response, error)

func (f attachmentTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestAttachmentHostAndRedirectBoundary(t *testing.T) {
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	calls := []string{}
	http.DefaultTransport = attachmentTransport(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.String())
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Fatal("credential sent to CDN")
		}
		status := http.StatusOK
		h := http.Header{}
		if strings.Contains(r.URL.Path, "redirect") {
			status = http.StatusFound
			h.Set("Location", "https://example.invalid/private")
		}
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader("hello")), Request: r}, nil
	})
	d := &Driver{}
	for _, url := range []string{"http://cdn.discordapp.com/attachments/x", "https://cdn.discordapp.com.evil.invalid/attachments/x", "https://example.invalid/x", "https://cdn.discordapp.com:443/attachments/x", "https://person@cdn.discordapp.com/attachments/x", "https://cdn.discordapp.com/preview/x", "https://cdn.discordapp.com/attachments/x#fragment"} {
		if body, err := d.OpenAttachment(context.Background(), url); err == nil {
			body.Close()
			t.Fatalf("accepted %s", url)
		}
	}
	if len(calls) != 0 {
		t.Fatal("untrusted host reached transport")
	}
	for _, host := range []string{"cdn.discordapp.com", "media.discordapp.net"} {
		body, err := d.OpenAttachment(context.Background(), "https://"+host+"/attachments/message/file?signature=example")
		if err != nil {
			t.Fatal(err)
		}
		body.Close()
	}
	if body, err := d.OpenAttachment(context.Background(), "https://cdn.discordapp.com/attachments/redirect"); err == nil {
		body.Close()
		t.Fatal("accepted redirect")
	}
	if len(calls) != 3 {
		t.Fatalf("redirect target requested: %v", calls)
	}
}
