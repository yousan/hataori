package instance

import (
	"strings"
	"testing"
	"time"
)

// The keys of a supervisor's posts (#657): their defaults, the values they
// take, and a bad value of each refused, naming the key.
func TestSupervisorPostsConfig(t *testing.T) {
	with := func(keys string) (*Supervisor, error) {
		c, err := ParseConfig([]byte(strings.Replace(validConfig, `"posters": ["hook-1"]`, `"posters": ["hook-1"], "supervisor": {`+keys+`}`, 1)))
		if err != nil {
			return nil, err
		}
		return c.Scopes[0].Supervisor, nil
	}
	sv, err := with(``)
	if err != nil || sv.Report != ReportEvery || sv.ReportEvery != time.Hour || sv.Mention != MentionNamed || sv.QuietHours != nil {
		t.Fatalf("defaults: %+v, %v", sv, err)
	}
	sv, err = with(`"report": "digest", "report_every": "30m", "mention": "never", "quiet_hours": {"from": "22:30", "to": "07:00", "zone": "America/New_York"}`)
	if err != nil || sv.Report != ReportDigest || sv.ReportEvery != 30*time.Minute || sv.Mention != MentionNever ||
		sv.QuietHours == nil || sv.QuietHours.From != 22*60+30 || sv.QuietHours.To != 7*60 || sv.QuietHours.Zone.String() != "America/New_York" {
		t.Fatalf("values: %+v, %v", sv, err)
	}
	if sv, err = with(`"report": "needed", "quiet_hours": {"from": "01:00", "to": "02:00"}`); err != nil || sv.Report != ReportNeeded || sv.QuietHours.Zone != time.Local {
		t.Fatalf("needed, the daemon's zone: %+v, %v", sv, err)
	}
	for bad, key := range map[string]string{
		`"report": "some"`:                                                  "report",
		`"report": "Every"`:                                                 "report",
		`"report_every": "0s"`:                                              "report_every",
		`"report_every": "-1h"`:                                             "report_every",
		`"report_every": "daily"`:                                           "report_every",
		`"mention": "always"`:                                               "mention",
		`"quiet_hours": {"from": "22:00", "to": "22:00"}`:                   "quiet_hours.to",
		`"quiet_hours": {"from": "24:00", "to": "07:00"}`:                   "quiet_hours.from",
		`"quiet_hours": {"from": "7:00", "to": "08:00"}`:                    "quiet_hours.from",
		`"quiet_hours": {"from": "22:00"}`:                                  "quiet_hours.to",
		`"quiet_hours": {"from": "22:00", "to": "07:60"}`:                   "quiet_hours.to",
		`"quiet_hours": {"from": "22:00", "to": "07:00", "zone": "Mars/X"}`: "quiet_hours.zone",
	} {
		if _, err := with(bad); err == nil || !strings.Contains(err.Error(), `"scopes[0].supervisor.`+key+`"`) {
			t.Errorf("%s: %v, want it refused naming %s", bad, err, key)
		}
	}
}
