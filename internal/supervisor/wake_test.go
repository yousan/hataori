package supervisor

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/yousan/hataori/internal/control"
)

var update = flag.Bool("update", false, "rewrite the golden digest")

// The digest of a captured set of events (testdata/wake/capture.json,
// taken by TestCaptureWakeEvents in cmd/hataori) matches the golden file:
// each thread once, with its latest turn end, the supervisor's own thread
// left out, the output cut to 1 KB at a character, and the threads past
// the cap counted in one line; then each decision of the supervisor's
// requests, in order (#650). The cap is 6 here, so that the capture's
// eight threads pass it.
func TestDigestOfCapturedEvents(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "wake", "capture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Supervisor control.ThreadID `json:"supervisor"`
		Events     []control.Event  `json:"events"`
		Threads    map[control.ThreadID]struct {
			Title  string `json:"title"`
			Recent string `json:"recent"`
		} `json:"threads"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	ended := map[control.ThreadID]control.Event{}
	var decided []control.Event
	for _, e := range c.Events {
		if err := control.CheckEvent(e); err != nil {
			t.Fatalf("the capture's event %d: %v", e.Pos, err)
		}
		Fold(ended, e, e.Thread == c.Supervisor)
		if Decides(e, e.Thread == c.Supervisor) {
			decided = append(decided, e)
		}
	}
	var threads []Ended
	for id, e := range ended {
		threads = append(threads, Ended{Thread: id, Event: e})
	}
	sort.Slice(threads, func(i, j int) bool { return threads[i].Thread < threads[j].Thread })
	got, err := DigestOf(threads, decided, func(id control.ThreadID) (control.ThreadDetail, error) {
		th := c.Threads[id]
		return control.ThreadDetail{ThreadInfo: control.ThreadInfo{ID: id, Title: th.Title}, Recent: th.Recent}, nil
	}, 1024, 6)
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "wake", "digest.txt")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("digest:\n%s\nwant (%s):\n%s", got, golden, want)
	}
}

// A wake's key names the position and the thread, and a key from before
// it, with the position only, still reads (#443).
func TestWakeKey(t *testing.T) {
	if k := WakeKey(7, 1234); k != "hataori-wake-1234-7" {
		t.Fatalf("WakeKey(7, 1234) = %q", k)
	}
	for key, want := range map[string]struct {
		to uint64
		sv control.ThreadID
		ok bool
	}{
		"hataori-wake-1234-7": {1234, 7, true}, "hataori-wake-1234": {1234, 0, true},
		"hataori-wake-x": {}, "hataori-wake-1234-": {}, "hataori-wake-1234-0": {}, "other-1234": {},
	} {
		to, sv, ok := ParseWakeKey(key)
		if to != want.to || sv != want.sv || ok != want.ok {
			t.Errorf("ParseWakeKey(%q) = %d, %d, %v; want %d, %d, %v", key, to, sv, ok, want.to, want.sv, want.ok)
		}
	}
	took := map[string]control.ThreadID{"hataori-wake-1234": 7}
	sentTo := func(key string) (control.ThreadID, bool, error) {
		id, ok := took[key]
		return id, ok, nil
	}
	for _, c := range []struct {
		stored string
		sv     control.ThreadID
		want   string
	}{
		{"hataori-wake-1234-7", 7, "hataori-wake-1234-7"}, // the same thread: a repeat
		{"hataori-wake-1234-7", 9, "hataori-wake-1234-9"}, // a replacing thread
		{"hataori-wake-1234", 7, "hataori-wake-1234"},     // an old key, taken by the same thread
		{"hataori-wake-1234", 9, "hataori-wake-1234-9"},   // an old key, taken by the thread replaced
		{"hataori-wake-99", 9, "hataori-wake-99"},         // an old key no thread took
	} {
		if k, err := keyFor(c.stored, c.sv, sentTo); err != nil || k != c.want {
			t.Errorf("keyFor(%q, %d) = %q, %v; want %q", c.stored, c.sv, k, err, c.want)
		}
	}
}
