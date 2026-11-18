package main

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/wire"
)

// A question with choices from the fake agent, in a terminal and on pipes,
// reaches the control API's Request.choices and the request event in
// order; one without has none; answering with a choice ends the question,
// and the agent's last text names the choice.
func TestQuestionWithChoicesThroughTheCore(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint("terminal=", terminal), func(t *testing.T) {
			root := newRoot(t)
			if terminal {
				root = terminalRoot(t)
			}
			runDaemon(t, root)
			c, err := wire.Dial(root)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			repo := bareRepo(t)
			for _, tc := range []struct {
				key, script, question string
				choices               []string
				answer                control.Answer
				reply                 string
			}{
				{"t1", "ask Which colour? -- red | green | blue", "Which colour?", []string{"red", "green", "blue"},
					control.Answer{Choices: []string{"green"}}, "answer: green"},
				{"t2", "ask Which colour?", "Which colour?", nil, control.Answer{Text: "teal"}, "answer: teal"},
			} {
				id, err := c.Start(ctx, control.StartRequest{Key: "k-" + tc.key, Scope: "s", Thread: tc.key, Repo: repo, Input: control.Input{Text: tc.script}})
				if err != nil {
					t.Fatal(err)
				}
				var d control.ThreadDetail
				eventually(t, "the question", 20*time.Second, func() bool {
					d, err = c.Thread(ctx, id, 0, 0)
					return err == nil && d.State == control.Waiting && len(d.Requests) == 1
				})
				r := d.Requests[0]
				var want []control.Choice
				for _, ch := range tc.choices {
					want = append(want, control.Choice{Label: ch, Token: ch})
				}
				if r.Text != tc.question || !reflect.DeepEqual(r.Choices, want) {
					t.Fatalf("%q: Request %q with choices %+v, want %q with %+v", tc.script, r.Text, r.Choices, tc.question, want)
				}
				// The request event carries the same choices, in order.
				var event []any
				found := false
				_, out, _ := cli(t, "events", "--root", root)
				for _, e := range lines(t, out) {
					if th, _ := e["thread"].(map[string]any); e["kind"] == "request" && th["key"] == tc.key {
						data := e["data"].(map[string]any)
						event, _ = data["choices"].([]any)
						found = data["text"] == tc.question
					}
				}
				var got []string
				for _, ch := range event {
					got = append(got, ch.(string))
				}
				if !found || !reflect.DeepEqual(got, tc.choices) {
					t.Fatalf("%q: the request event has choices %q (found %v), want %q", tc.script, got, found, tc.choices)
				}
				if err := c.Answer(ctx, control.RequestID(r.ID), tc.answer); err != nil {
					t.Fatal(err)
				}
				eventually(t, "the answered turn to end", 20*time.Second, func() bool {
					d, err = c.Thread(ctx, id, 0, 0)
					return err == nil && d.State == control.Idle && d.Last == tc.reply
				})
			}
		})
	}
}
