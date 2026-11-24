package core

import (
	"context"
	"testing"

	"github.com/yousan/hataori/internal/control"
)

// An input to a session whose agent is still to be told the thread's earlier
// conversation carries it, unless the input the agent was woken for, not
// reported yet, does (#579).
func TestInputCarriesTheEarlierConversationOnce(t *testing.T) {
	for first, want := range map[string]string{"": "CTX", `{"text":"wake up","context":"CTX","sent":true}`: ""} {
		f := newFixture(t, fakeOwner{})
		id, sid := f.idle(t, "k")
		if _, err := f.core.Store.DB.Exec(`DELETE FROM pending_inputs`); err != nil {
			t.Fatal(err)
		}
		if _, err := f.core.Store.DB.Exec(`UPDATE sessions SET context = 'CTX', first_input = ? WHERE id = ?`, first, sid); err != nil {
			t.Fatal(err)
		}
		if err := f.owner.Send(context.Background(), id, "m", control.Input{Text: "go on"}, control.Deliver); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := f.core.Store.DB.QueryRow(`SELECT context FROM pending_inputs`).Scan(&got); err != nil || got != want {
			t.Errorf("first input %q: carries %q (%v), want %q", first, got, err, want)
		}
	}
}
