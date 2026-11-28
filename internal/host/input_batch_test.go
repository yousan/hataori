package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
)

func TestInputBatchExplicitRefusalDiffersFromLostReply(t *testing.T) {
	h := &host{run: "fixture-run", done: make(chan struct{}), input: inputWriterFunc(func(b []byte) (int, error) { t.Fatal("refused batch wrote bytes"); return 0, nil }), batches: map[string]InputReceipt{}}
	for n := 0; n < MaxInputBatches; n++ {
		h.batches[string(rune(n+1))] = InputReceipt{State: "written", Digest: "old"}
	}
	c := batchPeer(t, h)
	_, err := c.InputBatch(InputBatch{Run: h.run, Key: "new-key", Data: [][]byte{[]byte("never written")}})
	var refused *InputBatchRefused
	if !errors.As(err, &refused) {
		t.Fatalf("explicit capacity refusal was ambiguous: %v", err)
	}
	got, err := c.InputReceipt(h.run, "new-key")
	if err != nil || got.State != "absent" {
		t.Fatal(got, err)
	}
	c.Close()
	_, err = c.InputBatch(InputBatch{Run: h.run, Key: "new-key", Data: [][]byte{[]byte("never written")}})
	if err == nil || errors.As(err, &refused) {
		t.Fatalf("connection loss was called a received refusal: %v", err)
	}
}

func batchPeer(t *testing.T, h *host) *Client {
	t.Helper()
	s, c := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	go func() {
		defer s.Close()
		d, e := json.NewDecoder(s), json.NewEncoder(s)
		for {
			var r request
			if d.Decode(&r) != nil {
				return
			}
			if h.reply(s, e, r) != nil {
				return
			}
		}
	}()
	return &Client{conn: c, dec: json.NewDecoder(c)}
}

func TestInputBatchSurvivesLostReplyAndConcurrentRetry(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var got bytes.Buffer
	h := &host{run: "fixture-run", done: make(chan struct{})}
	h.input = inputWriterFunc(func(b []byte) (int, error) {
		once.Do(func() { close(entered); <-release })
		return got.Write(b)
	})
	c := batchPeer(t, h)
	cap, err := c.InputCapability()
	if err != nil || cap.Run != h.run {
		t.Fatalf("capability: %+v %v", cap, err)
	}
	b := InputBatch{Run: cap.Run, Key: "deferred-fixture", Data: [][]byte{[]byte("complete "), []byte("input\n")}}
	done := make(chan struct{})
	go func() { c.InputBatch(b); close(done) }()
	<-entered
	c.Close() // the daemon loses the reply while its receiver continues
	other := batchPeer(t, h)
	r, err := other.InputBatch(b)
	if err != nil || r.State != "pending" {
		t.Fatalf("in-flight retry: %+v %v", r, err)
	}
	lookup, err := other.InputReceipt(b.Run, b.Key)
	if err != nil || lookup.State != "pending" {
		t.Fatalf("in-flight lookup: %+v %v", lookup, err)
	}
	close(release)
	<-done
	// Reading the cached completion synchronizes with the entire write.
	r, err = other.InputBatch(b)
	if err != nil || r.State != "written" || r.CompletedAt == 0 {
		t.Fatalf("reconnected: %+v %v", r, err)
	}
	if got.String() != "complete input\n" {
		t.Fatalf("actual bytes: %q", got.String())
	}
	for range 2 {
		fresh := batchPeer(t, h)
		r, err = fresh.InputBatch(b)
		if err != nil || r.State != "written" {
			t.Fatalf("second reconstruction: %+v %v", r, err)
		}
	}
	if got.String() != "complete input\n" {
		t.Fatalf("duplicate actual input: %q", got.String())
	}
	b.Data = [][]byte{[]byte("different")}
	if _, err := other.InputBatch(b); err == nil {
		t.Fatal("changed payload reused key")
	}
	b.Run = "replacement-run"
	if _, err := other.InputBatch(b); err == nil {
		t.Fatal("replacement run took input")
	}
}

func TestInputBatchPartialWriteNeverRepeatsPrefix(t *testing.T) {
	writes := 0
	h := &host{run: "fixture-run", done: make(chan struct{})}
	h.input = inputWriterFunc(func(b []byte) (int, error) { writes++; return 1, nil })
	c := batchPeer(t, h)
	b := InputBatch{Run: h.run, Key: "partial", Data: [][]byte{[]byte("first"), []byte("never")}}
	for range 3 {
		r, err := c.InputBatch(b)
		if err != nil || r.State != "failed" || r.Error == "" {
			t.Fatalf("partial: %+v %v", r, err)
		}
	}
	if writes != 1 {
		t.Fatalf("partial prefix written %d times", writes)
	}
}

func TestInputBatchCapacityNeverEvictsReceipt(t *testing.T) {
	h := &host{run: "fixture-run", done: make(chan struct{}), input: io.Discard, batches: make(map[string]InputReceipt)}
	for i := range MaxInputBatches {
		h.batches[string(rune(i+1))] = InputReceipt{State: "written", Digest: InputBatchDigest([][]byte{[]byte("old")})}
	}
	b := InputBatch{Run: h.run, Key: "new", Data: [][]byte{[]byte("new")}}
	if r := h.inputBatch(&b, false); r.Error == "" {
		t.Fatal("full receiver accepted a new key")
	}
	b.Key, b.Data = string(rune(1)), [][]byte{[]byte("old")}
	if r := h.inputBatch(&b, false); r.InputReceipt == nil || r.InputReceipt.State != "written" {
		t.Fatalf("evicted known receipt: %+v", r)
	}
	h.spec.Relay = true
	if r := h.inputCapability(); r.Error == "" {
		t.Fatal("relay advertised real input")
	}
}
