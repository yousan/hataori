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
	"github.com/yousan/hataori/internal/runner"
)

// A call in a scope that names no supervisor, or whose supervisor has
// conversations turned off, starts nothing: it makes no thread, no
// directory, and one log line that names the scope, who called and why. One
// without a key, which could not be told from another, starts nothing
// either (#871).
func TestConverseStartsNothingWhereThereAreNone(t *testing.T) {
	var log bytes.Buffer
	s, _ := layerSessions(t, &log)
	on := s.Scopes["c1"]
	on.Conversations = true
	s.Scopes = map[string]Scope{"c1": {}, "c2": on}
	rows := func() (n int) {
		t.Helper()
		if err := s.Core.Store.DB.QueryRow(`SELECT (SELECT count(*) FROM threads) + (SELECT count(*) FROM supervisor_dirs) + (SELECT count(*) FROM links)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for scope, why := range map[string]string{"c1": "turned off", "c3": "names no supervisor"} {
		log.Reset()
		id, err := s.Converse(context.Background(), Call{Scope: scope, Thread: "th", Key: "k", From: "chat:ann", Text: "hello"})
		if !errors.Is(err, ErrNoConversations) || id != 0 {
			t.Errorf("%s: %d, %v; want ErrNoConversations", scope, id, err)
		}
		if got := log.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "conversation refused") ||
			!strings.Contains(got, "scope="+scope) || !strings.Contains(got, "from=chat:ann") || !strings.Contains(got, why) {
			t.Errorf("%s: logged %q", scope, got)
		}
	}
	if id, err := s.Converse(context.Background(), Call{Scope: "c2", Thread: "th", From: "chat:ann", Text: "hello"}); err == nil || id != 0 {
		t.Errorf("without a key: %d, %v", id, err)
	}
	if n := rows(); n != 0 {
		t.Errorf("%d rows of threads, directories and links, want none", n)
	}
}

// A refused call does not even have its scope's directory recorded, in a
// scope whose supervisor has none yet: who may start is asked first (#871).
func TestConverseRecordsNoDirectoryForARefusedCall(t *testing.T) {
	var log bytes.Buffer
	s, _ := layerSessions(t, &log)
	on := s.Scopes["c1"]
	on.Conversations = true
	s.Scopes = map[string]Scope{"c1": on, "c2": on}
	s.Core.Log = s.Log
	s.Core.Scopes = map[string]core.Scope{"c1": {Enabled: true}, "c2": {}}
	s.Core.People = people.New([]people.Person{{Name: "ann", IDs: []string{"chat:ann"}}}, "boss", map[string][]string{"c1": {"ann"}, "c2": {"ann"}})
	for _, c := range []struct{ scope, from, why string }{
		{"c1", "chat:stranger", "a guest"},
		{"c1", "", ""},
		{"c2", "chat:ann", "scope is not enabled"},
	} {
		log.Reset()
		id, err := s.Converse(context.Background(), Call{Scope: c.scope, Thread: "th", Key: "k", From: c.from, Text: "hello"})
		if err == nil || id != 0 {
			t.Errorf("%+v: %d, %v; want it refused", c, id, err)
		}
		if got := log.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "refused") || !strings.Contains(got, c.why) {
			t.Errorf("%+v: logged %q, want one line", c, got)
		}
		var n int
		if err := s.Core.Store.DB.QueryRow(`SELECT count(*) FROM supervisor_dirs`).Scan(&n); err != nil || n != 0 {
			t.Errorf("%+v: %d directories recorded (%v)", c, n, err)
		}
	}
}

// A closed thread's session keeps no token: what Unconfine is told is
// revoked, once, and no other session's; a launch of it after a reopen
// binds the same one again.
func TestUnconfineRevokesTheSessionsToken(t *testing.T) {
	var log bytes.Buffer
	s, tokens := layerSessions(t, &log)
	beside(t, s.Core, control.Idle, control.Idle, control.Idle)
	dirs := map[int64]string{}
	confine := func(session int64, thread control.ThreadID) {
		t.Helper()
		if dirs[session] == "" {
			dirs[session] = t.TempDir()
		}
		if _, err := s.Confine(context.Background(), runner.ConfinedLaunch{Scope: "c1", Thread: thread, Session: session,
			Dir: t.TempDir(), StateDir: dirs[session]}); err != nil {
			t.Fatal(err)
		}
	}
	confine(50, own)
	confine(80, newer)
	second := "1" + testToken[1:]
	if err := os.WriteFile(filepath.Join(dirs[80], TokenFile), []byte(second+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	confine(80, newer) // as a resume, with a token of its own

	s.Unconfine(99)
	if len(tokens.revoked) != 0 {
		t.Fatalf("a session without a token: revoked %q", tokens.revoked)
	}
	s.Unconfine(80)
	s.Unconfine(80)
	if got := fmt.Sprint(tokens.revoked); got != "["+second+"]" {
		t.Fatalf("revoked %s, want the conversation's token, once", got)
	}
	bound := len(tokens.bound)
	confine(80, newer)
	if len(tokens.bound) != bound+1 || len(tokens.revoked) != 1 {
		t.Fatalf("its launch after a reopen bound %d, revoked %q", len(tokens.bound)-bound, tokens.revoked)
	}
	s.Unconfine(80)
	if len(tokens.revoked) != 2 || tokens.revoked[1] != second {
		t.Fatalf("closed again: revoked %q", tokens.revoked)
	}
	s.Unconfine(50)
	if len(tokens.revoked) != 3 || tokens.revoked[2] != testToken {
		t.Fatalf("the own thread's: revoked %q", tokens.revoked)
	}
}

// A conversation's launch is given the user's files, and tells the
// supervisor's own thread nothing: its next wake still names what changed
// since its own session was given them.
func TestAConversationsLaunchIsNotTheOwnThreads(t *testing.T) {
	ctx := context.Background()
	var log bytes.Buffer
	s, _ := layerSessions(t, &log)
	beside(t, s.Core, control.Idle, control.Idle, control.Idle)
	dir := t.TempDir()
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	launch := func(session int64, thread control.ThreadID, want string) {
		t.Helper()
		c, err := s.Confine(ctx, runner.ConfinedLaunch{Scope: "c1", Thread: thread, Session: session, Dir: dir, StateDir: t.TempDir()})
		if err != nil || !strings.Contains(c.Instructions, want) {
			t.Fatalf("thread %d's launch is not given %q: %v", thread, want, err)
		}
	}
	told := func() string {
		t.Helper()
		m, err := s.recorded(ctx, "c1", "told")
		if err != nil {
			t.Fatal(err)
		}
		return m["AGENTS.md"]
	}
	write("one")
	launch(80, newer, "one")
	if got := told(); got != "" {
		t.Fatalf("after a conversation's launch the own thread was told %q", got)
	}
	launch(50, own, "one")
	first := told()
	if first == "" {
		t.Fatal("after its own launch it was told nothing")
	}
	write("two")
	launch(80, newer, "two")
	launch(90, lookUp, "two")
	if got := told(); got != first {
		t.Fatal("a conversation's launch counts as telling the own thread of the change")
	}
	launch(50, own, "two")
	if got := told(); got == first {
		t.Fatal("its own launch does not")
	}
}
