package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/plugin"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// The criteria of the plugin phase (design, Appendix E, Plugins; #354),
// each with the example plugin of examples/plugins/marker, started by the
// daemon, with the fake surface and the fake agent. Where a test needs two
// plugins, the example runs under two ids, each from a copy of its
// directory with its manifest changed.

// exampleAs copies the example into a directory of its own, with its
// manifest's id set to id and then changed by change, and returns the
// executable and its SHA-256.
func exampleAs(t *testing.T, id string, change func(m map[string]any)) (string, string) {
	t.Helper()
	src, _ := examplePlugin(t)
	dir := filepath.Join(t.TempDir(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, id)
	if err := os.WriteFile(exe, b, 0o755); err != nil {
		t.Fatal(err)
	}
	mb, err := os.ReadFile(filepath.Join(filepath.Dir(src), plugin.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(mb, &m); err != nil {
		t.Fatal(err)
	}
	m["id"] = id
	if change != nil {
		change(m)
	}
	mb, _ = json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, plugin.ManifestFile), mb, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return exe, hex.EncodeToString(sum[:])
}

// entry is a config entry for plugin id at exe in c1, with the
// conversation grant, settings, and more keys of the entry.
func entry(id, exe, sum string, settings map[string]any, more map[string]any) map[string]any {
	e := map[string]any{"id": id, "path": exe, "sha256": sum, "scopes": []string{"c1"}, "conversation": true}
	if settings != nil {
		e["settings"] = settings
	}
	for k, v := range more {
		e[k] = v
	}
	return e
}

// withPlugins is a chat root with the plugins' entries in its config.
func withPlugins(t *testing.T, entries ...map[string]any) *chatRoot {
	t.Helper()
	return newChatRoot(t, func(cfg map[string]any) { cfg["plugins"] = entries })
}

// started waits until each of the plugins has connected and called
// Threads (or had it refused).
func started(t *testing.T, logged func() string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		eventually(t, "plugin "+id+" to start", 20*time.Second, func() bool {
			return strings.Contains(logged(), "plugin="+id+" stream=stdout line=\"marker: threads")
		})
	}
}

// stateFile is the path of plugin id's file name in its state directory.
func stateFile(root, id, name string) string {
	return filepath.Join(root, "state", "plugins", id, name)
}

// recordedEvents are the positions and kinds in plugin id's events.jsonl,
// in the order it handled them.
func recordedEvents(t *testing.T, root, id string) [][2]string {
	t.Helper()
	f, err := os.Open(stateFile(root, id, "events.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out [][2]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e struct {
			Pos  uint64
			Kind string
		}
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, [2]string{strconv.FormatUint(e.Pos, 10), e.Kind})
		}
	}
	return out
}

// eventsOf are the positions and kinds of thread id's events of kinds, as
// the owner reads them, oldest first.
func eventsOf(t *testing.T, c control.API, id control.ThreadID, kinds []string) [][2]string {
	t.Helper()
	evs, err := c.ThreadEvents(context.Background(), id, 0, control.MaxEvents)
	if err != nil {
		t.Fatal(err)
	}
	var out [][2]string
	for _, e := range evs {
		if slices.Contains(kinds, string(e.Kind)) {
			out = append(out, [2]string{strconv.FormatUint(e.Pos, 10), string(e.Kind)})
		}
	}
	return out
}

var exampleKinds = []string{"thread_created", "turn_started", "input", "text", "turn_ended", "post_sent"}

