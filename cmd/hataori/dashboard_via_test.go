package main

import (
	"testing"

	"github.com/yousan/hataori/internal/people"
)

// Every dashboard session's Caller says that its calls came through the
// dashboard, the owner's and another person's alike (#604).
func TestDashboardCallerSaysItsWay(t *testing.T) {
	dir := people.New([]people.Person{{Name: "ada"}, {Name: "grace"}}, "ada", map[string][]string{"s": {"grace"}})
	for _, person := range []string{"ada", "grace"} {
		if c := dashboardCaller(dir, []string{"s"}, false, false, person); c.Via != "dashboard" {
			t.Errorf("%s's dashboard Caller has Via %q, want dashboard", person, c.Via)
		}
	}
}
