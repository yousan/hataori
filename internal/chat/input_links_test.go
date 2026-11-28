package chat

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

// A destination need not have a running front end for its input permission
// to lapse. Settlement after restoration must not turn that period's
// messages into input, while another link keeps its ordinary backlog.
func TestRevokedInputsStayRevokedUntilVerifiedRestoration(t *testing.T) {
	f := newFront(t, workspaces{}, func(fe *FrontEnd) {
		fe.ShowIn = map[string][]string{"c9": {channel}}
	})
	id := f.runningThread(t)
	main := f.conv(id)
	shown := f.shownIn(t, id, "c9")
	ctx := context.Background()
	st := f.fe.Outbox.Store
	settleInput := func() {
		t.Helper()
		if err := RevokeUnservedInputs(ctx, st, func(client, ch, scope string) bool {
			return client == f.fe.Name && f.fe.serves(ch, scope)
		}); err != nil {
			t.Fatal(err)
		}
	}
	since := func() int64 {
		t.Helper()
		var n int64
		if err := st.DB.QueryRow(`SELECT since FROM links WHERE client = ? AND channel = ? AND thread = ?`,
			f.fe.Name, shown.Channel, shown.Thread).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	postedAt := time.Now().Add(time.Minute)
	post := func(conv surface.Conversation, text string, at time.Time) {
		f.driver.InjectWith(conv, fake.Person(owner), text, func(in *surface.Inbound) { in.Time = at })
	}
	f.fe.ShowIn = nil
	settleInput() // daemon startup, with no destination front end
	post(shown, "revoked input", postedAt)
	post(main, "ordinary backlog", postedAt)
	settleInput() // another startup during the same lapse

	f.fe.ShowIn = map[string][]string{"c9": {channel}}
	settleInput() // configuration alone cannot reopen the boundary
	if n := since(); n != neverSince {
		t.Errorf("input boundary before verified settlement = %d, want revoked", n)
	}
	restoredAt := postedAt.Add(time.Minute)
	f.fe.in, f.fe.inboundOnce = nil, sync.Once{}
	f.fe.inboundState().since = restoredAt.UnixMilli()
	if err := f.fe.settleLinks(ctx); err != nil {
		t.Fatal(err)
	}
	post(shown, "new input one", restoredAt.Add(time.Second))
	post(shown, "new input two", restoredAt.Add(2*time.Second))
	f.fe.catchUp(ctx, main)
	f.fe.catchUp(ctx, shown)
	want := []string{"deliver first input", "deliver ordinary backlog", "deliver new input one", "deliver new input two"}
	if got := f.runner.handed(); !slices.Equal(got, want) {
		t.Fatalf("handed over %q, want %q", got, want)
	}

	// A rebuilt front end uses the persisted boundary and cursors.
	f.fe.in, f.fe.inboundOnce = nil, sync.Once{}
	settleInput()
	if err := f.fe.settleLinks(ctx); err != nil {
		t.Fatal(err)
	}
	f.fe.catchUp(ctx, main)
	f.fe.catchUp(ctx, shown)
	if got := f.runner.handed(); !slices.Equal(got, want) {
		t.Fatalf("after restart handed over %q, want %q", got, want)
	}
}
