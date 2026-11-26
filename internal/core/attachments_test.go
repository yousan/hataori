package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
)

type fileResolver func(context.Context, int64, string, string, []control.IncomingFile) ([]control.InputFile, error)

func (f fileResolver) Resolve(ctx context.Context, sid int64, client, key string, files []control.IncomingFile) ([]control.InputFile, error) {
	return f(ctx, sid, client, key, files)
}
func TestAttachmentFetchCannotCrossSessionReplacement(t *testing.T) {
	f := newFixture(t, fakeOwner{key: "t1"})
	id, sid := f.running(t, "files")
	calls := 0
	f.core.Attachments = fileResolver(func(ctx context.Context, which int64, _, _ string, _ []control.IncomingFile) ([]control.InputFile, error) {
		calls++
		if calls == 1 {
			if which != sid {
				t.Fatal("wrong first session")
			}
			if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET state='failed' WHERE id=?`, sid); err != nil {
				t.Fatal(err)
			}
			if _, err := f.core.Store.DB.Exec(`INSERT INTO sessions(thread_id,agent,state,created_at,updated_at) VALUES(?,'fake','idle',0,0)`, int64(id)); err != nil {
				t.Fatal(err)
			}
		} else if which == sid {
			t.Fatal("retry resolved the replaced session")
		}
		return []control.InputFile{{Name: "x", Reason: "test skip"}}, nil
	})
	input := control.Input{Text: "examine", Attachments: []control.IncomingFile{{Name: "x", URL: "service-file"}}}
	err := f.owner.Send(context.Background(), id, "file-input", input, control.Deliver)
	if err == nil || !strings.Contains(err.Error(), "session changed") {
		t.Fatalf("crossed replacement: %v", err)
	}
	if err = f.owner.Send(context.Background(), id, "file-input", input, control.Deliver); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("retry calls: %d", calls)
	}
	var accepted int
	if err = f.core.Store.DB.QueryRow(`SELECT count(*) FROM events WHERE kind='input' AND json_extract(data,'$.text')='examine'`).Scan(&accepted); err != nil || accepted != 1 {
		t.Fatalf("accepted %d times: %v", accepted, err)
	}
}
func TestFailedInputDoesNotFetchAttachments(t *testing.T) {
	f := newFixture(t, fakeOwner{key: "t1"})
	id, sid := f.running(t, "files")
	f.core.Store.DB.Exec(`UPDATE sessions SET state='failed' WHERE id=?`, sid)
	f.core.Attachments = fileResolver(func(context.Context, int64, string, string, []control.IncomingFile) ([]control.InputFile, error) {
		t.Fatal("failed thread fetched attachments")
		return nil, nil
	})
	err := f.owner.Send(context.Background(), id, "file-input", control.Input{Attachments: []control.IncomingFile{{Name: "x", URL: "service-file"}}}, control.Deliver)
	if !errors.Is(err, session.ErrRefused) {
		t.Fatalf("failed input: %v", err)
	}
}
