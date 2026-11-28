package main

import (
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// postedWith waits for a post in conv whose text has all of parts, and
// returns it.
func (r *chatRoot) postedWith(t *testing.T, conv surface.Conversation, parts ...string) fakesurface.Call {
	t.Helper()
	var got fakesurface.Call
	eventually(t, "a post with "+strings.Join(parts, ", "), r.within(15*time.Second), func() bool {
		for _, c := range r.driver.Calls(conv) {
			if c.Op != fakesurface.OpPost {
				continue
			}
			all := true
			for _, p := range parts {
				all = all && strings.Contains(c.Message.Text, p)
			}
			if all {
				got = c
				return true
			}
		}
		return false
	})
	return got
}

// commandRun drives the chat commands end to end, as commands or as words
// after the trigger: the owner's start opens a thread with a first reply,
// stop ends a running turn within 10 seconds, and status answers in the
// thread and in the channel.
func commandRun(t *testing.T, words bool) {
	r := newChatRoot(t, nil)
	if !words {
		l := fakesurface.DefaultLimits
		l.Commands = true
		r.driver.SetLimits(l)
	}
	r.deadline = time.Now().Add(90 * time.Second)
	startDaemon(t, r.root)
	r.connected(t)
	owner := fakesurface.Person(e2eOwner)
	do := func(conv surface.Conversation, name, text string, args ...surface.Arg) {
		if words {
			r.driver.Inject(conv, owner, text)
		} else {
			r.driver.InjectCommand(conv, owner, name, args...)
		}
	}
	repo := bareRepo(t)
	do(c1, "new", "!go new model:m1 repo:"+repo+" reply hello",
		surface.Arg{Name: "repository", Value: repo}, surface.Arg{Name: "prompt", Value: "reply hello"},
		surface.Arg{Name: "model", Value: "m1"})
	th := r.thread(t, 0)
	r.posted(t, th, "reply hello")
	r.posted(t, th, "hello")
	n := numberOf(t, r.root, th)
	if !words {
		r.postedWith(t, c1, "Started thread #"+n+".")
	}

	// A turn that runs for a minute, stopped.
	r.driver.Inject(th, owner, "tool wait 60s\nreply never")
	eventually(t, "the tool", r.within(15*time.Second), func() bool { return len(r.events(t, "tool_started", th)) == 1 })
	start := time.Now()
	do(th, "stop", "!go stop")
	eventually(t, "the end of the turn", r.within(10*time.Second), func() bool { return len(r.events(t, "turn_ended", th)) == 2 })
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the turn ended %v after the stop", took)
	}
	if e := r.events(t, "turn_ended", th)[1]; e["reason"] != "interrupted" {
		t.Fatalf("the second turn ended %v, want interrupted", e)
	}
	r.postedWith(t, th, "Stopped the running turn of thread #"+n+".")
	if posts(r.driver, th, "never") != 0 {
		t.Fatalf("the stopped turn went on")
	}

	eventually(t, "the thread to be idle", r.within(15*time.Second), func() bool { return r.state(t, th) == "idle" })
	do(th, "status", "!go status")
	r.postedWith(t, th, "Thread #"+n+": idle", "Model: m1", "Last: hello")
	do(c1, "status", "!go status")
	r.postedWith(t, c1, "Threads here:", "#"+n+" idle on local: reply hello")
}

func TestCommandsStartStopAndShowStatus(t *testing.T) { commandRun(t, false) }

// On a surface without commands, the same words after the trigger.
func TestTheSameWordsAfterTheTriggerOnASurfaceWithoutCommands(t *testing.T) {
	commandRun(t, true)
}