// ownerStart starts a thread in c1 as the owner, with script.
func ownerStart(t *testing.T, owner *wire.Client, key, script string) control.ThreadID {
	t.Helper()
	id, err := owner.Start(context.Background(), control.StartRequest{Key: key, Scope: "c1", Thread: "t-" + key, Repo: bareRepo(t),
		Input: control.Input{Text: script}})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func turnsEnded(t *testing.T, owner control.API, id control.ThreadID, n int) {
	t.Helper()
	eventually(t, fmt.Sprintf("%d turn ends of thread %d", n, id), 30*time.Second, func() bool {
		return len(eventsOf(t, owner, id, []string{"turn_ended"})) >= n
	})
}

// handledOnce checks that plugin id handled each event of thread th of the
// example's kinds exactly once, in order.
func handledOnce(t *testing.T, root, id string, owner control.API, th control.ThreadID) {
	t.Helper()
	var want, got [][2]string
	eventually(t, "plugin "+id+" to handle the thread's events", 20*time.Second, func() bool {
		want = eventsOf(t, owner, th, exampleKinds)
		got = nil
		for _, e := range recordedEvents(t, root, id) {
			if slices.Contains(want, e) {
				got = append(got, e)
			}
		}
		return len(got) >= len(want)
	})
	if !slices.Equal(got, want) {
		t.Fatalf("plugin %s handled\n%v\nwant each once, in order:\n%v", id, got, want)
	}
	seen := map[string]bool{}
	for _, e := range recordedEvents(t, root, id) {
		if seen[e[0]] {
			t.Fatalf("plugin %s handled the event at %s twice", id, e[0])
		}
		seen[e[0]] = true
	}
}

// The plugin connects, reads events from a position and calls Threads: a
// second run reads after the position the first kept, handling no event
// twice and missing none. A call to an operation its grant lacks is
// refused and logged.
func TestPluginReadsFromItsPositionAndIsRefusedWhatItsGrantLacks(t *testing.T) {
	exe, sum := exampleAs(t, "marker", nil)
	nexe, nsum := exampleAs(t, "narrow", nil)
	r := withPlugins(t, entry("marker", exe, sum, map[string]any{"record": true}, nil),
		entry("narrow", nexe, nsum, nil, map[string]any{"operations": []string{"Events"}}))
	stop, logged := runDaemonLog(t, r.root)
	r.connected(t)
	started(t, logged, "marker", "narrow")
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	a := ownerStart(t, owner, "a", "reply first")
	turnsEnded(t, owner, a, 1)
	handledOnce(t, r.root, "marker", owner, a)
	if !strings.Contains(logged(), "marker: threads 0") {
		t.Fatalf("the first run's Threads:\n%s", logged())
	}
	// One line names the operation, the scope and the plugin.
	refused := slices.ContainsFunc(strings.Split(logged(), "\n"), func(l string) bool {
		return strings.Contains(l, `msg="control call refused" op=Threads scope=""`) && strings.Contains(l, "from=plugin:narrow")
	})
	if !refused || !strings.Contains(logged(), "plugin=narrow stream=stdout line=\"marker: threads refused: forbidden\"") {
		t.Fatalf("the refused Threads:\n%s", logged())
	}
	owner.Close()
	stop()
	// Where the next run starts: after the position kept, or the last event
	// recorded, the later of the two, read once the plugin has stopped.
	kept, err := os.ReadFile(stateFile(r.root, "marker", "position"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := strconv.ParseUint(strings.TrimSpace(string(kept)), 10, 64)
	if err != nil || after == 0 {
		t.Fatalf("the kept position %q: %v", kept, err)
	}
	for _, e := range recordedEvents(t, r.root, "marker") {
		n, _ := strconv.ParseUint(e[0], 10, 64)
		after = max(after, n)
	}

	stop, logged = runDaemonLog(t, r.root)
	defer stop()
	r.connected(t)
	// Both: input waits for every before_agent_run handler to be connected.
	started(t, logged, "marker", "narrow")
	eventually(t, "the second run to read after the kept position", 20*time.Second, func() bool {
		return strings.Contains(logged(), "plugin=marker stream=stdout line=\"marker: reading events after "+strconv.FormatUint(after, 10)+"\"")
	})
	if !strings.Contains(logged(), "plugin=marker stream=stdout line=\"marker: threads 1\"") {
		t.Fatalf("the second run did not see the thread:\n%s", logged())
	}
	owner, err = wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := owner.Send(context.Background(), a, "s1", control.Input{Text: "reply second"}, control.Refuse); err != nil {
		t.Fatal(err)
	}
	turnsEnded(t, owner, a, 2)
	// Once it has read every stored event, its whole record has each event
	// once: nothing before the position was read again.
	eventually(t, "the second run to catch up", 20*time.Second, func() bool {
		return strings.Contains(logged(), "plugin=marker stream=stdout line=\"marker: caught up\"")
	})
	handledOnce(t, r.root, "marker", owner, a)
}

// A plugin is not started when the hash of its executable differs from the
// config, and it is refused when its protocol major version differs; each
// log line names both values.
func TestPluginNotStartedForItsHashOrItsProtocol(t *testing.T) {
	exe, sum := exampleAs(t, "badhash", nil)
	pexe, psum := exampleAs(t, "oldproto", func(m map[string]any) { m["protocol"] = "2.0" })
	wrong := strings.Repeat("0", 64)
	r := withPlugins(t, entry("badhash", exe, wrong, nil, nil), entry("oldproto", pexe, psum, nil, nil))
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	line := func(id string) string {
		var l string
		eventually(t, "the line of "+id, 10*time.Second, func() bool {
			for _, x := range strings.Split(logged(), "\n") {
				if strings.Contains(x, "plugin not started") && strings.Contains(x, "plugin="+id) {
					l = x
					return true
				}
			}
			return false
		})
		return l
	}
	if l := line("badhash"); !strings.Contains(l, "expected="+wrong) || !strings.Contains(l, "actual="+sum) {
		t.Fatalf("the hash's line: %s", l)
	}
	if l := line("oldproto"); !strings.Contains(l, "needs protocol 2.0, the daemon speaks "+wire.Version) {
		t.Fatalf("the protocol's line: %s", l)
	}
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(logged(), "plugin=badhash stream=") || strings.Contains(logged(), "plugin=oldproto stream=") {
		t.Fatalf("a refused plugin ran:\n%s", logged())
	}
}

var startedPID = regexp.MustCompile(`msg="plugin started" plugin=marker pid=([0-9]+)`)

// A plugin killed during a turn is started again, the turn finishes, and
// the plugin reads each event it missed exactly once.
func TestPluginKilledDuringATurn(t *testing.T) {
	exe, sum := exampleAs(t, "marker", nil)
	r := withPlugins(t, entry("marker", exe, sum, map[string]any{"record": true}, nil))
	// Down longer than the turn takes: the turn ends while the plugin is
	// not running.
	runnerHook = func(pr *plugin.Runner) { pr.Backoff = 4 * time.Second }
	t.Cleanup(func() { runnerHook = nil })
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	started(t, logged, "marker")
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	id := ownerStart(t, owner, "k", "tool sleep 1s\nreply after the kill")
	eventually(t, "the plugin to read the turn's start", 20*time.Second, func() bool {
		return slices.ContainsFunc(recordedEvents(t, r.root, "marker"), func(e [2]string) bool { return e[1] == "turn_started" })
	})
	pids := startedPID.FindAllStringSubmatch(logged(), -1)
	pid, _ := strconv.Atoi(pids[len(pids)-1][1])
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	turnsEnded(t, owner, id, 1)
	if n := len(startedPID.FindAllStringSubmatch(logged(), -1)); n != len(pids) {
		t.Fatalf("the plugin started again before the turn ended: %d starts", n)
	}
	ended := eventsOf(t, owner, id, []string{"turn_ended"})[0]
	if slices.Contains(recordedEvents(t, r.root, "marker"), ended) {
		t.Fatal("the plugin read the turn's end while it was killed")
	}
	eventually(t, "the plugin to start again", 20*time.Second, func() bool {
		return len(startedPID.FindAllStringSubmatch(logged(), -1)) == len(pids)+1
	})
	if s := threadEvents(t, owner, id, "turn_ended"); s[0]["outcome"] != "completion" {
		t.Fatalf("the turn's end: %v", s[0])
	}
	handledOnce(t, r.root, "marker", owner, id)
}

// A message_sending handler rewrites one post and cancels another, and a
// before_agent_run handler stops an input before it reaches the agent; the
// thread names the plugin and the reason.
func TestPluginRewritesCancelsAndStops(t *testing.T) {
	exe, sum := exampleAs(t, "marker", nil)
	r := withPlugins(t, entry("marker", exe, sum, nil, nil))
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	started(t, logged, "marker")
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" reply hello")
	r.posted(t, th, "hello")
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply a REWRITE-ME b")
	r.posted(t, th, "a [rewritten] b")
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply CANCEL-ME")
	stopped := r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply STOP-ME")
	if n := r.repliesTo(t, th, stopped.Ref, "Not sent to the agent: the plugin marker stopped it: the input holds STOP-ME"); n != 1 {
		t.Fatalf("%d lines for the stopped input", n)
	}
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply after")
	r.posted(t, th, "after")
	if r.postsWith(th, "REWRITE-ME") != 0 || r.postsWith(th, "CANCEL-ME") != 0 {
		t.Fatal("the original or the cancelled post went out")
	}
	for _, d := range r.events(t, "input", th) {
		if strings.Contains(fmt.Sprint(d["text"]), "STOP-ME") {
			t.Fatalf("the stopped input reached the agent: %v", d)
		}
	}
	c := r.cancelledOf(t, th)
	if len(c) != 1 || c[0].Plugin != "marker" || c[0].Reason != "the post holds CANCEL-ME" {
		t.Fatalf("hataori thread shows %+v", c)
	}
}

// Two handlers on one point run by priority, then in config order: each
// rewrites ORDER and puts its own tag after it, so the post shows the
// order, the last one's tag first.
func TestPluginHandlersRunByPriorityThenConfigOrder(t *testing.T) {
	for _, tc := range []struct {
		name               string
		firstPri, otherPri float64
		want               string
	}{
		// The config lists second before first, against their ids' order.
		{"config order", 0, 0, "ORDER-first-second"},
		{"priority", 10, 0, "ORDER-second-first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hooks := func(pri float64) func(m map[string]any) {
				return func(m map[string]any) {
					m["hooks"] = []map[string]any{{"point": "message_sending", "priority": pri}}
				}
			}
			fexe, fsum := exampleAs(t, "first", hooks(tc.firstPri))
			sexe, ssum := exampleAs(t, "second", hooks(tc.otherPri))
			r := withPlugins(t, entry("second", sexe, ssum, map[string]any{"rewrite": "ORDER", "with": "ORDER-second"}, nil),
				entry("first", fexe, fsum, map[string]any{"rewrite": "ORDER", "with": "ORDER-first"}, nil))
			logged := startDaemonLog(t, r.root)
			r.connected(t)
			started(t, logged, "first", "second")
			th := r.order(t, 0, "!go repo:"+bareRepo(t)+" reply hello")
			r.posted(t, th, "hello")
			r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply ORDER")
			r.posted(t, th, tc.want)
		})
	}
}

