package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// archiveRoot is a chat root whose scope c1 has archive_when_asleep set to
// on, with an idle period of idle and the fake agent in a terminal, so
// that `hataori attach` can wake its threads.
func archiveRoot(t *testing.T, on bool, idle string) *chatRoot {
	t.Helper()
	conv := filepath.Join(t.TempDir(), "conversations")
	os.MkdirAll(conv, 0o700)
	return newChatRoot(t, func(cfg map[string]any) {
		cfg["idle"] = idle
		cfg["scopes"].([]map[string]any)[0]["archive_when_asleep"] = on
		cfg["adapters"] = map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"},
			Env: []string{"PATH=" + os.Getenv("PATH")}, Conversations: conv, Terminal: true, Hataori: os.Args[0]}}
	})
}

// setThreads are the archived flags SetThread set on th, in order.
func (r *chatRoot) setThreads(th surface.Conversation) []bool {
	var out []bool
	for _, c := range r.driver.Calls(th) {
		if c.Op == fakesurface.OpSetThread {
			out = append(out, c.Thread.Archived)
		}
	}
	return out
}

// asleepAndArchived waits until th's thread is asleep and its chat thread
// archived, and checks that the archive came after every post: nothing
// is posted into an archived thread.
func (r *chatRoot) asleepAndArchived(t *testing.T, th surface.Conversation) {
	t.Helper()
	eventually(t, "the thread asleep", r.within(20*time.Second), func() bool { return r.state(t, th) == string(control.Asleep) })
	eventually(t, "the chat thread archived", r.within(10*time.Second), func() bool { return r.driver.Archived(th) })
	calls := r.driver.Calls(th)
	if last := calls[len(calls)-1]; last.Op != fakesurface.OpSetThread || !last.Thread.Archived || last.Thread.Title != "" {
		t.Fatalf("the last call in the chat thread is %+v, want the archive, after the posts and leaving the title", last)
	}
}

// unarchivedFirst checks that the chat thread was un-archived by the wake
// that posted text, before that post.
func (r *chatRoot) unarchivedFirst(t *testing.T, th surface.Conversation, text string) {
	t.Helper()
	post := r.posted(t, th, text)
	calls := r.driver.Calls(th)
	unarchive, at := -1, -1
	for i, c := range calls {
		if c.Op == fakesurface.OpSetThread && !c.Thread.Archived && at < 0 {
			unarchive = i
		}
		if c.Op == fakesurface.OpPost && c.Ref == post.Ref {
			at = i
		}
		if c.Op == fakesurface.OpSetThread && c.Thread.Archived && at < 0 {
			unarchive = -1 // only an un-archive after the last archive counts
		}
	}
	if unarchive < 0 || unarchive > at {
		t.Fatalf("no un-archive before %q: calls %+v", text, calls)
	}
}

// The criteria of #636 with the option on: a thread that goes asleep has
// its chat thread archived; a message there un-archives it, as the chat
// service does, and resumes the thread, also when it came while the driver
// was not connected; a wake from the dashboard's way (Send through the
// control API, without a link) and one by `hataori attach` un-archive it
// before anything is posted. A closed thread: TestArchiveWhenClosed.
func TestArchiveWhenAsleep(t *testing.T) {
	r := archiveRoot(t, true, "1s")
	startDaemon(t, r.root)
	r.connected(t)
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" reply a")
	th := r.thread(t, 0)
	r.posted(t, th, "a")
	r.asleepAndArchived(t, th)

	// A message in the archived chat thread: the service un-archives it,
	// and the thread resumes.
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply b")
	r.posted(t, th, "b")
	r.asleepAndArchived(t, th)

	// The same while the driver is disconnected: caught up on reconnect.
	r.driver.Disconnect()
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply c")
	r.driver.Reconnect()
	r.posted(t, th, "c")
	r.asleepAndArchived(t, th)

	// Woken as the dashboard wakes it: no message in the chat thread.
	num, _ := strconv.ParseInt(numberOf(t, r.root, th), 10, 64)
	c, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Send(ctx, control.ThreadID(num), "dash-1", control.Input{Text: "reply d"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	r.unarchivedFirst(t, th, "d")
	if r.driver.Archived(th) && r.state(t, th) != string(control.Asleep) {
		t.Fatal("the woken thread's chat thread is archived")
	}
	r.asleepAndArchived(t, th)

	// Woken by `hataori attach`.
	n := len(r.setThreads(th))
	a := attach(t, r.root, numberOf(t, r.root, th))
	eventually(t, "the attached thread un-archived", r.within(15*time.Second), func() bool {
		s := r.setThreads(th)
		return len(s) > n && !s[n]
	})
	eventually(t, "the attached thread awake", r.within(15*time.Second), func() bool { return r.state(t, th) == string(control.Idle) })
	a.detach(t)
	r.asleepAndArchived(t, th)
}

// A thread closed while it is idle has its chat thread archived.
func TestArchiveWhenClosed(t *testing.T) {
	r := archiveRoot(t, true, "1h")
	startDaemon(t, r.root)
	r.connected(t)
	th, _, _ := r.idleThread(t, 0, "a")
	if s := r.setThreads(th); len(s) != 0 {
		t.Fatalf("set-thread calls %v while the thread is idle", s)
	}
	if code, _, errOut := cli(t, "close", "--root", r.root, numberOf(t, r.root, th)); code != 0 {
		t.Fatalf("close: %d %s", code, errOut)
	}
	eventually(t, "the chat thread archived", r.within(10*time.Second), func() bool { return r.driver.Archived(th) })
}

// With the option off (the default), and with the option on but a driver
// that cannot set a thread's flags, an asleep thread's chat thread is left
// open.
func TestArchiveWhenAsleepLeftOpen(t *testing.T) {
	for _, tc := range []struct {
		name     string
		on       bool
		noThread bool
	}{
		{name: "option off"},
		{name: "driver without SetThread", on: true, noThread: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := archiveRoot(t, tc.on, "1s")
			if tc.noThread {
				l := fakesurface.DefaultLimits
				l.SetThread = false
				r.driver.SetLimits(l)
			}
			startDaemon(t, r.root)
			r.connected(t)
			r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" reply a")
			th := r.thread(t, 0)
			r.posted(t, th, "a")
			eventually(t, "the thread asleep", r.within(20*time.Second), func() bool { return r.state(t, th) == string(control.Asleep) })
			r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply b")
			r.posted(t, th, "b")
			eventually(t, "the thread asleep again", r.within(20*time.Second), func() bool { return r.state(t, th) == string(control.Asleep) })
			time.Sleep(500 * time.Millisecond) // what the front end would write for it
			if s := r.setThreads(th); len(s) != 0 {
				t.Fatalf("set-thread calls %v, want none", s)
			}
		})
	}
}

