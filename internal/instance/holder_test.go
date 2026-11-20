package instance

import (
	"strings"
	"testing"
)

// "holder" is hataori's own host when absent, tmux when it says so, and
// any other value is refused, naming the key.
func TestHolderConfig(t *testing.T) {
	with := func(keys string) (*Config, error) {
		return ParseConfig([]byte(strings.Replace(validConfig, "{", "{"+keys+",", 1)))
	}
	c, err := ParseConfig([]byte(validConfig))
	if err != nil || c.Holder != HolderHost {
		t.Fatalf("without the key: %q, %v", c.Holder, err)
	}
	for keys, want := range map[string]string{`"holder": "host"`: HolderHost, `"holder": "tmux"`: HolderTmux, `"holder": ""`: HolderHost} {
		if c, err := with(keys); err != nil || c.Holder != want {
			t.Errorf("%s: %+v, %v", keys, c, err)
		}
	}
	for _, bad := range []string{`"holder": "screen"`, `"holder": "TMUX"`, `"holder": "iterm2"`} {
		if _, err := with(bad); err == nil || !strings.Contains(err.Error(), `"holder"`) {
			t.Errorf("%s accepted: %v", bad, err)
		}
	}
}