// A handler that times out gets its point's default failure policy, and a
// per-plugin override in the config replaces it: before_agent_run blocks
// unless the override skips, message_sending skips unless it blocks. The
// gate's plugin is slow for SLOWIN, the outbox's for SLOWOUT.
func TestPluginTimeoutPolicies(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "override"}[override], func(t *testing.T) {
			gexe, gsum := exampleAs(t, "gate", func(m map[string]any) {
				m["hooks"] = []map[string]any{{"point": "before_agent_run", "timeout": "1s"}}
			})
			oexe, osum := exampleAs(t, "outbox", func(m map[string]any) {
				m["hooks"] = []map[string]any{{"point": "message_sending", "timeout": "1s"}}
			})
			var gmore, omore map[string]any
			if override {
				gmore = map[string]any{"hooks": []map[string]any{{"point": "before_agent_run", "on_failure": "skip"}}}
				omore = map[string]any{"hooks": []map[string]any{{"point": "message_sending", "on_failure": "block"}}}
			}
			r := withPlugins(t, entry("gate", gexe, gsum, map[string]any{"slow": "SLOWIN", "slow_seconds": 4}, gmore),
				entry("outbox", oexe, osum, map[string]any{"slow": "SLOWOUT", "slow_seconds": 4}, omore))
			logged := startDaemonLog(t, r.root)
			r.connected(t)
			started(t, logged, "gate", "outbox")
			th := r.order(t, 0, "!go repo:"+bareRepo(t)+" reply hello")
			r.posted(t, th, "hello")

			in := r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply SLOWIN")
			if override {
				r.posted(t, th, "SLOWIN") // skipped: the input went on
			} else if n := r.repliesTo(t, th, in.Ref, "Not sent to the agent: "+
				"the plugin gate failed (it did not answer within 1s), and its policy blocks the input."); n != 1 {
				t.Fatalf("%d lines for the blocked input", n)
			}

			r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply SLOWOUT")
			r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply after")
			r.posted(t, th, "after")
			c := r.cancelledOf(t, th)
			if !override {
				if r.count(th, "SLOWOUT") != 1 || len(c) != 0 {
					t.Fatalf("skipped: %d posts of SLOWOUT, cancelled %+v", r.count(th, "SLOWOUT"), c)
				}
				return
			}
			if r.count(th, "SLOWOUT") != 0 || len(c) != 1 || c[0].Plugin != "outbox" || !c[0].Failed ||
				!strings.Contains(c[0].Reason, "did not answer within 1s") {
				t.Fatalf("blocked: %d posts of SLOWOUT, cancelled %+v", r.count(th, "SLOWOUT"), c)
			}
		})
	}
}

