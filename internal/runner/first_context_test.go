package runner

import (
	"context"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/proctest"
)

type undeliveredContext struct {
	agent.Adapter
	used   atomic.Bool
	reason agent.Reason
}

type contextCodec struct {
	agent.Codec
	owner     *undeliveredContext
	replacing bool
}

func (a *undeliveredContext) NewCodec(s agent.Spec) agent.Codec {
	return &contextCodec{Codec: a.Adapter.NewCodec(s), owner: a, replacing: filepath.Base(s.StateDir) == "2"}
}

func (c *contextCodec) Input(in agent.Input) (agent.Actions, error) {
	if c.replacing && in.ID == firstInputID && !c.owner.used.Swap(true) {
		if c.owner.reason != "" {
			return agent.Actions{Events: []agent.Event{{Kind: agent.TurnEnded, Input: in.ID, Status: agent.Failed, Reason: c.owner.reason}}}, nil
		}
		return agent.Actions{}, nil // held until this process dies
	}
	return c.Codec.Input(in)
}

func TestReplacingFirstInputKeepsItsUnreportedContext(t *testing.T) {
	for _, failure := range []string{"not delivered", "agent died"} {
		t.Run(failure, func(t *testing.T) {
			conv := t.TempDir()
			rec := &recorder{inputs: map[string][]string{}}
			ad := &undeliveredContext{}
			if failure == "not delivered" {
				ad.reason = agent.NotDelivered
			}
			f := newFixture(t, options{conversations: conv, wrap: func(a agent.Adapter) agent.Adapter { rec.Adapter = a; ad.Adapter = rec; return ad }})
			f.start("reply hello")
			waitFor(t, "the first turn", 15*time.Second, f.has("turn_ended", 1))
			f.sleepNow(t, 1)
			files, _ := filepath.Glob(filepath.Join(conv, "*"))
			for _, p := range files {
				removeConversation(t, p)
			}
			send(t, owner(f.core), thread, "m2", "reply missed")
			waitFor(t, "the replacing input", 15*time.Second, func() bool { return ad.used.Load() })
			if failure == "agent died" {
				proctest.Kill(filepath.Join(StateDir(f.root, 2), host.PIDFile))
				waitFor(t, "asleep", 15*time.Second, func() bool { return sessionState(t, f.st, 2) == "asleep" })
			} else {
				waitFor(t, "the failed input", 15*time.Second, func() bool {
					var fi string
					f.st.DB.QueryRow(`SELECT first_input FROM sessions WHERE id=2`).Scan(&fi)
					return fi == "" && sessionState(t, f.st, 2) == "idle"
				})
			}
			send(t, owner(f.core), thread, "m3", "reply after")
			waitFor(t, "the delivered input", 15*time.Second, func() bool { return withText(t, f.st, "text", "after") == 1 })
			send(t, owner(f.core), thread, "m4", "reply last")
			waitFor(t, "the following input", 15*time.Second, func() bool { return withText(t, f.st, "text", "last") == 1 })
			if got, want := rec.of(f.root, 2), []string{helloContext + "reply after", "reply last"}; !slices.Equal(got, want) {
				t.Fatalf("inputs %q, want %q", got, want)
			}
			var owed string
			if err := f.st.DB.QueryRow(`SELECT context FROM sessions WHERE id=2`).Scan(&owed); err != nil || owed != "" {
				t.Fatalf("context %q: %v", owed, err)
			}
		})
	}
}

// Kill the daemon after the launch stores its first input, before the
// host starts. Recover must use that stored context without prepending it.
func TestRepeatedLaunchGivesTheEarlierConversationOnce(t *testing.T) {
	f := newFixture(t, options{})
	f.start("reply hello")
	waitFor(t, "the first turn", 15*time.Second, f.has("turn_ended", 1))
	f.sleepNow(t, 1)
	f.run.Close()
	if _, err := f.st.DB.Exec(`UPDATE sessions SET agent_session_id='' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	runDaemon(t, f.root, false, "die-send", "before host start", "reply after").waitKilled(t)
	rec := &recorder{inputs: map[string][]string{}}
	st, c, r, err := build(f.root, options{wrap: func(a agent.Adapter) agent.Adapter { rec.Adapter = a; return rec }}, f.log)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	defer r.Close()
	if err := r.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the recovered input", 15*time.Second, func() bool { return withText(t, st, "text", "after") == 1 })
	if got, want := rec.of(f.root, 1), []string{helloContext + "reply after"}; !slices.Equal(got, want) {
		t.Fatalf("inputs %q, want %q", got, want)
	}
}
