package main

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/chat"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// Configuration, rather than the available drivers or identity checks,
// decides whether each stored link's input boundary must be closed.
func TestRevokeUnservedInputsFromConfig(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &instance.Config{Scopes: []instance.Scope{{Key: "work", Client: "chat", ShowIn: []string{"other:shown"}}}}
	for i, tc := range []struct {
		scope, client, channel string
		want                   int64
	}{
		{"work", "chat", "work", 12},
		{"work", "other", "shown", 12}, // authorized even with no driver
		{"work", "other", "revoked", math.MaxInt64},
		{"gone", "chat", "gone", math.MaxInt64},
		{"work", "wrong", "work", math.MaxInt64},
		{"work", "chat", "shown", math.MaxInt64},
	} {
		id := i + 1
		if _, err := st.DB.Exec(`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at)
			VALUES (?, ?, '', 'owner', 'closed', 0, 0)`, id, tc.scope); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.Exec(`INSERT INTO links (id, thread_id, client, channel, thread, created_at, since)
			VALUES (?, ?, ?, ?, 'thread', 0, 12)`, id, id, tc.client, tc.channel); err != nil {
			t.Fatal(err)
		}
		if err := revokeUnservedInputs(ctx, st, cfg); err != nil {
			t.Fatal(err)
		}
		var got int64
		if err := st.DB.QueryRow(`SELECT since FROM links WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("%s/%s of scope %s: since %d, want %d", tc.client, tc.channel, tc.scope, got, tc.want)
		}
	}
	// Restoring permission does not reset any revoked boundary before a
	// verified front end runs its normal settlement.
	cfg.Scopes = append(cfg.Scopes, instance.Scope{Key: "gone", Client: "chat"})
	if err := revokeUnservedInputs(ctx, st, cfg); err != nil {
		t.Fatal(err)
	}
	var since int64
	if err := st.DB.QueryRow(`SELECT since FROM links WHERE id = 4`).Scan(&since); err != nil || since != math.MaxInt64 {
		t.Fatalf("restoration before verification: since %d, error %v", since, err)
	}
}

// This starts only a test-owned daemon and fake driver, with a closed
// thread and no sessions, agent processes or terminal holder. The revoked
// input boundary must be durable even if no destination front end starts.
// In particular, moving all of settleLinks before identity verification
// would incorrectly drop the pending outbox row and fail this regression.
func TestDaemonRevokesInputsBeforeDestinationStarts(t *testing.T) {
	for _, mode := range []string{"absent", "identity unavailable", "wrong identity"} {
		t.Run(mode, func(t *testing.T) {
			r := newChatRoot(t, func(cfg map[string]any) {
				if mode == "absent" {
					delete(cfg, "drivers")
				}
			})
			ctx := context.Background()
			if err := os.Mkdir(filepath.Join(r.root, "state"), 0o700); err != nil {
				t.Fatal(err)
			}
			st, err := store.Open(ctx, r.root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			if _, err := st.DB.Exec(`INSERT INTO threads (id, scope, key, started_by, state, created_at, updated_at)
				VALUES (1, 'c1', 'seed', 'owner', 'closed', 0, 0)`); err != nil {
				t.Fatal(err)
			}
			shown := surface.Conversation{Channel: "revoked", Thread: "linked-thread"}
			if _, err := st.DB.Exec(`INSERT INTO links (thread_id, client, channel, thread, created_at, since)
				VALUES (1, ?, ?, ?, 0, 12)`, fakeSurface, shown.Channel, shown.Thread); err != nil {
				t.Fatal(err)
			}
			out := &chat.Outbox{Store: st, Client: fakeSurface}
			if err := out.Write(ctx, chat.Row{Event: 73, Conv: shown, Message: surface.Message{Text: "pending before identity check"}}); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "identity unavailable":
				r.driver.Fail(fakesurface.OpIdentity, 1<<30, errors.New("identity endpoint unavailable"))
			case "wrong identity":
				r.driver.Fail(fakesurface.OpIdentity, 1<<30, surface.ErrOtherIdentity)
			}
			var log bytes.Buffer
			var mu sync.Mutex
			runCtx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			var runErr error
			go func() {
				runErr = daemon(runCtx, r.root, lockedWriter{&mu, &log})
				close(done)
			}()
			stop := func() { cancel(); <-done }
			t.Cleanup(stop)
			if mode == "wrong identity" {
				select {
				case <-done:
					if !errors.Is(runErr, surface.ErrOtherIdentity) {
						t.Fatalf("daemon: %v", runErr)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("daemon did not reject the wrong identity")
				}
			} else {
				eventually(t, "startup without a verified destination", 10*time.Second, func() bool {
					mu.Lock()
					defer mu.Unlock()
					if mode == "absent" {
						return strings.Contains(log.String(), "hataori runs")
					}
					return strings.Contains(log.String(), "its identity could not be checked")
				})
			}
			stop()
			db := r.db(t)
			var since int64
			if err := db.QueryRow(`SELECT since FROM links WHERE client = ? AND channel = ? AND thread = ?`,
				fakeSurface, shown.Channel, shown.Thread).Scan(&since); err != nil || since != math.MaxInt64 {
				t.Fatalf("revoked boundary = %d, error %v; want closed", since, err)
			}
			var state, message string
			var failures int
			if err := db.QueryRow(`SELECT state, failures, json_extract(message, '$.text') FROM chat_outbox WHERE event = 73`).Scan(&state, &failures, &message); err != nil {
				t.Fatal(err)
			}
			if state != "pending" || failures != 0 || message != "pending before identity check" {
				t.Fatalf("outbox changed before verification: state=%s failures=%d message=%q", state, failures, message)
			}
			if r.driver.Runs() != 0 || len(r.driver.Calls(c1)) != 0 || len(r.driver.Calls(shown)) != 0 {
				t.Fatal("destination made outbound calls before verification")
			}
		})
	}
}
