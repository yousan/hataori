package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

func outsideSessions(t *testing.T) (*Sessions, *recordingTokens) {
	t.Helper()
	s, tokens := layerSessions(t, &bytes.Buffer{})
	s.Core.Machine = &session.Machine{Store: s.Core.Store}
	s.Core.People = people.New([]people.Person{{Name: "sv", IDs: []string{ID("c1")}}}, "owner", map[string][]string{"c1": {"sv"}})
	s.Core.Scopes = map[string]core.Scope{"c1": {Enabled: true, Owner: "chat"}}
	s.Core.Agent = "fake"
	s.Core.Log = s.Log
	svThread(t, s.Core)
	return s, tokens
}

func TestOutsideQuotaUsesHalfAndDefaultAcrossConversations(t *testing.T) {
	ctx := context.Background()
	for _, configured := range []int{0, 1, 3, 4} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			s, _ := outsideSessions(t)
			sc := s.Scopes["c1"]
			sc.Autonomy, sc.Limits.Sends = Act, configured
			s.Scopes["c1"] = sc
			for _, id := range []int{2, 3, 4} {
				exec(t, s.Core, fmt.Sprintf(`INSERT INTO threads (id, scope, key, started_by, state, confined, kind, created_at, updated_at)
					VALUES (%d, 'c1', 'k%d', 'owner', 'creating', 0, 'ordinary', 0, 0)`, id, id))
				sessionOf(t, s.Core, control.ThreadID(id), int64(id))
			}
			for _, id := range []control.ThreadID{1, 2} {
				if err := s.Core.Store.Update(ctx, func(tx *store.Tx) error {
					return store.MarkConversation(ctx, tx, id, "", []control.OutsideText{{Kind: "message", ID: "outside"}})
				}); err != nil {
					t.Fatal(err)
				}
			}
			limit := configured
			if limit == 0 {
				limit = 20
			}
			limit /= 2
			s.Reminders = map[string]*Reminders{"c1": NewReminders(s.Core, "c1", 100, s.Log)}
			proposals := limit
			if limit > 0 {
				if _, err := s.API("c1", 1).Remind(ctx, control.RemindRequest{Delay: 600, Note: "marked reminder"}); err != nil {
					t.Fatal(err)
				}
				proposals--
			}
			for i := range proposals {
				api := s.API("c1", control.ThreadID(i%2+1))
				err := api.Send(ctx, 4, fmt.Sprintf("m%d", i), control.Input{Text: "proposed"}, control.Refuse)
				if !errors.Is(err, control.ErrProposed) {
					t.Fatalf("send %d of %d: %v", i+1, limit, err)
				}
			}
			if err := s.API("c1", 2).Send(ctx, 4, "over", control.Input{Text: "over"}, control.Refuse); !errors.Is(err, control.ErrForbidden) {
				t.Fatalf("past marked half: %v", err)
			}
			// The remaining unmarked allowance is unaffected.
			if err := s.APIOf("c1", 3, Propose).Send(ctx, 4, "unmarked", control.Input{Text: "allowed"}, control.Refuse); !errors.Is(err, control.ErrProposed) {
				t.Fatalf("unmarked proposal refused: %v", err)
			}
		})
	}
}

func TestOutsideTokenRestoresLaunchKeyAfterWakeEnds(t *testing.T) {
	ctx := context.Background()
	s, tokens := outsideSessions(t)
	sc := s.Scopes["c1"]
	sc.Autonomy = Act
	s.Scopes["c1"] = sc
	key := "hataori-wake-marked"
	exec(t, s.Core, `UPDATE sessions SET outside_key = 'hataori-wake-marked' WHERE id = 1`)
	if err := s.Core.Store.Update(ctx, func(tx *store.Tx) error {
		return store.MarkConversation(ctx, tx, 1, key, []control.OutsideText{{Kind: "message", ID: "outside"}})
	}); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	if _, err := s.bind(ctx, 1, 1, "c1", stateDir, true); err != nil {
		t.Fatal(err)
	}
	exec(t, s.Core, `UPDATE sessions SET outside_key = '', state = 'asleep' WHERE id = 1`)
	// A reconnect restores the launch identity, even after set_aside was
	// restored, rather than using the current main-conversation key.
	if _, err := s.bind(ctx, 1, 1, "c1", stateDir, false); err != nil {
		t.Fatal(err)
	}
	for i, api := range tokens.bound {
		_, err := api.Start(ctx, control.StartRequest{Scope: "c1", Key: fmt.Sprint(i), Repo: "o/listed", Input: control.Input{Text: "late"}})
		if !errors.Is(err, control.ErrProposed) {
			t.Fatalf("launch binding %d lost its mark: %v", i, err)
		}
	}
}

