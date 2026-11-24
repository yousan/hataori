package main

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/chat"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// What a thread is told of its conversation when a choice starts it again
// (#710), with the fake agent of TestTheChoicesOfABlockedStart:
//
//   - a new thread, which had no conversation, is told nothing, with each
//     of the two choices that start it;
//   - a thread that ran a turn, slept, and was blocked as a message woke it
//     is told once, before its answer, that it starts again in a new
//     conversation, with each of the two choices; and it does: the agent's
//     earlier conversation is not resumed, in the same directory.
func TestWhatAThreadIsToldWhenAChoiceStartsItAgain(t *testing.T) {
	const without, trust = "Start without this repository's settings", "Trust and start"
	r := newReopenRoot(t, func(cfg map[string]any) {
		cfg["idle"] = "2s"
		c := cfg["adapters"].(map[string]any)[fake.Name].(fake.Config)
		c.Terminal, c.Hataori, c.ProjectHook = true, os.Args[0], true
		c.Dialog, c.DialogTrust = "Do you trust the files in this folder? Enter to go on", []string{".claude/settings.json"}
		cfg["adapters"] = map[string]any{fake.Name: c}
	})

	// choose presses the button label of th's nth message with choices.
	choose := func(th surface.Conversation, nth int, label string) {
		t.Helper()
		var msg fakesurface.Call
		eventually(t, "the choices", r.within(20*time.Second), func() bool {
			n := 0
			for _, c := range r.driver.Calls(th) {
				if c.Op == fakesurface.OpPost && len(c.Message.Choices) == 3 {
					if msg = c; n == nth {
						return true
					}
					n++
				}
			}
			return false
		})
		if err := r.driver.Press(msg.Ref, e2eOwner, choiceToken(t, msg, label)); err != nil {
			t.Fatal(err)
		}
	}
	// told is the posts in th that say something of its conversation.
	told := func(th surface.Conversation) (lines []string) {
		for _, c := range r.driver.Calls(th) {
			if c.Op == fakesurface.OpPost && strings.Contains(c.Message.Text, "conversation") {
				lines = append(lines, c.Message.Text)
			}
		}
		return lines
	}
	// start starts the nth thread with the choice label.
	start := func(nth int, label string) surface.Conversation {
		t.Helper()
		r.driver.Inject(c1, fakesurface.Webhook(e2eHook), "!go repo:"+bareRepo(t)+" reply first")
		th := r.thread(t, nth)
		choose(th, 0, label)
		r.posted(t, th, "first")
		if got := told(th); len(got) != 0 {
			t.Fatalf("a new thread started with %q was told %q", label, got)
		}
		if ev := r.events(t, "session_replaced", th); len(ev) != 1 {
			t.Fatalf("session_replaced %v: the check above proves nothing", ev)
		}
		return th
	}

	untrust := func() {
		t.Helper()
		var tf struct{ ID int64 }
		code, out, errOut := cli(t, "trusts", "--root", r.root)
		if code != 0 || json.Unmarshal([]byte(strings.TrimSpace(out)), &tf) != nil {
			t.Fatalf("hataori trusts: %d %q %q", code, out, errOut)
		}
		if code, out, errOut := cli(t, "untrust", strconv.FormatInt(tf.ID, 10), "--root", r.root); code != 0 {
			t.Fatalf("hataori untrust: %d %q %q", code, out, errOut)
		}
	}

	start(0, without)
	for i, label := range []string{trust, without} {
		th := start(1+i, trust)
		eventually(t, "the thread to sleep", r.within(20*time.Second), func() bool { return r.state(t, th) == "asleep" })
		untrust()
		r.driver.Inject(th, fakesurface.Webhook(e2eHook), "reply again")
		choose(th, 1, label)
		r.posted(t, th, "again")
		const line = "The thread starts again in a new conversation, in the same directory."
		if got := told(th); len(got) != 1 || got[0] != line || line != chat.StartedAgainLine {
			t.Fatalf("a woken thread started with %q was told %q, want %q once", label, got, line)
		}
		if !r.before(t, th, line, "again") {
			t.Fatal("the line is not before the new conversation's first answer")
		}
		// The line is true: the new conversation's first turn is not the old
		// one's, and its directory is.
		ss := r.starts(t)
		if last := ss[len(ss)-1]; last.resumes() || !sameDir(last.Dir, r.workspace(t, th)) {
			t.Fatalf("the agent started in %s with %v", last.Dir, last.Args)
		}
		r.driver.Inject(th, fakesurface.Webhook(e2eHook), "recall")
		if got := r.postedWith(t, th, "recall: ").Message.Text; strings.HasPrefix(got, "recall: reply first") || got == "recall: nothing" {
			t.Fatalf("the agent recalls %q: no new conversation", got)
		}
		if label == trust {
			untrust() // the next thread's is then the only one again
		}
	}
}
