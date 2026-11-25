package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/core"
	"github.com/yousan/hataori/internal/people"
	"github.com/yousan/hataori/internal/runner"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// supervisors gives the running daemon's supervisors, through which these
// tests start conversations as a chat front end would. Call it before the
// daemon starts.
func supervisors(t *testing.T) *atomic.Pointer[supervisor.Sessions] {
	t.Helper()
	p := &atomic.Pointer[supervisor.Sessions]{}
	sessionsHook = func(s *supervisor.Sessions) { p.Store(s) }
	t.Cleanup(func() { sessionsHook = nil })
	return p
}

// svTalks is svAutonomy and the conversations of its scope.
type svTalks struct {
	*svAutonomy
	sessions *atomic.Pointer[supervisor.Sessions]
}

func newSvTalks(t *testing.T, mode string, more ...func(cfg map[string]any)) *svTalks {
	t.Helper()
	s := supervisors(t)
	return &svTalks{newSvAutonomy(t, mode, append([]func(cfg map[string]any){caller}, more...)...), s}
}

// svCaller is a person of each scope who calls the supervisor. The other
// tests' person, the webhook e2eHook, calls nobody (design, section 7.1).
const svCaller = "caller-1"

func caller(cfg map[string]any) {
	cfg["people"] = append(cfg["people"].([]map[string]any), map[string]any{"name": "ann", "ids": []string{fakeSurface + ":" + svCaller}})
	for _, sc := range cfg["scopes"].([]map[string]any) {
		sc["people"] = append(sc["people"].([]string), "ann")
	}
}

// as calls the supervisor of c1 from the chat thread key, as from.
func (f *svTalks) as(from, key, text string) (control.ThreadID, error) {
	return f.sessions.Load().Converse(context.Background(), supervisor.Call{Scope: "c1", Thread: "t-" + key, Key: key, From: from, Text: text})
}

// talk starts a conversation of a person of the scope in the chat thread
// t-<key>, and waits for the end of its first turn.
func (f *svTalks) talk(t *testing.T, key string) control.ThreadID {
	t.Helper()
	id, err := f.as(people.Join(fakeSurface, svCaller), key, "reply hello from "+key)
	if err != nil {
		t.Fatal(err)
	}
	f.wt.ended(t, id, 1)
	return id
}

// chat is the chat thread of talk's conversation key.
func (f *svTalks) chat(key string) surface.Conversation {
	return surface.Conversation{Channel: "c1", Thread: "t-" + key}
}

// tokenOf is the token of thread id's newest session.
func tokenOf(t *testing.T, root string, id control.ThreadID) string {
	t.Helper()
	var sid int64
	if err := dbOf(t, root).QueryRow(`SELECT id FROM sessions WHERE thread_id = ? ORDER BY id DESC LIMIT 1`, int64(id)).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(runner.StateDir(root, sid), supervisor.TokenFile))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// tools are the supervisor's tools as thread id's session reaches them.
func (f *svTalks) tools(t *testing.T, id control.ThreadID) *supervisor.MCP {
	t.Helper()
	root, token := f.wt.r.root, tokenOf(t, f.wt.r.root, id)
	return &supervisor.MCP{Dial: func() (control.API, error) { return wire.DialToken(root, token) }, Scope: "c1"}
}

// by runs do with the tools of thread id in place of the own thread's.
func (f *svTalks) by(t *testing.T, id control.ThreadID, key string) *svTalks {
	t.Helper()
	g := *f.svAutonomy
	g.m, g.conv = f.tools(t, id), f.chat(key)
	return &svTalks{&g, f.sessions}
}

// made counts what a start leaves: threads, links, recorded directories,
// directories on the disk, and chat threads of c1's channel.
func (f *svTalks) made(t *testing.T) string {
	t.Helper()
	var n [3]int
	for i, table := range []string{"threads", "links", "supervisor_dirs"} {
		if err := f.wt.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n[i]); err != nil {
			t.Fatal(err)
		}
	}
	dirs, _ := filepath.Glob(filepath.Join(f.wt.r.root, "*", "*"))
	return fmt.Sprint(n, len(dirs), f.wt.r.openedIn("c1"))
}

