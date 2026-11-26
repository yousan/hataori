package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/attachments"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

type attachmentDriver struct {
	*fake.Driver
	data  []byte
	calls int
}

func (d *attachmentDriver) OpenAttachment(context.Context, string) (io.ReadCloser, error) {
	d.calls++
	return io.NopCloser(bytes.NewReader(d.data)), nil
}
func TestAuthorizedWorkerAttachments(t *testing.T) {
	f := newFront(t, workspaces{})
	var imageData bytes.Buffer
	png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	d := &attachmentDriver{Driver: f.driver, data: imageData.Bytes()}
	f.fe.Driver = d
	f.core.Attachments = &attachments.Manager{Root: t.TempDir(), Store: f.core.Store, Driver: func(string) surface.Driver { return d }}
	ctx := context.Background()
	// A trigger's attachments reach the first input and survive in the stored
	// first input before any agent process is ready to take it.
	trigger := f.driver.Inject(chanConv, fake.Person(owner), "!go repo:https://example.com/r.git examine")
	trigger.Files = []surface.File{{Name: "photo.png", URL: "https://cdn.example.invalid/photo"}}
	if err := f.fe.handle(trigger); err != nil {
		t.Fatal(err)
	}
	starts := f.runner.all()
	if len(starts) != 1 || len(starts[0].Input.Files) != 1 {
		t.Fatalf("first input %+v", starts)
	}
	file := starts[0].Input.Files[0]
	got, err := os.ReadFile(file.Path)
	if err != nil || !bytes.Equal(got, imageData.Bytes()) {
		t.Fatal("first file missing or differs")
	}
	var stored string
	if err := f.core.Store.DB.QueryRow(`SELECT first_input FROM sessions WHERE id=?`, starts[0].Session).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored, file.Path) || strings.Contains(stored, "cdn.example") {
		t.Fatalf("first input not durable or retains URL after copy: %s", stored)
	}
	sid := starts[0].Session
	if err := f.core.Machine.Apply(ctx, sid, session.Input{Kind: session.AgentEvent, Agent: agent.Event{Kind: agent.SessionReady, SessionID: "fake", Seq: 1}}); err != nil {
		t.Fatal(err)
	}
	conv := f.threads()[0].Opened
	n := d.calls
	for i, author := range []surface.Author{fake.Person(outsider), fake.Bot("other"), fake.Webhook(hook)} {
		in := surface.Inbound{Ref: surface.MessageRef{Conv: conv, ID: []string{"guest-file", "bot-file", "hook-file"}[i]}, Author: author, Text: "check", Files: trigger.Files}
		if err := f.fe.followUp(in); err != nil {
			t.Fatal(err)
		}
	}
	if d.calls != n {
		t.Fatal("guest, bot or webhook attachment requested at transport")
	}
	// No text is necessary for input. The path is in the agent's message, and
	// the input event reports it without a signed attachment URL.
	in := surface.Inbound{Ref: surface.MessageRef{Conv: conv, ID: "picture-only"}, Author: fake.Person(owner), Files: trigger.Files}
	if err := f.fe.followUp(in); err != nil {
		t.Fatal(err)
	}
	calls := f.runner.handed()
	if !strings.Contains(calls[len(calls)-1], "photo.png") {
		t.Fatalf("missing agent file line: %v", calls)
	}
	var event string
	if err := f.core.Store.DB.QueryRow(`SELECT data FROM events WHERE kind='input' ORDER BY pos DESC LIMIT 1`).Scan(&event); err != nil {
		t.Fatal(err)
	}
	var e struct {
		Files []control.InputFile `json:"files"`
	}
	json.Unmarshal([]byte(event), &e)
	if len(e.Files) != 1 || e.Files[0].Path == "" || strings.Contains(event, "cdn.example") {
		t.Fatalf("event: %s", event)
	}
	b, err := os.ReadFile(e.Files[0].Path)
	if err != nil || !bytes.Equal(b, imageData.Bytes()) {
		t.Fatal("picture not copied")
	}
	if filepath.Base(e.Files[0].Path) == "photo.png" {
		t.Fatal("not a generated filename")
	}
	// Repeated delivery opens no new transport request.
	n = d.calls
	if err := f.fe.followUp(in); err != nil {
		t.Fatal(err)
	}
	if d.calls != n {
		t.Fatal("duplicate input fetched again")
	}
	// Confined sessions do not gain access before #891 and the marks land.
	if _, err := f.core.Store.DB.Exec(`UPDATE threads SET confined=1 WHERE id=?`, int64(starts[0].Thread)); err != nil {
		t.Fatal(err)
	}
	in.Ref.ID = "confined-file"
	if err := f.fe.followUp(in); err != nil {
		t.Fatal(err)
	}
	if d.calls != n {
		t.Fatal("confined attachment fetched")
	}
}
