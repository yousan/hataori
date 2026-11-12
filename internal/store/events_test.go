package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
)

func openStore(t *testing.T, root string) *Store {
	t.Helper()
	s, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func appendEvents(t *testing.T, s *Store, scope string, n int) {
	t.Helper()
	err := s.Update(context.Background(), func(tx *Tx) error {
		for i := range n {
			if _, err := tx.Append(context.Background(), control.Event{
				Thread: control.ThreadID{Scope: scope, Key: "k"}, Kind: "test", Data: []byte(fmt.Sprintf(`{"i":%d}`, i)),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAcceptOnce(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, newRoot(t))
	results := make(chan bool, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.Update(ctx, func(tx *Tx) error {
				ok, err := tx.Accept(ctx, "msg-1", []byte("x"))
				results <- ok
				return err
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	close(results)
	accepted := 0
	for ok := range results {
		if ok {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("%d of 2 calls with one key were accepted", accepted)
	}
	var rows int
	s.DB.QueryRow(`SELECT count(*) FROM inbox`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("inbox has %d rows", rows)
	}
}

func TestFollowWhileWritersAppend(t *testing.T) {
	const total, start = 10000, 100
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := openStore(t, newRoot(t))
	appendEvents(t, s, "a", start)

	out := make(chan control.Event)
	done := make(chan error, 1)
	go func() { done <- s.Follow(ctx, control.Filter{}, start, out) }()

	// Two writers, in transactions of 50 events each.
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range total / 2 / 50 {
				appendEvents(t, s, "a", 50)
			}
		}()
	}
	want := uint64(start)
	timeout := time.After(60 * time.Second)
	for range total {
		select {
		case e := <-out:
			if e.Pos != want+1 {
				t.Fatalf("got position %d after %d", e.Pos, want)
			}
			want = e.Pos
		case <-timeout:
			t.Fatalf("follower stopped at %d", want)
		}
	}
	wg.Wait()
	select {
	case e := <-out:
		t.Fatalf("extra event %d", e.Pos)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Follow: %v", err)
	}
}

func TestEventsAfterRestart(t *testing.T) {
	ctx := context.Background()
	root := newRoot(t)
	s, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	appendEvents(t, s, "a", 20)
	before, err := s.Events(ctx, control.Filter{}, 7, 100)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	s = openStore(t, root)
	after, err := s.Events(ctx, control.Filter{}, 7, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 13 || fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("before restart %d events, after %d; differ: %v", len(before), len(after), fmt.Sprint(before) != fmt.Sprint(after))
	}
}

func TestFollowByScope(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := openStore(t, newRoot(t))
	out := make(chan control.Event, 100)
	go s.Follow(ctx, control.Filter{Scope: "a"}, 0, out)
	appendEvents(t, s, "b", 5)
	appendEvents(t, s, "a", 3)
	appendEvents(t, s, "b", 5)
	for range 3 {
		if e := <-out; e.Thread.Scope != "a" {
			t.Fatalf("follower of scope a got scope %q", e.Thread.Scope)
		}
	}
	evs, _ := s.Events(ctx, control.Filter{Scope: "a"}, 0, 100)
	if len(evs) != 3 {
		t.Fatalf("Events of scope a: %d", len(evs))
	}
	select {
	case e := <-out:
		t.Fatalf("follower of scope a got %+v", e)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestRolledBackEventIsNeverSeen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := openStore(t, newRoot(t))
	out := make(chan control.Event, 10)
	go s.Follow(ctx, control.Filter{}, 0, out)

	err := s.Update(ctx, func(tx *Tx) error {
		if _, err := tx.Append(ctx, control.Event{Thread: control.ThreadID{Scope: "a", Key: "k"}, Kind: "rolled-back"}); err != nil {
			return err
		}
		return errors.New("injected failure")
	})
	if err == nil {
		t.Fatal("Update succeeded")
	}
	appendEvents(t, s, "a", 1)
	if e := <-out; e.Kind != "test" {
		t.Fatalf("follower saw %q", e.Kind)
	}
	evs, _ := s.Events(ctx, control.Filter{}, 0, 100)
	if len(evs) != 1 || evs[0].Kind != "test" {
		t.Fatalf("log holds %v", evs)
	}
}
