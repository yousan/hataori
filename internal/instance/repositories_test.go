package instance

import (
	"strings"
	"testing"
)

func TestRepositoryEnvironmentConfig(t *testing.T) {
	base := `{"owner":"x","trigger":"!go","idle":"1m","repositories":`
	for _, tc := range []struct{ entries, key string }{
		{`[{"url":"https://example.com/a/b","env":{"HATAORI_HOST_SOCKET":"x"}}]`, "repositories[0].env"},
		{`[{"url":"https://example.com/a/b","env":{"":"x"}}]`, "repositories[0].env"},
		{`[{"url":"https://example.com/a/b","env":{"A=B":"x"}}]`, "repositories[0].env"},
		{`[{"url":"https://example.com/a/b","env":{"A\u0000":"x"}}]`, "repositories[0].env"},
		{`[{"url":"https://example.com/a/b","env":{"A\n\u0000":"x"}}]`, "repositories[0].env"},
		{`[{"url":"https://example.com/a/b","env":{"TOKEN":"secret\u0000value"}}]`, "repositories[0].env"},
		{`[{"url":"https://example.com/a/b"},{"url":" https://example.com/a/b "}]`, "repositories[1].url"},
		{`[{"env":{}}]`, "repositories[0].url"},
		{`[{"url":"relative"}]`, "repositories[0].url"},
		{`[{"url":"https://example.com/a/b","env":{"A":false}}]`, "repositories.0.env.A"},
		{`[{"url":"https://example.com/a/b","env":{"A":null}}]`, "repositories[0].env.A"},
	} {
		_, err := ParseConfig([]byte(base + tc.entries + `}`))
		if err == nil || !strings.Contains(err.Error(), tc.key) || strings.Contains(err.Error(), "\n") || strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: %v; want one line naming %s without the value", tc.entries, err, tc.key)
		}
	}
	cfg, err := ParseConfig([]byte(base + `[{"url":" https://example.com/a/b ","env":{"GOFLAGS":"-trimpath","EMPTY":"","TEXT":" spaces\nand=equals "}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Repositories[0]
	if r.URL != "https://example.com/a/b" || r.Env["GOFLAGS"] != "-trimpath" || r.Env["EMPTY"] != "" || r.Env["TEXT"] != " spaces\nand=equals " {
		t.Fatalf("repository: %+v", r)
	}
}
