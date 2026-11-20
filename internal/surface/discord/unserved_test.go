package discord

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/chat"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
)

// logLines is a log that keeps its lines.
type logLines struct{ bytes.Buffer }

func (l *logLines) lines() []string { return strings.Split(strings.TrimSpace(l.String()), "\n") }

// The captured command used in a thread reaches a chat front end with no
// scope in that thread's channel, as it reaches an instance that does not
// serve the channel when several share one server. The front end answers
// it once: the private acknowledgement of the capture is replaced with one
// line saying the instance does not serve the channel, nothing else is
// posted, and one log line is written, also when the command is delivered
// again.
func TestCommandOutsideTheScopesAgainstTheCapture(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := newFakeREST(t)
	d := testDriver(t, botID(t), r, nil)
	log := &logLines{}
	lg := slog.New(slog.NewTextHandler(log, nil))
	fe := &chat.FrontEnd{Name: Name, Driver: d, Trigger: "!go", Log: lg,
		Outbox: &chat.Outbox{Store: st, Client: Name, Driver: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Retry: 5 * time.Millisecond},
		Scopes: map[string]chat.Scope{"1000000000000000999": {Enabled: true}}}
	// The captured command stands for the front end's /new, the command
	// that answered nothing from a channel outside the scopes, and is
	// acknowledged as the front end asks for it: privately.
	d.commands = map[string]surface.CommandSpec{"hataori-test": fe.Commands()[0]}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { fe.Outbox.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	in := payload(t, "commands/gw-02-INTERACTION_CREATE.json")
	r.serve(t, ackKey(in), "commands/rest-command-ack-thread.json")
	r.serve(t, "PATCH "+answerKey(in), "commands/rest-command-answer-thread.json")
	for range 2 { // delivered again, as after a resumed session
		if err := d.dispatch(context.Background(), "INTERACTION_CREATE", bytes.NewReader(raw(t, in)), fe); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := content(t, r.requests(ackKey(in))[0])
	if want := capture(t, "commands/rest-command-ack-thread.json")["request"].(map[string]any)["body"]; !reflect.DeepEqual(b, want) {
		t.Fatalf("acknowledged with %v, want the private acknowledgement %v", b, want)
	}
	for deadline := time.Now().Add(5 * time.Second); r.called("PATCH "+answerKey(in)) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("not answered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // time for a second answer, if there were one
	if n := r.called("PATCH " + answerKey(in)); n != 1 {
		t.Fatalf("answered %d times", n)
	}
	b, _ = content(t, r.requests("PATCH " + answerKey(in))[0])
	if text, _ := b["content"].(string); !strings.Contains(text, "does not serve this channel") || strings.Contains(text, "\n") {
		t.Fatalf("answered with %v, want one line saying the instance does not serve the channel", b)
	}
	if n := r.called("POST /channels/") + r.called("POST /webhooks/"); n != 0 {
		t.Fatalf("%d more posts", n)
	}
	if l := log.lines(); len(l) != 1 || !strings.Contains(l[0], "does not serve") || !strings.Contains(l[0], "id="+id(in["id"])) {
		t.Fatalf("log %q, want one line", l)
	}
}