// An archive that fails is logged once, not tried again, and changes
// nothing else: the thread is asleep, and the next message resumes it.
func TestArchiveFailureIsLoggedOnce(t *testing.T) {
	r := archiveRoot(t, true, "1s")
	r.driver.Fail(fakesurface.OpSetThread, 1, nil) // a second try would succeed
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" reply a")
	th := r.thread(t, 0)
	r.posted(t, th, "a")
	const line = `msg="chat outbox: archive of a chat thread failed"`
	eventually(t, "the failure logged", r.within(20*time.Second), func() bool { return strings.Contains(logged(), line) })
	time.Sleep(6 * time.Second) // past the outbox's retry period
	if n := strings.Count(logged(), line); n != 1 || r.driver.Archived(th) || len(r.setThreads(th)) != 0 {
		t.Fatalf("the failure logged %d times, archived %v:\n%s", n, r.driver.Archived(th), logged())
	}
	if s := r.state(t, th); s != string(control.Asleep) {
		t.Fatalf("state %s after the failed archive", s)
	}
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply b")
	r.posted(t, th, "b")
}

// A thread shown in a channel of another client (show_in) has that chat
// thread archived too: the option is the thread's scope's, whichever client
// shows it.
func TestArchiveWhenAsleepOnASecondSurface(t *testing.T) {
	var two *fakesurface.Driver
	r := newChatRoot(t, func(cfg map[string]any) {
		root := cfg["drivers"].(map[string]any)[fakeSurface].(map[string]any)["id"].(string)
		two = fakesurface.New()
		drivers.Store(root+"/two", two)
		t.Cleanup(func() { drivers.Delete(root + "/two") })
		cfg["owner"] = "ada"
		cfg["people"] = []map[string]any{
			{"name": "ada", "ids": []string{fakeSurface + ":" + e2eOwner}},
			{"name": "dispatch", "ids": []string{fakeSurface + ":" + e2eHook}},
			{"name": "grace", "ids": []string{secondSurface + ":g-1"}},
		}
		cfg["scopes"] = []map[string]any{
			{"key": "c1", "client": fakeSurface, "enabled": true, "people": []string{"dispatch", "grace"},
				"show_in": []string{secondSurface + ":d1"}, "archive_when_asleep": true},
			{"key": "d1", "client": secondSurface, "enabled": true},
		}
		cfg["drivers"].(map[string]any)[secondSurface] = map[string]any{"id": root + "/two"}
	})
	startDaemon(t, r.root)
	r.connected(t)
	eventually(t, "the second driver to connect", 10*time.Second, two.Connected)
	time.Sleep(20 * time.Millisecond)
	th, _, _ := r.idleThread(t, 0, "hello")
	d1 := surface.Conversation{Channel: "d1"}
	two.InjectCommand(d1, fakesurface.Person("g-1"), "attach", surface.Arg{Name: "number", Value: numberOf(t, r.root, th)})
	var there surface.Conversation
	eventually(t, "the attached thread", 15*time.Second, func() bool {
		o := opened(two, d1)
		if len(o) == 1 {
			there = o[0]
		}
		return len(o) == 1
	})
	if code, _, errOut := cli(t, "close", "--root", r.root, numberOf(t, r.root, th)); code != 0 {
		t.Fatalf("close: %d %s", code, errOut)
	}
	eventually(t, "both chat threads archived", 10*time.Second, func() bool { return r.driver.Archived(th) && two.Archived(there) })
}
