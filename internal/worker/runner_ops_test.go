package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/workerproto"
)

func runningSource(t *testing.T) (*fixture, workerproto.Source, workerproto.Bytes) {
	t.Helper()
	f := newFixture(t, false)
	f.prepare()
	f.receipt(f.startRequest())
	f.waitReady()
	reply := f.call(f.request("follow", "", &f.target, workerproto.Follow{Source: "events"}))
	if reply.Kind != "reply" {
		t.Fatalf("follow: %s", reply.Body)
	}
	var s workerproto.Source
	json.Unmarshal(reply.Body, &s)
	reply = f.call(f.request("read", "", &f.target, workerproto.Read{Source: s, Order: 1, Limit: workerproto.MaxChunk}))
	var b workerproto.Bytes
	json.Unmarshal(reply.Body, &b)
	if b.State != "data" || len(b.Data) == 0 {
		t.Fatalf("actual fake output: %s", reply.Body)
	}
	return f, s, b
}

func readRequest(f *fixture, s workerproto.Source, off uint64, limit uint32) workerproto.Message {
	return f.request("read", "", &f.target, workerproto.Read{Source: s, Order: 7, Offset: off, Limit: limit})
}

func TestRetainedHistoryAndSourceFailures(t *testing.T) {
	f, s, first := runningSource(t)
	p := filepath.Join(f.r.hostDir(f.target), host.EventsFile)
	if err := os.Rename(p, p+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	var got workerproto.Bytes
	reply := f.call(readRequest(f, s, 0, uint32(len(first.Data))))
	json.Unmarshal(reply.Body, &got)
	if got.State != "data" || string(got.Data) != string(first.Data) {
		t.Fatalf("retained data changed: %s", reply.Body)
	}
	reply = f.call(readRequest(f, s, first.Next, 1))
	json.Unmarshal(reply.Body, &got)
	if got.State != "replaced" || got.Next != first.Next || len(got.Data) != 0 {
		t.Fatalf("replacement: %s", reply.Body)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p+".original", p); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, 0); err != nil {
		t.Fatal(err)
	}
	reply = f.call(readRequest(f, s, first.Next, 1))
	json.Unmarshal(reply.Body, &got)
	if got.State != "truncated" {
		t.Fatalf("truncation: %s", reply.Body)
	}
	sr := f.r.state.Sources[targetKey(f.target)+"-events"]
	if err := f.r.root.Remove(f.r.historyPath(sr)); err != nil {
		t.Fatal(err)
	}
	reply = f.call(readRequest(f, s, 0, 1))
	json.Unmarshal(reply.Body, &got)
	if got.State != "history_lost" {
		t.Fatalf("missing retained history: %s", reply.Body)
	}
}

func TestFollowAndReadRequireCurrentRunMarker(t *testing.T) {
	f, s, _ := runningSource(t)
	p := filepath.Join(f.r.hostDir(f.target), "owner.json")
	original, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(p, original, 0600); err != nil {
			t.Error(err)
		}
	})
	if err := os.WriteFile(p, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	refused(t, f.call(f.request("follow", "", &f.target, workerproto.Follow{Source: "events"})), "wrong_target")
	refused(t, f.call(readRequest(f, s, 0, 1)), "wrong_target")
}

func TestFollowRejectsUnreportedFilesAndCrossRunSources(t *testing.T) {
	f, s, _ := runningSource(t)
	refused(t, f.call(f.request("follow", "", &f.target, workerproto.Follow{Source: workerproto.FileSource(filepath.Join(f.root, "config/worker.json"))})), "denied")
	other := workerproto.Target{Thread: 8, Session: "other-session", Run: "other-run"}
	f.receipt(f.request("prepare", "other-prepare", &other, workerproto.Prepare{}))
	m := f.request("read", "", &other, workerproto.Read{Source: s, Order: 1, Limit: 1})
	refused(t, f.call(m), "wrong_target")
}

func TestReportedFIFORefusesWithoutBlockingResident(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(p, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := openSource(p)
		if f != nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("source open blocked")
	}
}

func TestSourceJournalRejectsMalformedRetention(t *testing.T) {
	f, s, _ := runningSource(t)
	k := targetKey(f.target) + "-events"
	record := f.r.state.Sources[k]
	for _, bad := range []func(){func() { f.r.state.Sources[k] = nil }, func() { record.Size = -1 }, func() { record.Source.Handle = "../outside" }, func() { record.Target.Thread = 99 }} {
		copy := *record
		f.r.state.Sources[k] = record
		bad()
		if f.r.validateJournal() == nil {
			t.Fatal("invalid source journal accepted")
		}
		*record = copy
		f.r.state.Sources[k] = record
	}
	if record.Source != s {
		t.Fatal("fixture not restored")
	}
}
