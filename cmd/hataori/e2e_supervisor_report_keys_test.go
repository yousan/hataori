package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/chat"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
	"github.com/yousan/hataori/internal/wire"
)

// The acceptance criteria of #657: the keys report, mention and
// quiet_hours of scopes[].supervisor, end to end with the fake agent and
// the wake test's clock, which the core, the waker and the chat front end
// all go by.

// reportTest is a wake test whose supervisor answers each wake with the
// text its worker gave, and whose keys are keys.
type reportTest struct {
	*wakeTest
	n int // wakes so far
}

func newReportTest(t *testing.T, keys map[string]any) *reportTest {
	t.Helper()
	wt := newWakeTest(t, budget(0, 0, 0), func(cfg map[string]any) {
		sv := cfg["scopes"].([]map[string]any)[0]["supervisor"].(map[string]any)
		sv["instructions"] = "wake-script"
		for k, v := range keys {
			sv[k] = v
		}
	})
	frontEndHook = func(fe *chat.FrontEnd) { fe.Now, fe.Tick = wt.clk.Now, 10*time.Millisecond }
	t.Cleanup(func() { frontEndHook = nil })
	return &reportTest{wakeTest: wt}
}

// started runs the daemon, and returns its stop, the owner's client, the
// supervisor's thread and its conversation, once the answer to its first
// message is posted.
func (rt *reportTest) started(t *testing.T) (func(), *wire.Client, control.ThreadInfo, surface.Conversation) {
	t.Helper()
	stop, owner, sv := rt.start(t)
	conv := surface.Conversation{Channel: sv.Links[0].Channel, Thread: sv.Links[0].Thread}
	rt.r.posted(t, conv, "done")
	return stop, owner, sv, conv
}

// wake makes a worker end a turn whose last output is text, and waits
// until the supervisor's turn of the wake, which answers text, ended.
func (rt *reportTest) wake(t *testing.T, owner *wire.Client, sv control.ThreadInfo, text string) {
	t.Helper()
	rt.n++
	_, pos := rt.worker(t, owner, "c1", fmt.Sprintf("w%d", rt.n), "reply reply "+text)
	rt.read(t, pos)
	rt.quiet(t)
	rt.woken(t, owner, sv.ID, rt.n)
	time.Sleep(100 * time.Millisecond) // the answer's rows are written
}

// advance moves the clock on by d, a quiet period at a time.
func (rt *reportTest) advance(d time.Duration) {
	for ; d > 0; d -= 30 * time.Second {
		rt.clock().advance(min(d, 30*time.Second))
		time.Sleep(2 * time.Millisecond)
	}
}

func texts(d *fakesurface.Driver, conv surface.Conversation) []string {
	var out []string
	for _, c := range postsIn(d, conv) {
		out = append(out, c.Message.Text)
	}
	return out
}

// Under "needed", a wake's answer that names nobody is not posted and one
// that names a person is; a person's turn is posted.
func TestReportNeeded(t *testing.T) {
	rt := newReportTest(t, map[string]any{"report": "needed"})
	stop, owner, sv, conv := rt.started(t)
	defer stop()
	before := len(postsIn(rt.r.driver, conv))
	rt.wake(t, owner, sv, "all is well")
	rt.wake(t, owner, sv, "@boss thread 2 needs you")
	got := rt.r.posted(t, conv, "@boss thread 2 needs you")
	if !slices.Equal(got.Message.Notify, []string{e2eOwner}) {
		t.Fatalf("the answer that names the owner mentions %v", got.Message.Notify)
	}
	if p := texts(rt.r.driver, conv)[before:]; len(p) != 1 {
		t.Fatalf("posts %q, want only the answer that names a person", p)
	}
	rt.r.driver.Inject(conv, fakesurface.Person(e2eOwner), "reply nothing for anyone")
	rt.r.posted(t, conv, "nothing for anyone")
}

// Under "digest", an answer that names a person is posted at once; three
// that name nobody, in one period, are posted together as one message at
// its end, once, also with a restart during the period and one after.
func TestReportDigestAcrossARestart(t *testing.T) {
	rt := newReportTest(t, map[string]any{"report": "digest", "report_every": "1h"})
	stop, owner, sv, conv := rt.started(t)
	before := len(postsIn(rt.r.driver, conv))
	rt.wake(t, owner, sv, "first of three")
	rt.wake(t, owner, sv, "@dispatch look at this now")
	rt.r.posted(t, conv, "@dispatch look at this now")
	rt.wake(t, owner, sv, "second of three")
	stop()
	stop, owner, sv = rt.start(t)
	rt.wake(t, owner, sv, "third of three")
	if p := texts(rt.r.driver, conv)[before:]; len(p) != 1 {
		t.Fatalf("posts %q during the period, want only the answer that names a person", p)
	}
	rt.r.driver.Inject(conv, fakesurface.Person(e2eOwner), "reply posted at once")
	rt.r.posted(t, conv, "posted at once")

	rt.advance(time.Hour)
	var digest string
	eventually(t, "the digest", 15*time.Second, func() bool {
		for _, s := range texts(rt.r.driver, conv) {
			if strings.HasPrefix(s, "3 reports held") {
				digest = s
				return true
			}
		}
		return false
	})
	for _, w := range []string{"first of three", "second of three", "third of three"} {
		if !strings.Contains(digest, w) {
			t.Fatalf("the digest %q lacks %q", digest, w)
		}
	}
	stop()
	stop, _, _ = rt.start(t)
	defer stop()
	rt.advance(2 * time.Hour)
	time.Sleep(300 * time.Millisecond)
	n := 0
	for _, s := range texts(rt.r.driver, conv) {
		if strings.Contains(s, "of three") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the held answers were posted %d times, want once, in the digest: %q", n, texts(rt.r.driver, conv)[before:])
	}
}

