package runner

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// readBytes counts the bytes f reads, as the kernel counts them for the
// thread f runs on: it does not depend on how the reading is written, and
// other tests' reads are not in it.
func readBytes(t *testing.T, f func()) int64 {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	count := func() int64 {
		b, err := os.ReadFile("/proc/thread-self/io")
		if err != nil {
			t.Fatal(err)
		}
		var n int64
		if _, err := fmt.Sscanf(string(b), "rchar: %d", &n); err != nil {
			t.Fatalf("%v: %s", err, b)
		}
		return n
	}
	before := count()
	f()
	return count() - before
}

// A step reads only what is behind the stored offsets (#694).
func TestStepReadsOnlyWhatIsNew(t *testing.T) {
	const big = 5 << 20
	text := func(n int) []byte {
		return []byte(fmt.Sprintf(`{"t":"text","text":%q,"final":false}`, bytes.Repeat([]byte("x"), n)))
	}
	// A session with a followed file and a log of 5 MB each, both read.
	read := func(t *testing.T) (*reader, string) {
		x := newReader(t, fakeTerminalCodec(t))
		transcript := filepath.Join(x.lv.dir, "transcript.jsonl")
		x.hooks(hookLine("h1", `{"t":"ready","session":"s","transcript":"`+transcript+`"}`),
			hookLine("h2", `{"t":"turn","by":"caller"}`),
			hookLine("h3", `{"t":"nothing","text":"`+string(bytes.Repeat([]byte("x"), big))+`"}`))
		for range 5 {
			x.appendTo(transcript, text(big/5))
		}
		if n := len(x.step()); n < 6 {
			t.Fatalf("%d events, want the file's five lines and those of the log", n)
		}
		return x, transcript
	}
	steps := func(x *reader, n int) (events int) {
		for range n {
			events += len(x.step())
		}
		return events
	}

	t.Run("nothing new", func(t *testing.T) {
		x, _ := read(t)
		events := 0
		if n := readBytes(t, func() { events = steps(x, 20) }); n >= 1<<20 || events != 0 {
			t.Fatalf("20 steps read %d bytes and gave %d events, want less than 1 MB and none", n, events)
		}
	})
	t.Run("one new line each", func(t *testing.T) {
		x, transcript := read(t)
		events := 0
		n := readBytes(t, func() {
			for range 20 {
				x.appendTo(transcript, text(10))
				events += steps(x, 1)
			}
		})
		if n >= 1<<20 || events != 20 {
			t.Fatalf("20 steps read %d bytes and gave %d events, want less than 1 MB and 20", n, events)
		}
	})
}