// Without the conversation grant nothing sent to the plugin contains
// message text: a marker in the input, the agent's text and a post never
// reaches it, in a hook call or an event, while it is sent both.
func TestPluginWithoutConversationGetsNoText(t *testing.T) {
	const marker = "MARKER-9f31"
	exe, sum := exampleAs(t, "marker", nil)
	r := withPlugins(t, entry("marker", exe, sum, map[string]any{"record": true}, map[string]any{"conversation": false}))
	runnerHook = func(pr *plugin.Runner) { pr.Backoff = 100 * time.Millisecond }
	t.Cleanup(func() { runnerHook = nil })
	logged := startDaemonLog(t, r.root)
	r.connected(t)
	started(t, logged, "marker")
	// The thread's title, the first line of its first prompt, holds the
	// marker too.
	th := r.order(t, 0, "!go repo:"+bareRepo(t)+" reply "+marker)
	r.posted(t, th, marker) // the agent's text, and a post
	r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply also "+marker)
	r.posted(t, th, "also "+marker)
	// Started again, it calls Threads, which lists the thread.
	pids := startedPID.FindAllStringSubmatch(logged(), -1)
	pid, _ := strconv.Atoi(pids[len(pids)-1][1])
	syscall.Kill(pid, syscall.SIGKILL)
	eventually(t, "Threads to list the thread", 20*time.Second, func() bool {
		return strings.Contains(logged(), "plugin=marker stream=stdout line=\"marker: threads 1\"")
	})
	// Up to the thread's last event, as the owner reads it.
	n, _ := strconv.Atoi(numberOf(t, r.root, th))
	owner, err := wire.Dial(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	all := eventsOf(t, owner, control.ThreadID(n), exampleKinds)
	last := all[len(all)-1]
	var got string
	eventually(t, "the plugin to be sent the hooks and every event", 20*time.Second, func() bool {
		if !slices.Contains(recordedEvents(t, r.root, "marker"), last) {
			return false
		}
		b, _ := os.ReadFile(stateFile(r.root, "marker", "received.jsonl"))
		got = string(b)
		for _, s := range []string{`"op": "hook"`, `"point": "before_agent_run"`, `"point": "message_sending"`,
			`"kind": "input"`, `"kind": "text"`, `"kind": "post_sent"`} {
			if !strings.Contains(got, s) {
				return false
			}
		}
		return true
	})
	if strings.Contains(got, marker) {
		t.Fatalf("the plugin was sent the marker:\n%s", got)
	}
}
