package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/attachments"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

type fileDriver struct {
	*fake.Driver
	data  []byte
	calls int
}

func (d *fileDriver) OpenAttachment(context.Context, string) (io.ReadCloser, error) {
	d.calls++
	return io.NopCloser(bytes.NewReader(d.data)), nil
}

type stoppedCopies struct {
	manager *attachments.Manager
	after   bool
}

func (s stoppedCopies) Resolve(ctx context.Context, sid int64, client, key string, files []control.IncomingFile) ([]control.InputFile, error) {
	if s.after {
		if _, err := s.manager.Resolve(ctx, sid, client, key, files); err != nil {
			return nil, err
		}
	}
	panic("daemon stopped before file handoff")
}
func TestAttachmentFirstInputSurvivesRecovery(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "before copy", true: "after copy"}[after], func(t *testing.T) {
			f := newFixture(t, options{})
			var pic bytes.Buffer
			png.Encode(&pic, image.NewRGBA(image.Rect(0, 0, 2, 2)))
			d := &fileDriver{Driver: fake.New(), data: pic.Bytes()}
			manager := &attachments.Manager{Root: f.root, Store: f.st, Driver: func(string) surface.Driver { return d }}
			f.core.Attachments = stoppedCopies{manager: manager, after: after}
			func() {
				defer func() {
					if p := recover(); p != "daemon stopped before file handoff" {
						t.Fatalf("unexpected stop: %v", p)
					}
				}()
				_, err := owner(f.core).Start(context.Background(), control.StartRequest{Key: "files", Scope: "s", Thread: "t1", Repo: "file:///unused", Input: control.Input{Text: "reply examined", Attachments: []control.IncomingFile{{Name: "picture.png", URL: "service-attachment"}}}})
				t.Fatalf("start did not stop: %v", err)
			}()
			// New core, runner, store handle and manager: only the committed first
			// input and attachment ledger cross this daemon boundary.
			rec := &recorder{inputs: map[string][]string{}}
			st, c, r, err := build(f.root, options{wrap: func(a agent.Adapter) agent.Adapter { rec.Adapter = a; return rec }}, f.log)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { r.Close(); st.Close() })
			c.Attachments = &attachments.Manager{Root: f.root, Store: st, Driver: func(string) surface.Driver { return d }}
			if err = r.Reconnect(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err = c.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "recovered attachment turn", 10*time.Second, func() bool { return counts(t, st)["turn_ended"] == 1 })
			var data string
			if err = st.DB.QueryRow(`SELECT data FROM events WHERE kind='input' ORDER BY pos LIMIT 1`).Scan(&data); err != nil {
				t.Fatal(err)
			}
			var input struct {
				Files []control.InputFile `json:"files"`
			}
			if err = json.Unmarshal([]byte(data), &input); err != nil {
				t.Fatal(err)
			}
			if len(input.Files) != 1 || input.Files[0].Path == "" {
				t.Fatalf("no recovered picture: %s", data)
			}
			b, err := os.ReadFile(input.Files[0].Path)
			if err != nil || !bytes.Equal(b, pic.Bytes()) {
				t.Fatal("recovered file differs")
			}
			if d.calls != 1 {
				t.Fatalf("attachment fetched %d times", d.calls)
			}
			var given int
			if err = st.DB.QueryRow(`SELECT given FROM input_attachments WHERE path=?`, input.Files[0].Path).Scan(&given); err != nil || given != 1 {
				t.Fatalf("delivery receipt: %d %v", given, err)
			}
			// The codec gets the path, beyond its presence in the input event.
			if got := rec.of(f.root, 1); len(got) != 1 || !strings.Contains(got[0], input.Files[0].Path) {
				t.Fatalf("codec input lacks picture path: %q", got)
			}

		})
	}
}

func TestAttachmentReceiptOnlyAfterConfirmedInputOrAnswer(t *testing.T) {
	f := newFixture(t, options{})
	ctx := context.Background()
	// These rows exercise the runner's receipt transactions without starting
	// an agent process. The ordinary-input and answer encodings match core.
	for _, q := range []string{
		`INSERT INTO threads(id,scope,key,started_by,state,created_at,updated_at) VALUES(1,'s','k','owner','idle',0,0)`,
		`INSERT INTO sessions(id,thread_id,agent,state,created_at,updated_at) VALUES(1,1,'fake','idle',0,0)`,
		`INSERT INTO input_attachments(session_id,message,ordinal,name,path) VALUES(1,'one',0,'x','saved-copy')`,
	} {
		if _, err := f.st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	files := []control.InputFile{{Name: "x", Path: "saved-copy"}}
	data, _ := json.Marshal(session.PendingInput{Input: control.Input{Text: "examine"}, Files: files})
	if _, err := f.st.DB.Exec(`INSERT INTO pending_inputs(id,session_id,data) VALUES(42,1,?)`, string(data)); err != nil {
		t.Fatal(err)
	}
	receipt := func() int {
		var given int
		f.st.DB.QueryRow(`SELECT given FROM input_attachments`).Scan(&given)
		return given
	}
	if receipt() != 0 {
		t.Fatal("acceptance counted as delivery")
	}
	if err := f.st.Update(ctx, func(tx *store.Tx) error {
		return clearReported(ctx, tx, 1, []agent.Event{{Kind: agent.InputTaken, Input: "42"}})
	}); err != nil {
		t.Fatal(err)
	}
	if receipt() != 1 {
		t.Fatal("input confirmation lost its receipt")
	}
	f.st.DB.Exec(`UPDATE input_attachments SET given=0`)
	data, _ = json.Marshal(session.PendingAnswer{Answer: control.Answer{Text: "examine"}, Files: files})
	if _, err := f.st.DB.Exec(`INSERT INTO pending_inputs(id,session_id,request,data) VALUES(43,1,'question',?)`, string(data)); err != nil {
		t.Fatal(err)
	}
	if err := f.run.answerGiven(1, 43); err != nil {
		t.Fatal(err)
	}
	if receipt() != 1 {
		t.Fatal("successful answer lost its receipt")
	}
}