// A conversation is started by a person of the scope alone: a call by a
// guest, a bot or a webhook that is nobody's, a person of another scope
// only, or nobody makes no thread, no link and no directory, and one log
// line. A person's makes one thread, in the chat thread the call came from
// or in a new one on the channel; its key again makes none, also after a
// restart, and a key that started a thread of another kind is refused
// (#871).
func TestConversationIsStartedByAPerson(t *testing.T) {
	f := newSvTalks(t, "", func(cfg map[string]any) {
		cfg["people"] = append(cfg["people"].([]map[string]any), map[string]any{"name": "eve", "ids": []string{fakeSurface + ":eve"}})
		cfg["scopes"].([]map[string]any)[1]["people"] = []string{"dispatch", "eve"}
	})
	ctx := context.Background()
	before := f.made(t)
	for _, c := range []struct{ who, from, reason string }{
		{"a guest", people.Join(fakeSurface, svGuest), "a guest"},
		{"a bot", people.Join(fakeSurface, "e2e-some-bot"), "a guest"},
		{"a webhook", people.Join(fakeSurface, "e2e-some-webhook"), "a guest"},
		{"a person of c2 only", people.Join(fakeSurface, "eve"), "the person may not act in the scope"},
		{"a name in place of an id", "dispatch", "a guest"},
		{"nobody", "", ""},
	} {
		was := len(f.log())
		id, err := f.as(c.from, "key-of-the-refused", "reply no")
		if !errors.Is(err, control.ErrForbidden) || id != 0 {
			t.Errorf("%s: thread %d, %v; want it refused", c.who, id, err)
		}
		var lines []string
		for _, l := range strings.Split(strings.TrimSpace(f.log()[was:]), "\n") {
			if strings.Contains(l, "refused") {
				lines = append(lines, l)
			}
		}
		if len(lines) != 1 || !refusedLine(lines[0], "Start", "c1") || !strings.Contains(lines[0], c.reason) {
			t.Errorf("%s: the log says %q; want one line of the refused start", c.who, lines)
		}
	}
	if got := f.made(t); got != before {
		t.Fatalf("after refused calls: %s (threads, links, recorded directories; directories; chat threads), want %s still", got, before)
	}
	// Nobody has taken the key.
	id, err := f.as(people.Join(fakeSurface, svCaller), "key-of-the-refused", "\n  \nreply hello  \nand more")
	if err != nil {
		t.Fatal(err)
	}
	f.wt.ended(t, id, 1)
	d, err := f.owner.Thread(ctx, id, 0, 0)
	if err != nil || d.Kind != control.KindConversation || d.Title != "reply hello" || len(d.Links) != 1 ||
		d.Links[0] != (control.ThreadLink{LinkRef: control.LinkRef{Client: fakeSurface, Channel: "c1", Thread: "t-key-of-the-refused"}}) {
		t.Fatalf("the conversation: %+v %v", d, err)
	}
	if in := threadEvents(t, f.owner, id, "input"); len(in) != 1 || in[0]["principal"] != "ann" || in[0]["from"] != people.Join(fakeSurface, svCaller) {
		t.Fatalf("its input: %v", in)
	}
	var dir, own string
	f.wt.db.QueryRow(`SELECT workspace FROM threads WHERE id = ?`, int64(id)).Scan(&dir)
	f.wt.db.QueryRow(`SELECT workspace FROM threads WHERE id = ?`, int64(f.sv.ID)).Scan(&own)
	if dir == "" || dir != own {
		t.Fatalf("it works in %q, want the scope's directory %q", dir, own)
	}
	f.wt.r.postedWith(t, f.chat("key-of-the-refused"), "hello")

	once := f.made(t)
	again := func(when string) {
		t.Helper()
		if got, err := f.as(people.Join(fakeSurface, svCaller), "key-of-the-refused", "reply hello"); err != nil || got != id {
			t.Fatalf("%s: thread %d, %v; want %d again", when, got, err, id)
		}
		if got := f.made(t); got != once {
			t.Fatalf("%s: %s, want %s still", when, got, once)
		}
		if n := len(threadEvents(t, f.owner, id, "input")); n != 1 {
			t.Fatalf("%s: %d inputs", when, n)
		}
	}
	again("its key again")
	f.restart(t)
	again("its key again after a restart")

	// A call in the channel opens a chat thread for it.
	opened := f.wt.r.openedIn("c1")
	inChannel, err := f.sessions.Load().Converse(ctx, supervisor.Call{Scope: "c1", Key: "in-the-channel", From: people.Join(fakeSurface, e2eOwner), Text: "reply hi"})
	if err != nil {
		t.Fatal(err)
	}
	f.wt.ended(t, inChannel, 1)
	if d, err := f.owner.Thread(ctx, inChannel, 0, 0); err != nil || len(d.Links) != 1 || d.Links[0].Thread == "" || !d.Links[0].Opened || f.wt.r.openedIn("c1") != opened+1 {
		t.Fatalf("a call in the channel: %+v %v, %d chat threads opened", d, err, f.wt.r.openedIn("c1")-opened)
	}

	// A key is of one kind.
	f.wt.worker(t, f.owner, "c1", "worker-key", "reply w")
	once = f.made(t)
	if got, err := f.as(people.Join(fakeSurface, svCaller), "worker-key", "reply no"); !errors.Is(err, core.ErrKindKey) {
		t.Fatalf("a worker's key: thread %d, %v; want it refused", got, err)
	}
	if got := f.made(t); got != once {
		t.Fatalf("a worker's key: %s, want %s still", got, once)
	}
}

