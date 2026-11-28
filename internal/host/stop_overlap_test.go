package host

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/proctest"
)

// A command ending must not wait on the stop-reply WaitGroup while more
// stops can register. Sleep and close may overlap. Pipelining requests
// keeps registration busy across the first stop's reply; a single host
// scheduler makes the forbidden zero-to-positive reuse window reproducible.
func TestStopsWhileTheCommandEnds(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { killGroups(dir) })
	hc := hostCommand()
	hc.Env = []string{"GOMAXPROCS=1"}
	pid, err := Start(hc, dir, Spec{Argv: self("echo"), Env: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Hold replies briefly so a stop remains registered when the host's
	// shutdown goroutine wakes. Reading them afterwards lets normal
	// shutdown finish.
	var requests bytes.Buffer
	for range 4096 {
		if err := json.NewEncoder(&requests).Encode(request{Op: "stop", Interrupt: time.Second, Terminate: time.Second}); err != nil {
			t.Fatal(err)
		}
	}
	sent := make(chan struct{})
	go func() { defer close(sent); c.conn.Write(requests.Bytes()) }()
	read := make(chan struct{})
	go func() { defer close(read); time.Sleep(100 * time.Millisecond); io.Copy(io.Discard, c.conn) }()
	defer func() { c.Close(); <-sent; <-read }()
	waitFor(t, "the host to finish", func() bool { _, ok, _ := ReadStatus(dir); return ok || proctest.Ended(pid) })
	if st, ok, err := ReadStatus(dir); err != nil || !ok || !st.Stopped {
		log, _ := os.ReadFile(filepath.Join(dir, HostLog))
		t.Fatalf("host status %+v, present %v, error %v; host log:\n%s", st, ok, err, log)
	}
	if !Gone(dir) {
		t.Fatal("the completed host left its socket")
	}
}
