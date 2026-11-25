package wire

import (
	"context"
	"testing"

	"github.com/yousan/hataori/internal/control"
)

// A Start over the socket starts an ordinary thread, whatever it says of a
// kind: no operation starts a thread of another one (#867).
func TestStartOverTheSocketIsOrdinary(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	c, api := newCore(t, root)
	serve(t, root, api)
	client, err := Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	i := 0
	for _, kind := range []control.ThreadKind{control.KindSupervisor, control.KindConversation, control.KindLookup} {
		more := func(m map[string]any) map[string]any {
			for k, v := range map[string]any{"kind": kind, "Kind": kind, "confined": true, "Confined": true, "dir": root, "thread_kind": kind} {
				m[k] = v
			}
			return m
		}
		for _, key := range []string{"k", "hataori-supervisor", "hataori-supervisor-after-1"} {
			i++
			var id control.ThreadID
			err := client.call(ctx, "Start", more(map[string]any{"key": key + string(kind), "scope": "s", "thread": key + string(kind),
				"input": more(map[string]any{"text": "go"})}), &id)
			if err != nil || id == 0 {
				t.Fatalf("a start that says %q: %d, %v", kind, id, err)
			}
			d, err := client.Thread(ctx, id, 0, 0)
			if err != nil || d.Kind != control.KindOrdinary || d.Confined {
				t.Errorf("a start that says %q started a %q thread, confined %v, %v", kind, d.Kind, d.Confined, err)
			}
			if got, err := c.Kind(ctx, id); err != nil || got != control.KindOrdinary {
				t.Errorf("a start that says %q stored a %q thread, %v", kind, got, err)
			}
		}
	}
	if id, _, err := c.SupervisorThread(ctx, "s"); err != nil || id != 0 {
		t.Errorf("the scope's supervisor thread is %d, %v after %d starts over the socket", id, err, i)
	}
}