// A conversation is launched confined as the supervisor's own thread is,
// with a token of its own, at its start and at each resume; the token
// reaches its scope only, as its thread; a restart binds it again; it is
// revoked when the thread is closed, and the launch after a reopen binds
// it again (#871).
func TestConversationIsConfinedWithATokenOfItsOwn(t *testing.T) {
	record := t.TempDir()
	f := newSvTalks(t, "", recordIn(record))
	ctx := context.Background()
	other, err := f.owner.Start(ctx, control.StartRequest{Key: "x", Scope: "c2", Thread: "tx", Repo: f.wt.repo, Input: control.Input{Text: "reply x"}})
	if err != nil {
		t.Fatal(err)
	}
	f.wt.ended(t, other, 1)
	id := f.talk(t, "a")
	token := tokenOf(t, f.wt.r.root, id)
	if token == "" || token == f.token {
		t.Fatal("the conversation has no token of its own")
	}

	// Its start's arguments are the own thread's, but for what names the
	// session and for what an own thread's later launches have.
	launched := func() (own, conv []fakeStart) {
		t.Helper()
		for _, s := range starts(t, record) {
			switch {
			case len(s.Inputs) > 0 && strings.Contains(strings.Join(s.Inputs, "\n"), "reply x"):
			case strings.Contains(strings.Join(s.Inputs, "\n"), supervisor.FirstInput):
				own = append(own, s)
			default:
				conv = append(conv, s)
			}
		}
		return own, conv
	}
	flags := func(s fakeStart) string {
		var out []string
		for _, a := range s.Args {
			if strings.HasPrefix(a, "--") {
				out = append(out, a)
			}
		}
		return strings.Join(out, " ")
	}
	confined := func(when string, s fakeStart) {
		t.Helper()
		file, ok := s.value("--mcp-config")
		if !ok {
			t.Fatalf("%s: launched without the confined tools: %q", when, s.Args)
		}
		b, err := os.ReadFile(file)
		if err != nil || !strings.Contains(string(b), token) || strings.Contains(string(b), f.token) {
			t.Fatalf("%s: the MCP config does not have the conversation's token, or has the own thread's (%v)", when, err)
		}
	}
	own, conv := launched()
	if len(own) != 1 || len(conv) != 1 || flags(own[0]) != flags(conv[0]) {
		t.Fatalf("the own thread's start %v, the conversation's %v; want one each, with the same flags", own, conv)
	}
	confined("at its start", conv[0])

	reaches := func(when string) {
		t.Helper()
		a := sendAndRead(t, f.owner, id, "list-"+when, "mcp list_threads")
		if !strings.HasPrefix(a, "mcp: 2 open threads in this channel:") || strings.Contains(a, fmt.Sprintf("\n#%d ", other)) {
			t.Fatalf("%s: list_threads answered %q; want c1's two", when, a)
		}
		if a := sendAndRead(t, f.owner, id, "read-"+when, fmt.Sprintf(`mcp read_thread {"number": %d}`, other)); !strings.HasPrefix(a, "mcp error: not in this scope") {
			t.Fatalf("%s: read_thread of c2's thread answered %q", when, a)
		}
	}
	dials := func(when string, want bool) {
		t.Helper()
		c, err := wire.DialToken(f.wt.r.root, token)
		if err == nil {
			c.Close()
		}
		if (err == nil) != want {
			t.Fatalf("%s: the conversation's token connects: %v (%v), want %v", when, err == nil, err, want)
		}
	}
	reaches("at its start")
	dials("running", true)
	f.restart(t)
	dials("after a restart", true)
	reaches("after a restart")

	// Asleep, then woken by a message. The idle period goes by the real
	// clock, so the core does too from here on.
	f.owner.Close()
	f.stop()
	coreHook = nil
	setConfig(t, f.wt.r.root, func(cfg map[string]any) { cfg["idle"] = "1s" })
	f.start(t)
	reaches("with a short idle period")
	eventually(t, "the conversation asleep", 15*time.Second, func() bool {
		d, err := f.owner.Thread(ctx, id, 0, 0)
		return err == nil && d.State == control.Asleep
	})
	reaches("after it slept")
	_, conv = launched()
	if len(conv) < 2 {
		t.Fatalf("%d starts of the conversation; want another after it slept", len(conv))
	}
	confined("at its resume", conv[len(conv)-1])

	// Closed, and not for a connection that was open either.
	open, err := wire.DialToken(f.wt.r.root, token)
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	eventually(t, "the conversation closed", 15*time.Second, func() bool { return f.owner.CloseThread(ctx, id) == nil })
	dials("closed", false)
	if _, err := open.Threads(ctx, control.Filter{Scope: "c1"}); err == nil {
		t.Fatal("a connection of the closed conversation still reads")
	}
	if c, err := wire.DialToken(f.wt.r.root, f.token); err != nil {
		t.Fatalf("the own thread's token: %v", err)
	} else {
		c.Close()
	}
	f.restart(t)
	dials("closed, after a restart", false)
	if _, err := f.owner.ReopenThread(ctx, id); err != nil {
		t.Fatal(err)
	}
	reaches("after a reopen")
	dials("after a reopen", true)
	if tokenOf(t, f.wt.r.root, id) != token {
		t.Fatal("the reopened conversation has another token")
	}
	_, conv = launched()
	confined("after a reopen", conv[len(conv)-1])
}

