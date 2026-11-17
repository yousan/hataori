package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/host"
)

// A turn the agent starts by itself before it reports the first input does
// not count as that report: the input is kept, delivered once after a
// restart, and not again once the agent reported it.
func TestFirstInputConfirmedOnlyByItsID(t *testing.T) {
	recorders := []*recorder{}
	opts := func() options {
		rec := &recorder{inputs: map[string][]string{}}
		recorders = append(recorders, rec)
		// The fake never reports anything: the test writes its lines.
		return options{instructions: "never-start", wrap: func(a agent.Adapter) agent.Adapter { rec.Adapter = a; return rec }}
	}
	f := newFixture(t, opts())
	f.start("reply hi")
	events := filepath.Join(StateDir(f.root, 1), host.EventsFile)
	write := func(line string) {
		t.Helper()
		lf, err := os.OpenFile(events, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		lf.WriteString(line + "\n")
		lf.Close()
		// Read and committed.
		waitFor(t, "the line read", 10*time.Second, func() bool {
			var off int64
			f.st.DB.QueryRow(`SELECT log_offset FROM sessions WHERE id = 1`).Scan(&off)
			st, _ := os.Stat(events)
			return st != nil && off == st.Size()
		})
	}
	firstInput := func() string {
		var fi string
		f.st.DB.QueryRow(`SELECT first_input FROM sessions WHERE id = 1`).Scan(&fi)
		return fi
	}
	handed := func(r *recorder) int { return len(r.of(f.root, 1)) }

	write(`{"t":"ready","session":"s1"}`)
	waitFor(t, "the first input handed over", 10*time.Second, func() bool { return handed(recorders[0]) == 1 })
	if ids := recorders[0].idsOf(f.root, 1); len(ids) != 1 || ids[0] != firstInputID {
		t.Fatalf("the first input was handed over with ids %q, want %q", ids, firstInputID)
	}
	write(`{"t":"turn","by":"agent"}`)
	if firstInput() == "" {
		t.Fatal("a turn the agent started by itself confirmed the first input")
	}

	restart := func() *recorder {
		t.Helper()
		f.run.Close()
		st, _, r, err := build(f.root, opts(), f.log)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close(); st.Close() })
		f.run = r
		if err := r.Reconnect(context.Background()); err != nil {
			t.Fatal(err)
		}
		return recorders[len(recorders)-1]
	}
	rec := restart()
	waitFor(t, "the first input handed over again", 10*time.Second, func() bool { return handed(rec) == 1 })
	if ids := rec.idsOf(f.root, 1); len(ids) != 1 || ids[0] != firstInputID {
		t.Fatalf("the first input was handed over again with ids %q, want %q", ids, firstInputID)
	}
	write(`{"t":"turn","by":"caller","input":"` + firstInputID + `"}`)
	if fi := firstInput(); fi != "" {
		t.Fatalf("the first input is kept after its report: %s", fi)
	}

	rec = restart()
	time.Sleep(300 * time.Millisecond) // several polls
	if n := handed(rec); n != 0 {
		t.Fatalf("the reported first input was handed over %d more times", n)
	}
	if n := counts(t, f.st)["input"]; n != 1 {
		t.Fatalf("%d input events, want 1", n)
	}
}
