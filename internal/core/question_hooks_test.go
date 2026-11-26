package core

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
)

type changingQuestionHooks struct {
	run func(control.HookCall) control.HookResult
}

func (h changingQuestionHooks) Run(_ context.Context, c control.HookCall) control.HookResult {
	return h.run(c)
}

func TestQuestionClosingDuringHooksRunsTheMissingPromptBuild(t *testing.T) {
	for _, change := range []string{"answered", "withdrawn", "still open"} {
		for _, result := range []string{"allow", "stop", "block failure"} {
			t.Run(change+"/"+result, func(t *testing.T) {
				ctx := context.Background()
				f := newFixture(t, fakeOwner{})
				id, sid := f.running(t, "t1")
				f.agentEvent(t, sid, 2, agent.Event{Kind: agent.Request, Request: &agent.RequestInfo{ID: "r1", Text: "which?"}})
				before := len(f.runner.Calls())
				var points []string
				f.core.Hooks = changingQuestionHooks{run: func(c control.HookCall) control.HookResult {
					points = append(points, c.Point)
					if c.Point == PointBeforeAgentRun {
						if change == "answered" {
							var handle string
							if err := f.core.Store.DB.QueryRow(`SELECT handle FROM requests WHERE request='r1'`).Scan(&handle); err != nil {
								t.Fatal(err)
							}
							if err := f.owner.Answer(ctx, control.RequestID(handle), control.Answer{Text: "elsewhere"}); err != nil {
								t.Fatal(err)
							}
						} else if change == "withdrawn" {
							f.agentEvent(t, sid, 3, agent.Event{Kind: agent.RequestWithdrawn, Request: &agent.RequestInfo{ID: "r1"}})
						}
					}
					return control.HookResult{Text: c.Text, Before: "prefix", After: "suffix", Stopped: c.Point == PointBeforePromptBuild && result != "allow", Failed: result == "block failure", By: "test"}
				}}
				err := f.owner.Send(ctx, id, "m1", control.Input{Text: "message"}, control.Deliver)
				blocked := change != "still open" && result != "allow"
				var stopped *control.Stopped
				if blocked {
					if !errors.As(err, &stopped) || stopped.Point != PointBeforePromptBuild || stopped.Failed != (result == "block failure") {
						t.Fatalf("send %v, want prompt-build stop", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				want := []string{PointBeforeAgentRun}
				if change != "still open" {
					want = append(want, PointBeforePromptBuild)
				}
				if !slices.Equal(points, want) {
					t.Fatalf("points %q, want %q", points, want)
				}
				var taken int
				if err := f.core.Store.DB.QueryRow(`SELECT count(*) FROM inbox WHERE key=?`, "send:s\x00m1").Scan(&taken); err != nil {
					t.Fatal(err)
				}
				if blocked {
					if taken != 0 || strings.Contains(f.runner.Calls()[before:], "deliver ") {
						t.Fatalf("blocked input was taken: inbox %d, calls %s", taken, f.runner.Calls())
					}
				} else {
					if taken != 1 {
						t.Fatalf("accepted key count %d", taken)
					}
					if change != "still open" && !strings.Contains(f.runner.Calls(), "deliver prefix\nmessage\nsuffix") {
						t.Fatalf("calls %s", f.runner.Calls())
					}
					if err := f.owner.Send(ctx, id, "m1", control.Input{Text: "message"}, control.Deliver); err != nil || !slices.Equal(points, want) {
						t.Fatalf("duplicate send %v; points %q", err, points)
					}
				}
			})
		}
	}
}