// A conversation's proposal is stored in its own thread, shown there with
// its buttons, and runs once, after a person of the scope approved it
// there. Nothing of it is shown in the supervisor's own thread or in
// another conversation, and no button there decides it (#871).
func TestConversationsProposalIsDecidedInItsOwnThread(t *testing.T) {
	f := newSvTalks(t, "propose")
	ctx := context.Background()
	w, _ := f.wt.worker(t, f.owner, "c1", "w", "reply w")
	a, b := f.talk(t, "a"), f.talk(t, "b")
	inA, inB := f.by(t, a, "a"), f.by(t, b, "b")
	n := inA.propose(t, supervisor.ToolSend, map[string]any{"number": int64(w), "text": "reply ordered"})
	q := inA.shown(t, n)
	nb := inB.propose(t, supervisor.ToolSend, map[string]any{"number": int64(w), "text": "reply from b"})
	qb := inB.shown(t, nb)
	for _, id := range []control.ThreadID{a, b, f.sv.ID} {
		made := threadEvents(t, f.owner, id, "proposal_made")
		d, err := f.owner.Thread(ctx, id, 0, 0)
		want := 1
		if id == f.sv.ID {
			want = 0
		}
		if err != nil || len(made) != want || len(d.Requests) != want {
			t.Fatalf("thread %d: proposals %v, requests %+v, %v; want %d", id, made, d.Requests, err, want)
		}
	}
	// What is in one place is in no other.
	for _, c := range []struct {
		conv surface.Conversation
		not  string
	}{{f.conv, "Proposal "}, {f.chat("a"), "reply from b"}, {f.chat("b"), "reply ordered"}} {
		for _, p := range f.wt.r.driver.Calls(c.conv) {
			if strings.Contains(p.Message.Text, c.not) {
				t.Fatalf("in %v: %q", c.conv, p.Message.Text)
			}
		}
	}

	// A press of a's button in b's chat thread, and by a guest and the
	// supervisor's own person in a's, decide nothing.
	forged, err := f.wt.r.driver.Post(ctx, f.chat("b"), "", q.Message)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.wt.r.driver.PressMany(forged, e2eHook, choiceToken(t, q, "Approve")); err != nil {
		t.Fatal(err)
	}
	inA.press(t, q, svGuest, "Approve")
	inA.press(t, q, svPerson, "Approve")
	eventually(t, "the presses refused", 10*time.Second, func() bool {
		return strings.Contains(f.log(), "button ignored") && strings.Count(f.log(), "op=Answer") >= 2
	})
	for _, id := range []control.ThreadID{a, b} {
		if ev := threadEvents(t, f.owner, id, "proposal_answered"); len(ev) != 0 {
			t.Fatalf("before a person approved in its thread: %v in thread %d", ev, id)
		}
	}
	if got := f.fromSupervisor(t, w); got != 0 {
		t.Fatalf("%d inputs of the supervisor", got)
	}

	inA.press(t, q, e2eHook, "Approve")
	eventually(t, "the proposal answered", 15*time.Second, func() bool { return len(threadEvents(t, f.owner, a, "proposal_answered")) == 1 })
	f.wt.ended(t, w, 2)
	f.wt.r.posted(t, f.chat("a"), fmt.Sprintf("Approved by dispatch: proposal %d was sent to thread %d.", n, w))
	inA.press(t, q, e2eOwner, "Approve")
	f.restart(t)
	inA.press(t, q, e2eOwner, "Approve")
	time.Sleep(300 * time.Millisecond)
	if got := f.fromSupervisor(t, w); got != 1 {
		t.Fatalf("the proposal ran %d times, want once", got)
	}
	if ev := threadEvents(t, f.owner, b, "proposal_answered"); len(ev) != 0 {
		t.Fatalf("the other conversation's was answered: %v", ev)
	}
	inB.press(t, qb, e2eHook, "Refuse")
	eventually(t, "the other proposal answered", 15*time.Second, func() bool { return len(threadEvents(t, f.owner, b, "proposal_answered")) == 1 })
	if got := f.fromSupervisor(t, w); got != 1 || len(threadEvents(t, f.owner, a, "proposal_answered")) != 1 {
		t.Fatalf("after the other was refused: %d inputs of the supervisor", got)
	}
}