func fileMark(t *testing.T, f *filesFixture, id control.ThreadID, key string) {
	t.Helper()
	ctx := context.Background()
	if err := f.files.Store.Update(ctx, func(tx *store.Tx) error {
		return store.MarkConversation(ctx, tx, id, key, []control.OutsideText{{Kind: "file", ID: "outside-file"}})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOutsideFilesRefuseUserFilesAndEveryWriteUnderAct(t *testing.T) {
	ctx := context.Background()
	f := newFilesFixture(t)
	fileMark(t, f, filesConv, "")
	for _, name := range UserFiles {
		for _, path := range []string{name, strings.ToLower(name), strings.ToUpper(name)} {
			text := "NEVER-WRITTEN"
			_, err := f.conv.Files(ctx, control.FilesRequest{Op: control.FilesWrite, Path: path, Text: &text})
			if err == nil || !strings.Contains(err.Error(), "unmarked conversation") {
				t.Fatalf("protected file %q: %v", path, err)
			}
			if _, err := os.Lstat(filepath.Join(f.dir, path)); !os.IsNotExist(err) {
				t.Fatalf("protected file was written: %s, %v", path, err)
			}
		}
	}
	text := "notes are allowed under propose"
	r := control.FilesRequest{Op: control.FilesWrite, Path: "notes/proposed", Text: &text}
	if _, err := f.conv.Files(ctx, r); err != nil {
		t.Fatal(err)
	}
	act := f.files.API(nil, "c1", filesConv, Act)
	for _, op := range []string{control.FilesWrite, control.FilesEdit} {
		r := control.FilesRequest{Op: op, Path: "new-directory/refused", Text: &text, Old: "old", New: &text}
		if _, err := act.Files(ctx, r); err == nil {
			t.Fatalf("marked %s under act was allowed", op)
		}
		if _, err := os.Lstat(filepath.Join(f.dir, "new-directory")); !os.IsNotExist(err) {
			t.Fatalf("refused write created a directory: %v", err)
		}
	}
	// A separate unmarked conversation still writes instruction files.
	r = control.FilesRequest{Op: control.FilesWrite, Path: UserFiles[0], Text: &text}
	if _, err := f.own.Files(ctx, r); err != nil {
		t.Fatalf("unmarked write refused: %v", err)
	}
}

func TestOutsideFilePolicyRecheckedWithWriteRecord(t *testing.T) {
	ctx := context.Background()
	f := newFilesFixture(t)
	put(t, f.dir, UserFiles[0], "original")
	old, err := f.conv.Files(ctx, control.FilesRequest{Op: control.FilesRead, Path: UserFiles[0]})
	if err != nil {
		t.Fatal(err)
	}
	marked := false
	f.files.Hook = func(step string) {
		if step == "opening" && !marked {
			marked = true
			fileMark(t, f, filesConv, "")
		}
	}
	text := "replacement"
	if _, err := f.conv.Files(ctx, control.FilesRequest{Op: control.FilesWrite, Path: UserFiles[0], Text: &text, Version: old.File.Version}); err == nil {
		t.Fatal("write escaped after its conversation became marked")
	}
	got, err := os.ReadFile(filepath.Join(f.dir, UserFiles[0]))
	if err != nil || string(got) != "original" {
		t.Fatalf("protected file changed: %q, %v", got, err)
	}
}

func TestOutsideFinishedWakeFilesKeepLaunchMark(t *testing.T) {
	ctx := context.Background()
	f := newFilesFixture(t)
	key := "hataori-wake-marked"
	fileMark(t, f, filesOwn, key)
	api := f.files.API(nil, "c1", filesOwn, Act).(*filing)
	api.key = &key // a launch's binding, retained during shutdown
	text := "late write"
	if _, err := api.Files(ctx, control.FilesRequest{Op: control.FilesWrite, Path: "notes/late", Text: &text}); err == nil {
		t.Fatal("finished wake used the restored conversation's permissions")
	}
	if _, err := f.own.Files(ctx, control.FilesRequest{Op: control.FilesWrite, Path: "notes/main", Text: &text}); err != nil {
		t.Fatalf("set-aside conversation was marked: %v", err)
	}
}
