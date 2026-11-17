package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// fakeSurface is the name the tests register the fake surface driver under.
// The daemon makes a driver from the config's "drivers" section; the
// fake's settings name the driver a test made, {"id": ...}.
const fakeSurface = "fake-surface"

var (
	drivers sync.Map // id → *fakesurface.Driver
)

func fakeDriver(_ *os.Root, settings []byte) (surface.Driver, error) {
	var s struct{ ID string }
	json.Unmarshal(settings, &s)
	d, ok := drivers.Load(s.ID)
	if !ok {
		return nil, errors.New("no fake surface driver " + s.ID)
	}
	return d.(*fakesurface.Driver), nil
}

// chatRoot is an instance root whose config has the fake agent and the fake
// surface, with one enabled channel c1 that hook-1 may post in.
type chatRoot struct {
	root   string
	driver *fakesurface.Driver
}

const (
	e2eOwner = "owner-1"
	e2eHook  = "hook-1"
)

var c1 = surface.Conversation{Channel: "c1"}

func newChatRoot(t *testing.T, change func(cfg map[string]any)) *chatRoot {
	t.Helper()
	root := t.TempDir()
	d := fakesurface.New()
	drivers.Store(t.Name(), d)
	t.Cleanup(func() { drivers.Delete(t.Name()) })
	cfg := map[string]any{
		"owner": e2eOwner, "trigger": "!go", "idle": "1h",
		"scopes":   []map[string]any{{"key": "c1", "client": fakeSurface, "enabled": true, "posters": []string{e2eHook}}},
		"drivers":  map[string]any{fakeSurface: map[string]any{"id": t.Name()}},
		"adapters": map[string]any{fake.Name: fake.Config{Command: []string{os.Args[0], "fake"}, Env: []string{"PATH=" + os.Getenv("PATH")}}},
	}
	if change != nil {
		change(cfg)
	}
	b, _ := json.Marshal(cfg)
	os.MkdirAll(filepath.Join(root, "config"), 0o700)
	if err := os.WriteFile(filepath.Join(root, instance.ConfigFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return &chatRoot{root: root, driver: d}
}

func eventually(t *testing.T, what string, timeout time.Duration, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(timeout); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// connected waits until the daemon's front end has connected the driver.
func (r *chatRoot) connected(t *testing.T) {
	t.Helper()
	eventually(t, "the driver to connect", 10*time.Second, r.driver.Connected)
	time.Sleep(20 * time.Millisecond) // the first catch-up marks the channel as read from now
}

// thread returns the n-th thread opened in c1, once it is there.
func (r *chatRoot) thread(t *testing.T, n int) surface.Conversation {
	t.Helper()
	var th surface.Conversation
	eventually(t, "a thread", 15*time.Second, func() bool {
		i := 0
		for _, c := range r.driver.Calls(c1) {
			if c.Op == fakesurface.OpOpenThread {
				if i == n {
					th = c.Opened
					return true
				}
				i++
			}
		}
		return false
	})
	return th
}

// posted waits for a post with text in conv and returns it.
func (r *chatRoot) posted(t *testing.T, conv surface.Conversation, text string) fakesurface.Call {
	t.Helper()
	var got fakesurface.Call
	eventually(t, "a post of "+text, 15*time.Second, func() bool {
		for _, c := range r.driver.Calls(conv) {
			if c.Op == fakesurface.OpPost && c.Message.Text == text {
				got = c
				return true
			}
		}
		return false
	})
	return got
}

// events returns the instance's events, as `hataori events` prints them.
func (r *chatRoot) events(t *testing.T, kind string, thread surface.Conversation) []map[string]any {
	t.Helper()
	_, out, _ := cli(t, "events", "--root", r.root)
	var got []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var e struct {
			Kind   string
			Thread struct{ Scope, Key string }
			Data   map[string]any
		}
		if json.Unmarshal([]byte(l), &e) == nil && e.Kind == kind && e.Thread.Key == thread.Thread {
			got = append(got, e.Data)
		}
	}
	return got
}

// The first end-to-end test: a trigger posted through the fake surface
// opens a thread, the fake agent's final answer is posted there, and the
// turn's end event is written.
func TestOneThreadOneTurn(t *testing.T) {
	start := time.Now()
	r := newChatRoot(t, nil)
	startDaemon(t, r.root)
	r.connected(t)
	r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" reply hello there")
	th := r.thread(t, 0)
	prompt := r.posted(t, th, "reply hello there")
	answer := r.posted(t, th, "hello there")
	if answer.Message.ReplyTo == nil || *answer.Message.ReplyTo != prompt.Ref {
		t.Fatalf("answer %+v, want a reply to the prompt %+v", answer.Message, prompt.Ref)
	}
	var ended []map[string]any
	eventually(t, "the end event", 15*time.Second, func() bool {
		ended = r.events(t, "turn_ended", th)
		return len(ended) > 0
	})
	if len(ended) != 1 || ended[0]["outcome"] != "completion" {
		t.Fatalf("turn_ended %v", ended)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("took %v", took)
	}
}

// The fakes are for tests: the binary has none of them.
func TestTheBinaryHasNoFakes(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skip("go list:", err)
	}
	for _, p := range strings.Fields(string(out)) {
		if strings.HasSuffix(p, "/fake") || strings.Contains(p, "/fake/") {
			t.Errorf("the binary depends on %s", p)
		}
	}
}
