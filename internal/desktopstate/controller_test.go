package desktopstate

import (
	"context"
	"errors"
	"testing"
	"time"
)

type heldChild struct {
	done  chan struct{}
	stops int
}

func (c *heldChild) Stop() error           { c.stops++; return nil }
func (c *heldChild) Done() <-chan struct{} { return c.done }

func TestReplacementWaitsForOwnedChildAndRespectsStop(t *testing.T) {
	ctx := context.Background()
	desired := Selection{Intent: Running, Version: "v1", Binary: "/isolated/bin/v1/hataori"}
	var children []*heldChild
	c := Controller{
		Read: func(context.Context) (Selection, error) { return desired, nil },
		Launch: func(context.Context, Selection) (Child, error) {
			child := &heldChild{done: make(chan struct{})}
			children = append(children, child)
			return child, nil
		},
	}
	if err := c.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	desired.Version, desired.Binary = "v2", "/isolated/bin/v2/hataori"
	c.Tick(ctx)
	for range 3 {
		c.Tick(ctx)
	}
	if len(children) != 1 || children[0].stops != 1 {
		t.Fatal("replacement ran before old child joined or stopped it repeatedly")
	}
	// A stop racing the update must win once it has been persisted.
	desired.Intent = Stopped
	close(children[0].done)
	c.Tick(ctx)
	if len(children) != 1 {
		t.Fatal("update restarted a stopped instance")
	}
	desired.Intent = Running
	c.Tick(ctx)
	if len(children) != 2 || c.active.Version != "v2" {
		t.Fatal("explicit start failed to select the replacement")
	}
	close(children[1].done)
	c.Close(ctx)
}

func TestUncertainStateAndRepeatedFailuresNeverSpin(t *testing.T) {
	now := time.Unix(0, 0)
	starts := 0
	c := Controller{
		Now: func() time.Time { return now },
		Read: func(context.Context) (Selection, error) {
			return Selection{Intent: Running, Version: "v1", Binary: "/isolated/bin/v1/hataori"}, nil
		},
		Launch: func(context.Context, Selection) (Child, error) {
			starts++
			return nil, errors.New("fixture start refusal")
		},
	}
	for range 5 {
		c.Tick(context.Background())
		for range 10 {
			c.Tick(context.Background())
		}
		now = now.Add(time.Minute)
	}
	if starts != 5 || c.Tick(context.Background()) == nil {
		t.Fatalf("unbounded restart attempts: %d", starts)
	}
	c.Read = func(context.Context) (Selection, error) { return Selection{}, errors.New("unreadable intent") }
	if c.Tick(context.Background()) == nil || starts != 5 {
		t.Fatal("uncertain state admitted a child")
	}
}