// Under each autonomy, from the supervisor's own thread and from a
// conversation, a send, an interrupt request and a proposal for the own
// thread, another conversation or itself are refused and reach nothing;
// people reach each (#871).
func TestSupervisorReachesNoThreadOfItsOwn(t *testing.T) {
	for _, mode := range []string{supervisor.Act, supervisor.Propose, supervisor.Watch} {
		t.Run(mode, func(t *testing.T) {
			f := newSvTalks(t, mode)
			ctx := context.Background()
			a, b := f.talk(t, "a"), f.talk(t, "b")
			tried := map[string]*supervisor.MCP{"the own thread": f.m, "conversation a": f.tools(t, a)}
			calls := []struct {
				tool string
				args func(id control.ThreadID) map[string]any
			}{
				{supervisor.ToolSend, func(id control.ThreadID) map[string]any {
					return map[string]any{"number": int64(id), "text": "reply sent by the supervisor"}
				}},
				{supervisor.ToolInterrupt, func(id control.ThreadID) map[string]any {
					return map[string]any{"number": int64(id), "reason": "urgent, says the supervisor"}
				}},
			}
			try := func(when string, targets ...control.ThreadID) {
				t.Helper()
				for who, m := range tried {
					for _, id := range targets {
						for _, c := range calls {
							out := mcpCall(t, m, c.tool, c.args(id))
							if !out.IsError || !strings.HasPrefix(out.Text, "not allowed") {
								t.Errorf("%s: %s to thread %d from %s: %+v; want it refused", when, c.tool, id, who, out)
							}
						}
					}
				}
			}
			all := []control.ThreadID{f.sv.ID, a, b}
			try("idle", all...)
			// Running, as an interrupt request needs.
			if err := f.owner.Send(ctx, b, "busy", control.Input{Text: "tool t 4s"}, control.Refuse); err != nil {
				t.Fatal(err)
			}
			f.busy(t, b)
			try("running", b)
			f.wt.ended(t, b, 2)
			// Waiting on a question, which a send would answer.
			if err := f.owner.Send(ctx, b, "asks", control.Input{Text: "ask which? -- one | two"}, control.Refuse); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the question", 15*time.Second, func() bool {
				d, err := f.owner.Thread(ctx, b, 0, 0)
				return err == nil && d.State == control.Waiting
			})
			try("waiting on a question", b)
			for _, id := range all {
				if n := f.fromSupervisor(t, id); n != 0 {
					t.Errorf("%d inputs of the supervisor reached thread %d", n, id)
				}
				for _, kind := range []string{"proposal_made", "approval_asked"} {
					if ev := threadEvents(t, f.owner, id, kind); len(ev) != 0 {
						t.Errorf("thread %d has %s %v", id, kind, ev)
					}
				}
			}
			// A person reaches each, in its chat thread too.
			for _, id := range all[:2] {
				if got := sendAndRead(t, f.owner, id, fmt.Sprint("p", id), "reply to a person"); !strings.Contains(got, "to a person") {
					t.Errorf("a person's send to thread %d: %q", id, got)
				}
			}
			f.wt.r.driver.Inject(f.chat("a"), fakesurface.Person(svCaller), "reply in the chat")
			f.wt.r.postedWith(t, f.chat("a"), "in the chat")
		})
	}
}

