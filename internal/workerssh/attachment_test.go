package workerssh

import (
	"testing"

	"github.com/yousan/hataori/internal/workerproto"
)

func TestAttachmentInventoryIgnoresOtherRunsWithoutAdoptingThem(t *testing.T) {
	target := workerproto.Target{Thread: 1, Session: "s-1", Run: "run-1"}
	other := workerproto.Target{Thread: 2, Session: "s-2", Run: "run-2"}
	before := workerproto.Inventory{Complete: true, Hosts: []workerproto.HostRecord{{Target: target, State: "running"}}}
	after := before
	after.Hosts = append(after.Hosts, workerproto.HostRecord{Target: other, State: "running"})
	for _, inventory := range []workerproto.Inventory{before, after} {
		if err := checkScopedInventory(nil, &target, inventory); err != nil {
			t.Fatalf("unrelated start disconnected attachment: %v", err)
		}
	}
	if err := checkScopedInventory([]workerproto.Target{target}, nil, after); err == nil {
		t.Fatal("main adopted an unregistered run")
	}
	if err := checkScopedInventory(nil, &target, workerproto.Inventory{Complete: true, Hosts: []workerproto.HostRecord{{Target: other, State: "running"}}}); err == nil {
		t.Fatal("another run replaced the attachment target")
	}
}
