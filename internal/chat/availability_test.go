package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

func TestSupervisorOutageSurvivesReaderRestartAndReportsRecovery(t *testing.T) {
	f := newFront(t, workspaces{})
	id, _ := f.started(t)
	f.setKind(t, id, control.KindSupervisor)
	cases := []struct{ outcome, reason, detail, want string }{
		{"failure", "unavailable", "subscription rejected; check provider settings", "The supervisor is unavailable; worker management is unavailable.\nsubscription rejected; check provider settings"},
		{"failure", "unavailable", "subscription rejected; check provider settings", ""},
		{"blocked", "", "", ""},
		{"failure", "unavailable", "subscription rejected; check provider settings", ""},
		{"failure", "unavailable", "a different provider next step", "The supervisor is unavailable; worker management is unavailable.\na different provider next step"},
		{"failure", "api_error", "connection lost", "The turn failed: error (api_error).\nconnection lost"},
		{"failure", "unavailable", "a different provider next step", "The supervisor is unavailable; worker management is unavailable.\na different provider next step"},
		{"failure", "crashed", "", "The turn failed: the agent crashed."},
		{"completion", "", "", "The supervisor is available again; worker management has resumed."},
		{"completion", "", "", ""},
		{"failure", "unavailable", "subscription rejected; check provider settings", "The supervisor is unavailable; worker management is unavailable.\nsubscription rejected; check provider settings"},
	}
	var events []control.Event
	// Queue the entire history first. Later stored events must not change
	// what an earlier event reports when a reader catches up after a restart.
	for i, tc := range cases {
		d := map[string]any{"turn": i + 1, "by": "caller", "outcome": tc.outcome, "background": 0}
		if tc.reason != "" {
			d["reason"] = tc.reason
		}
		if tc.detail != "" {
			d["detail"] = tc.detail
		}
		pos := f.event(t, id, "turn_ended", d)
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, control.Event{Pos: pos, Thread: id, Kind: "turn_ended", Data: raw})
	}
	for i, ev := range events {
		// A new reader for every event proves the outage is not kept only in
		// memory. Only a completed turn proves recovery.
		rows, err := f.fe.rowsFor(context.Background(), &reader{}, ev, []surface.Conversation{f.conv(id)})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, row := range rows {
			got = append(got, row.Message.Text)
		}
		if s := strings.Join(got, "|"); s != cases[i].want {
			t.Errorf("event %d: got %q, want %q", i, s, cases[i].want)
		}
	}
}

func TestOnlyOwnSupervisorDeduplicatesUnavailableFailures(t *testing.T) {
	for _, kind := range []control.ThreadKind{control.KindOrdinary, control.KindConversation, control.KindLookup} {
		t.Run(string(kind), func(t *testing.T) {
			f := newFront(t, workspaces{})
			id, _ := f.started(t)
			f.setKind(t, id, kind)
			for i := 1; i <= 2; i++ {
				d := map[string]any{"turn": i, "by": "caller", "outcome": "failure", "reason": "unavailable", "background": 0, "detail": "provider next step"}
				pos := f.event(t, id, "turn_ended", d)
				raw, _ := json.Marshal(d)
				rows, err := f.fe.rowsFor(context.Background(), &reader{}, control.Event{Pos: pos, Thread: id, Kind: "turn_ended", Data: raw}, []surface.Conversation{f.conv(id)})
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 || rows[0].Message.Text != "The turn failed: the provider is unavailable.\nprovider next step" {
					t.Fatalf("got %+v", rows)
				}
			}
		})
	}
}

func TestQuietSupervisorWakeStillReportsRecovery(t *testing.T) {
	f := newFront(t, workspaces{})
	id, _ := f.started(t)
	f.setKind(t, id, control.KindSupervisor)
	stop := f.showing(t)
	f.event(t, id, "turn_started", map[string]any{"turn": 81, "by": "caller", "key": wakeKeyPrefix + "81"})
	f.event(t, id, "turn_ended", map[string]any{"turn": 81, "by": "caller", "outcome": "failure", "reason": "unavailable", "background": 0, "detail": "provider next step"})
	f.postedUntil(t, id, "The supervisor is unavailable; worker management is unavailable.\nprovider next step")
	stop()
	f.event(t, id, "turn_started", map[string]any{"turn": 82, "by": "caller", "key": wakeKeyPrefix + "82"})
	f.event(t, id, "text", map[string]any{"text": "(quiet)", "final": true})
	ended(f, t, id, 82, "completion", "")
	f.showing(t)
	posts := f.postedUntil(t, id, "The supervisor is available again; worker management has resumed.")
	var outages, recoveries int
	for _, p := range posts {
		if strings.Contains(p.Message.Text, "(quiet)") {
			t.Fatal("quiet wake posted its sentinel")
		}
		if strings.HasPrefix(p.Message.Text, "The supervisor is unavailable;") {
			outages++
		}
		if strings.HasPrefix(p.Message.Text, "The supervisor is available again;") {
			recoveries++
		}
	}
	if outages != 1 || recoveries != 1 {
		t.Fatalf("got %d outages and %d recoveries", outages, recoveries)
	}
}