// A change of the supervisor's config reaches a conversation as it reaches
// the own thread: its idle session is put to sleep with one log line, and
// the next message resumes it with the new model and instructions (#871).
func TestConfigChangeReachesAConversation(t *testing.T) {
	record := t.TempDir()
	f := newSvTalks(t, "", recordIn(record))
	id := f.talk(t, "a")
	f.owner.Close()
	f.stop()
	svConfig(t, f.wt.r.root, map[string]any{"model": "m-2"})
	f.start(t)
	before := len(starts(t, record))
	if got := sendAndRead(t, f.owner, id, "k", "reply again"); !strings.Contains(got, "again") {
		t.Fatalf("the answer is %q", got)
	}
	var lines []string
	for _, l := range changedLines(f.log()) {
		if strings.Contains(l, fmt.Sprintf("thread=%d ", id)) {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 || !strings.HasSuffix(lines[0], "changed=model") {
		t.Fatalf("the log says %q of conversation %d", changedLines(f.log()), id)
	}
	all := starts(t, record)
	if len(all) <= before-1 {
		t.Fatalf("%d starts", len(all))
	}
	last := all[len(all)-1]
	if m, _ := last.value("--model"); m != "m-2" {
		t.Fatalf("resumed with the model %q, want m-2", m)
	}
	if _, ok := last.value("--resume"); !ok {
		t.Fatalf("not resumed: %q", last.Args)
	}
}

// With "conversations": false no conversation starts in the scope, and one
// that exists goes on; another scope's are as before (#871).
func TestConversationsTurnedOffInOneScope(t *testing.T) {
	s := supervisors(t)
	r, _ := svRoot(t, true, func(cfg map[string]any) {
		cfg["people"] = append(cfg["people"].([]map[string]any), map[string]any{"name": "sv2", "ids": []string{supervisor.ID("c2")}})
		c2 := cfg["scopes"].([]map[string]any)[1]
		c2["people"], c2["supervisor"] = []string{"dispatch", "sv2"}, map[string]any{}
		caller(cfg)
	})
	ctx := context.Background()
	var (
		stop   func()
		logged func() string
		owner  *wire.Client
	)
	start := func() {
		t.Helper()
		stop, logged = runDaemonLog(t, r.root)
		r.connected(t)
		var err error
		if owner, err = wire.Dial(r.root); err != nil {
			t.Fatal(err)
		}
		supervisorReady(t, owner, "c1")
		supervisorReady(t, owner, "c2")
	}
	start()
	defer func() { stop() }()
	defer func() { owner.Close() }()
	talk := func(scope, key string) (control.ThreadID, error) {
		return s.Load().Converse(ctx, supervisor.Call{Scope: scope, Thread: "t-" + key, Key: key, From: people.Join(fakeSurface, svCaller), Text: "reply hello"})
	}
	ended := func(id control.ThreadID) {
		t.Helper()
		eventually(t, "the conversation's first turn ended", 15*time.Second, func() bool { return len(threadEvents(t, owner, id, "turn_ended")) > 0 })
	}
	before, err := talk("c1", "before")
	if err != nil {
		t.Fatal(err)
	}
	ended(before)
	owner.Close()
	stop()
	svConfig(t, r.root, map[string]any{"conversations": false})
	start()
	threads := func() (n int) {
		dbOf(t, r.root).QueryRow(`SELECT count(*) FROM threads`).Scan(&n)
		return n
	}
	had, opened := threads(), r.openedIn("c1")
	if id, err := talk("c1", "after"); !errors.Is(err, supervisor.ErrNoConversations) || id != 0 {
		t.Fatalf("a call where they are off: thread %d, %v", id, err)
	}
	if got := threads(); got != had || r.openedIn("c1") != opened {
		t.Fatalf("a call where they are off made %d threads", got-had)
	}
	if n := strings.Count(logged(), "conversation refused"); n != 1 {
		t.Fatalf("%d log lines of the refused call, want one", n)
	}
	if got := sendAndRead(t, owner, before, "k", "mcp list_threads"); !strings.HasPrefix(got, "mcp: 2 open threads in this channel:") {
		t.Fatalf("the conversation from before answered %q", got)
	}
	other, err := talk("c2", "other")
	if err != nil {
		t.Fatalf("the other scope's: %v", err)
	}
	ended(other)
	if got := sendAndRead(t, owner, other, "k2", "mcp list_threads"); !strings.HasPrefix(got, "mcp: 2 open threads in this channel:") {
		t.Fatalf("the other scope's conversation answered %q", got)
	}
}
