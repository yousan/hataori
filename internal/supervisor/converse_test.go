package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// A closed thread's session keeps no token: that of the thread Unconfine is
// told is revoked, once, and no other thread's, nor a newer session's of it;
// a launch of it after a reopen binds the same one again. The token of a
// session that was replaced by one never launched is revoked too.
func TestUnconfineRevokesTheSessionsToken(t *testing.T) {
	var log bytes.Buffer
	s, tokens := layerSessions(t, &log)
	beside(t, s.Core, control.Idle, control.Idle, control.Idle)
	sessionOf(t, s.Core, own, 50)
	sessionOf(t, s.Core, newer, 80)
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

	s.Unconfine(99, 99)
	s.Unconfine(newer, 79)
	if len(tokens.revoked) != 0 {
		t.Fatalf("a thread without a token, and an older session of one: revoked %q", tokens.revoked)
	}
	s.Unconfine(newer, 80)
	s.Unconfine(newer, 80)
	if got := fmt.Sprint(tokens.revoked); got != "["+second+"]" {
		t.Fatalf("revoked %s, want the conversation's token, once", got)
	}
	bound := len(tokens.bound)
	confine(80, newer)
	if len(tokens.bound) != bound+1 || len(tokens.revoked) != 1 {
		t.Fatalf("its launch after a reopen bound %d, revoked %q", len(tokens.bound)-bound, tokens.revoked)
	}
	s.Unconfine(newer, 81) // it replaced 80, and its own launch failed before it had one
	if len(tokens.revoked) != 2 || tokens.revoked[1] != second {
		t.Fatalf("closed again: revoked %q", tokens.revoked)
	}
	s.Unconfine(own, 50)
	if len(tokens.revoked) != 3 || tokens.revoked[2] != testToken {
		t.Fatalf("the own thread's: revoked %q", tokens.revoked)
	}
}