// Under mention "never", no post in the thread mentions anyone: not an
// answer that names a person, and not the ping of a failed turn.
func TestMentionNever(t *testing.T) {
	rt := newReportTest(t, map[string]any{"mention": "never"})
	stop, owner, sv, conv := rt.started(t)
	defer stop()
	rt.wake(t, owner, sv, "@boss and @dispatch, look")
	rt.r.posted(t, conv, "@boss and @dispatch, look")
	failing := rt.r.driver.Inject(conv, fakesurface.Person(e2eOwner), "fail api_error")
	eventually(t, "the failed turn's lamp", 15*time.Second, lampsAre(rt.r.driver, failing.Ref, working, failed))
	time.Sleep(300 * time.Millisecond)
	if anyPings(rt.r.driver, conv) {
		t.Fatalf("a post mentions someone: %+v", postsIn(rt.r.driver, conv))
	}
}

// During quiet hours across midnight, in a time zone other than the
// daemon's, no post mentions anyone; at their end one message mentions
// each person named during them, with how many times; after it, a name
// mentions again.
func TestQuietHoursAcrossMidnightInAnotherZone(t *testing.T) {
	// A zone where the clock's start is 23:00, or 01:00 if that is the
	// daemon's own offset: the span 22:00 to 01:00, or 23:00 to 02:00,
	// holds it, across midnight, and ends two hours after it.
	from, to, hour := "22:00", "01:00", 23
	_, local := wakeBase.Zone()
	off := ((hour-wakeBase.UTC().Hour())%24 + 24) % 24
	if off > 12 {
		off -= 24
	}
	if off*3600 == local {
		from, to, hour = "23:00", "02:00", 1
		if off = ((hour-wakeBase.UTC().Hour())%24 + 24) % 24; off > 12 {
			off -= 24
		}
	}
	zone := fmt.Sprintf("Etc/GMT%+d", -off) // Etc/GMT-9 is 9 hours ahead
	if z, err := time.LoadLocation(zone); err != nil || wakeBase.In(z).Hour() != hour {
		t.Fatalf("zone %s: %v, %v", zone, wakeBase.In(z), err)
	}
	rt := newReportTest(t, map[string]any{"quiet_hours": map[string]any{"from": from, "to": to, "zone": zone}})
	stop, owner, sv, conv := rt.started(t)
	defer stop()
	rt.wake(t, owner, sv, "@boss one")
	rt.wake(t, owner, sv, "@boss and @dispatch, two")
	failing := rt.r.driver.Inject(conv, fakesurface.Person(e2eOwner), "fail api_error")
	eventually(t, "the failed turn's lamp", 15*time.Second, lampsAre(rt.r.driver, failing.Ref, working, failed))
	rt.r.posted(t, conv, "@boss and @dispatch, two")
	time.Sleep(300 * time.Millisecond)
	if anyPings(rt.r.driver, conv) {
		t.Fatalf("a post mentions someone during quiet hours: %+v", postsIn(rt.r.driver, conv))
	}

	rt.advance(2 * time.Hour)
	over := rt.r.posted(t, conv, "Quiet hours are over. Named during them: @boss 2 times, @dispatch once.")
	if !slices.Equal(over.Message.Notify, []string{e2eOwner, e2eHook}) {
		t.Fatalf("the message at their end mentions %v, want the owner and dispatch", over.Message.Notify)
	}
	rt.wake(t, owner, sv, "@boss three")
	if got := rt.r.posted(t, conv, "@boss three"); !slices.Equal(got.Message.Notify, []string{e2eOwner}) {
		t.Fatalf("after quiet hours the answer mentions %v", got.Message.Notify)
	}
	time.Sleep(300 * time.Millisecond)
	n := 0
	for _, s := range texts(rt.r.driver, conv) {
		if strings.HasPrefix(s, "Quiet hours are over.") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d messages at the end of quiet hours, want 1", n)
	}
}
