package instance

import (
	"strings"
	"testing"
)

func TestMaxResidentAgentsConfig(t *testing.T) {
	const base = `"owner":"o","trigger":"!go","idle":"30m"`
	for _, tc := range []struct {
		field string
		want  int
	}{
		{"", 0}, {`,"max_resident_agents":0`, 0}, {`,"max_resident_agents":12`, 12},
	} {
		c, err := ParseConfig([]byte("{" + base + tc.field + "}"))
		if err != nil {
			t.Fatal(err)
		}
		if c.MaxResidentAgents != tc.want {
			t.Errorf("%s: cap %d, want %d", tc.field, c.MaxResidentAgents, tc.want)
		}
	}
	for _, value := range []string{"-1", "1.5", `"12"`} {
		_, err := ParseConfig([]byte("{" + base + `,"max_resident_agents":` + value + "}"))
		if err == nil || !strings.Contains(err.Error(), "max_resident_agents") {
			t.Errorf("%s: %v", value, err)
		}
	}
}
