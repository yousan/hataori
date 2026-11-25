package main

import (
	"slices"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// The acceptance criteria of #799: in a supervisor's thread nothing is
// posted of a turn whose message mentions another account and not
// hataori's, when its final answer is "(quiet)"; with any other answer, and
// for any other message, a turn is posted whole. A question such a turn
// asks is posted.
func TestSupervisorIsQuietToAMessageToSomeoneElse(t *testing.T) {
	wt := newWakeTest(t)
	stop, _, sv := wt.start(t)
	defer stop()
	conv := surface.Conversation{Channel: sv.Links[0].Channel, Thread: sv.Links[0].Thread}
	person := fakesurface.Person(e2eOwner)
	wt.r.posted(t, conv, "done") // the answer to its first message
	turns := wt.turns(t, sv.ID)
	seen := len(postsIn(wt.r.driver, conv))
	const other, own = "<@another> look", "<@" + fakesurface.Self + "> look"
	for _, c := range []struct {
		name, text string
		want       []string
	}{
		{"quiet to others", other + "\nreply thinking\nreply (quiet)", nil},
		{"an answer to others", other + "\nreply first\nreply a correction", []string{"first", "a correction"}},
		{"mentions both", own + " <@another>\nreply second\nreply (quiet)", []string{"second", "(quiet)"}},
		{"mentions hataori", own + "\nreply third\nreply (quiet)", []string{"third", "(quiet)"}},
		{"mentions nobody", "look\nreply fourth\nreply (quiet)", []string{"fourth", "(quiet)"}},
	} {
		wt.r.driver.Inject(conv, person, c.text)
		turns++
		pos := wt.ended(t, sv.ID, turns)
		eventually(t, "the front end reads the turn's end", 20*time.Second, func() bool {
			var cursor, pending uint64
			err := wt.db.QueryRow(`SELECT pos, (SELECT count(*) FROM chat_outbox WHERE state = 'pending') FROM chat_cursors WHERE client = ?`,
				sv.Links[0].Client).Scan(&cursor, &pending)
			return err == nil && cursor >= pos && pending == 0
		})
		var got []string
		p := postsIn(wt.r.driver, conv)
		for _, m := range p[seen:] {
			got = append(got, m.Message.Text)
		}
		seen = len(p)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: posted %q, want %q", c.name, got, c.want)
		}
	}

	// The question is asked, though the text before it waited.
	wt.r.driver.Inject(conv, person, other+"\nreply before\nreply still before\nask Which one?\nreply (quiet)")
	wt.r.posted(t, conv, "before")
	wt.r.posted(t, conv, "Which one?")
}