// A launch of a newer session of a thread revokes the token of the one it
// replaces, when its own is another (#871).
func TestANewerSessionsTokenReplacesTheOlders(t *testing.T) {
	var log bytes.Buffer
	s, tokens := layerSessions(t, &log)
	beside(t, s.Core, control.Idle, control.Idle, control.Idle)
	sessionOf(t, s.Core, newer, 80)
	sessionOf(t, s.Core, newer, 81)
	dir, second := t.TempDir(), "1"+testToken[1:]
	if err := os.WriteFile(filepath.Join(dir, TokenFile), []byte(second+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	confine := func(session int64, state string) {
		t.Helper()
		if _, err := s.Confine(context.Background(), runner.ConfinedLaunch{Scope: "c1", Thread: newer, Session: session, Dir: t.TempDir(), StateDir: state}); err != nil {
			t.Fatal(err)
		}
	}
	confine(80, t.TempDir())
	for range 2 { // again, as a resume of it
		confine(81, dir)
		if got := fmt.Sprint(tokens.revoked); got != "["+testToken+"]" {
			t.Fatalf("revoked %s, want the older session's token, once", got)
		}
	}
	// One whose token is the older's keeps it bound.
	sessionOf(t, s.Core, newer, 82)
	confine(82, dir)
	if got := fmt.Sprint(tokens.revoked); got != "["+testToken+"]" {
		t.Fatalf("revoked %s: the newer session's own token too", got)
	}
	s.Unconfine(newer, 82)
	if got := fmt.Sprint(tokens.revoked); got != "["+testToken+" "+second+"]" {
		t.Fatalf("closed: revoked %s, want the newer one's too", got)
	}
}

// oneByOne are Tokens that note when two of their calls run at once.
type oneByOne struct {
	in      atomic.Int32
	overlap atomic.Bool
}

func (o *oneByOne) step() {
	if o.in.Add(1) > 1 {
		o.overlap.Store(true)
	}
	time.Sleep(20 * time.Millisecond)
	o.in.Add(-1)
}
func (o *oneByOne) Issue(control.API) (string, error) { o.step(); return testToken, nil }
func (o *oneByOne) Restore(string, control.API) error { o.step(); return nil }
func (o *oneByOne) Revoke(string)                     { o.step() }

// Binding a session's token and revoking a closed one's go one at a time,
// and a session that a newer one of its thread replaced is bound none, so
// no token is bound that a close does not revoke.
func TestTokensAreBoundAndRevokedOneAtATime(t *testing.T) {
	var log bytes.Buffer
	s, recorded := layerSessions(t, &log)
	beside(t, s.Core, control.Idle, control.Idle, control.Idle)
	sessionOf(t, s.Core, own, 40)
	sessionOf(t, s.Core, own, 50)
	sessionOf(t, s.Core, newer, 80)
	dir := t.TempDir()
	dirs := map[int64]string{40: t.TempDir(), 50: t.TempDir(), 80: t.TempDir()}
	confine := func(session int64, thread control.ThreadID) error {
		_, err := s.Confine(context.Background(), runner.ConfinedLaunch{Scope: "c1", Thread: thread, Session: session, Dir: dir, StateDir: dirs[session]})
		return err
	}
	tokens := &oneByOne{}
	s.Tokens = tokens
	for range 3 {
		var wg sync.WaitGroup
		wg.Go(func() { confine(50, own) })
		wg.Go(func() { confine(80, newer) })
		wg.Go(func() { s.Unconfine(own, 50) })
		wg.Go(func() { s.Unconfine(newer, 80) })
		wg.Wait()
	}
	if tokens.overlap.Load() {
		t.Error("two bindings or revocations ran at once")
	}

	s.Tokens = recorded
	if err := confine(50, own); err != nil {
		t.Fatal(err)
	}
	bound := len(recorded.bound)
	if err := confine(40, own); err == nil || len(recorded.bound) != bound || len(recorded.revoked) != 0 {
		t.Fatalf("a replaced session: %v, bound %d more, revoked %q; want it refused and nothing bound", err, len(recorded.bound)-bound, recorded.revoked)
	}
	if s.Unconfine(own, 50); len(recorded.revoked) != 1 {
		t.Fatalf("the newer session's close revoked %q, want its token", recorded.revoked)
	}
}

// A session that was closed while its launch was on the way, as after its
// start timed out, is bound no token: nothing would revoke it. Once the
// thread is reopened and woken, its launch binds it.
func TestAClosedSessionIsBoundNoToken(t *testing.T) {
	var log bytes.Buffer
	s, tokens := layerSessions(t, &log)
	beside(t, s.Core, control.Idle, control.Idle, control.Idle)
	sessionOf(t, s.Core, newer, 80)
	state := t.TempDir()
	for _, c := range []struct {
		state control.State
		bound int
	}{{control.Failed, 1}, {control.Closed, 0}, {control.Removed, 0}, {control.Creating, 1}} {
		exec(t, s.Core, fmt.Sprintf(`UPDATE sessions SET state = '%s' WHERE id = 80`, c.state))
		tokens.bound = nil
		s.Unconfine(newer, 80)
		_, err := s.Confine(context.Background(), runner.ConfinedLaunch{Scope: "c1", Thread: newer, Session: 80, Dir: t.TempDir(), StateDir: state})
		if (err == nil) != (c.bound == 1) || len(tokens.bound) != c.bound {
			t.Errorf("a launch of a session that is %s: %v, bound %d, want %d", c.state, err, len(tokens.bound), c.bound)
		}
	}
}

// A session closed while its launch waits to bind its token is bound none:
// its state is read once no other binding or revocation runs, so that one
// closed after that is unbound by its Unconfine.
func TestASessionClosedBeforeItsTokenIsBound(t *testing.T) {
	var log bytes.Buffer
	s, tokens := layerSessions(t, &log)
	beside(t, s.Core, control.Idle, control.Idle, control.Idle)
	sessionOf(t, s.Core, newer, 80)
	exec(t, s.Core, `UPDATE sessions SET state = 'failed' WHERE id = 80`)
	s.mu.Lock() // as another thread's binding
	done := make(chan error, 1)
	l := runner.ConfinedLaunch{Scope: "c1", Thread: newer, Session: 80, Dir: t.TempDir(), StateDir: t.TempDir()}
	go func() {
		_, err := s.Confine(context.Background(), l)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond) // it waits for the lock
	exec(t, s.Core, `UPDATE sessions SET state = 'closed' WHERE id = 80`)
	s.mu.Unlock()
	if err := <-done; err == nil || len(tokens.bound) != 0 {
		t.Fatalf("a launch of a session closed meanwhile: %v, bound %d, want none", err, len(tokens.bound))
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
	sessionOf(t, s.Core, own, 50)
	sessionOf(t, s.Core, newer, 80)
	sessionOf(t, s.Core, lookUp, 90)
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
