package chat

import (
	"testing"

	"github.com/yousan/hataori/internal/surface"
	"github.com/yousan/hataori/internal/surface/fake"
)

func TestStoredScopeRejectsOrdersAndNonPersonButtons(t *testing.T) {
	f := newFront(t, workspaces{})
	id := f.runningThread(t)
	sc := f.core.Scopes[channel]
	sc.Unlisted = true
	f.core.Scopes[channel] = sc
	f.fe.Scopes[channel] = Scope{Enabled: true, Unlisted: true}
	var turn uint64
	if err := f.fe.Outbox.Store.DB.QueryRow(`SELECT pos FROM events WHERE thread_id=? AND kind='turn_started' ORDER BY pos DESC LIMIT 1`, int64(id)).Scan(&turn); err != nil {
		t.Fatal(err)
	}
	before := len(f.runner.all())
	f.runner.mu.Lock()
	beforeCalls := len(f.runner.calls)
	f.runner.mu.Unlock()
	f.deliver(chanConv, fake.Person(owner), "!go repo:https://example.com/r.git another")
	if len(f.runner.all()) != before {
		t.Fatal("order started in stored scope")
	}
	for _, author := range []surface.Author{fake.Bot(owner), fake.Webhook(owner)} {
		if err := f.fe.handle(f.driver.Inject(f.conv(id), author, "spoofed owner input")); err != nil {
			t.Fatal(err)
		}
		f.fe.Choice(surface.Selection{Ref: surface.MessageRef{Conv: f.conv(id), ID: "line"}, Author: author, Token: stopChoice(id, turn).Token})
	}
	f.runner.mu.Lock()
	stops := f.runner.stops
	calls := append([]string(nil), f.runner.calls...)
	f.runner.mu.Unlock()
	if stops != 0 || len(calls) != beforeCalls {
		t.Fatalf("non-person reached the agent: stops %d calls %v", stops, calls)
	}
	// The same id from a person can press the same button.
	f.fe.Choice(surface.Selection{Ref: surface.MessageRef{Conv: f.conv(id), ID: "line"}, Author: fake.Person(owner), Token: stopChoice(id, turn).Token})
	f.runner.mu.Lock()
	stops = f.runner.stops
	f.runner.mu.Unlock()
	if stops != 1 {
		t.Fatalf("owner person did not stop: %d", stops)
	}
}
