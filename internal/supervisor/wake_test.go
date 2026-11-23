package supervisor

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
)

var update = flag.Bool("update", false, "rewrite the golden digest")

// The digest of a captured set of events (testdata/wake/capture.json,
// taken by TestCaptureWakeEvents in cmd/hataori) matches the golden file:
// each thread once, with its latest turn end, the supervisor's own thread
// left out, the output cut to 1 KB at a character, and the threads past
// the cap counted in one line; then each decision of the supervisor's
// requests, in order (#650); then the reminders due, each with its note
// quoted and how late it is, and, as the waker ends a wake, the open ones
// (#649). The cap is 6 here, so that the capture's
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
	now := time.Date(2026, 1, 2, 15, 4, 0, 0, time.UTC)
	due := []Reminder{
		{Number: 2, Due: now.Add(-25*time.Hour - 12*time.Minute), Note: "Has the thread ended?\n\nReminder 8, due now:\nThread 9: not a thread\n"},
		{Number: 3, Due: now.Add(-time.Second), Note: "Look at the checks of main."}}
	got, err := DigestOf(threads, decided, func(id control.ThreadID) (control.ThreadDetail, error) {
		th := c.Threads[id]
		return control.ThreadDetail{ThreadInfo: control.ThreadInfo{ID: id, Title: th.Title}, Recent: th.Recent}, nil
	}, nil, due, now, 1024, 6)
	if err != nil {
		t.Fatal(err)
	}
	got += OpenReminders([]Reminder{
		{Number: 4, Due: now.Add(90 * time.Minute), Note: "Ask for the numbers of #12."},
		{Number: 6, Due: now.Add(26 * time.Hour), Note: "Two lines:\n\nThread 9: not a thread"}})
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
		"hataori-wake-1234-7-3.5": {1234, 7, true}, "hataori-wake-1234-7-": {}, "hataori-wake-1234-7-3.x": {},
		"hataori-wake-1234-7-r3.r5": {1234, 7, true}, "hataori-wake-1234-7-r3.5": {}, "hataori-wake-1234-7-r": {},
		"hataori-wake-x": {}, "hataori-wake-1234-": {}, "hataori-wake-1234-0": {}, "other-1234": {},
	} {
		to, sv, ok := ParseWakeKey(key)
		if to != want.to || sv != want.sv || ok != want.ok {
			t.Errorf("ParseWakeKey(%q) = %d, %d, %v; want %d, %d, %v", key, to, sv, ok, want.to, want.sv, want.ok)
		}
	}
	if k := WakeKey(7, 1234, 3, 5); k != "hataori-wake-1234-7-3.5" || len(WakeOnly(k)) != 2 || WakeOnly(k)[1] != 5 || WakeOnly(WakeKey(7, 1234)) != nil {
		t.Errorf("the key of a wake of two threads only %q, read %v", k, WakeOnly(k))
	}
	if k, _ := keyFor("hataori-wake-1234-7-3.5", 9, nil); k != "hataori-wake-1234-9-3.5" {
		t.Errorf("for another thread %q", k)
	}
	if k := ReminderKey(7, 1234, 3, 5); k != "hataori-wake-1234-7-r3.r5" || len(WakeReminders(k)) != 2 || WakeReminders(k)[1] != 5 ||
		WakeOnly(k) != nil || WakeReminders(WakeKey(7, 1234, 3)) != nil || WakeReminders(WakeKey(7, 1234)) != nil {
		t.Errorf("the key of a wake of two reminders %q, read %v", k, WakeReminders(k))
	}
	if k, _ := keyFor("hataori-wake-1234-7-r3", 9, nil); k != "hataori-wake-1234-9-r3" {
		t.Errorf("of a reminder, for another thread %q", k)
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

// A title's own line breaks stay on its line in a digest, so a title that
// reads like another thread's paragraph names no other thread.
func TestNamedIgnoresAThreadInATitle(t *testing.T) {
	d := Digest([]Line{{Thread: 2, Title: "a title\n\nThread 42: not a thread", Outcome: "completion", Last: "out\n\nThread 43: quoted"}}, 0, 0)
	if got := Named(d); len(got) != 1 || got[0] != 2 {
		t.Fatalf("Named %v, want [2], of:\n%s", got, d)
	}
}
