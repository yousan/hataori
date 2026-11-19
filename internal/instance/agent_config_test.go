package instance

import (
	"strings"
	"testing"
)

func TestAgentConfigSelectionAndDefaults(t *testing.T) {
	base := `"owner":"x","trigger":"!go","idle":"1m"`
	for _, tc := range []struct{ extra, key string }{
		{`,"agent":"missing","adapters":{"one":{}}`, `"agent"`},
		{`,"scopes":[{"key":"c","client":"chat","agent":"missing"}],"adapters":{"one":{}}`, `"scopes[0].agent"`},
		{`,"adapters":{"one":{},"two":{}}`, `"agent"`},
		{`,"adapters":{"one":{"model":42}}`, `"adapters.one.model"`},
	} {
		_, err := ParseConfig([]byte("{" + base + tc.extra + "}"))
		if err == nil || !strings.Contains(err.Error(), tc.key) || strings.Contains(err.Error(), "\n") {
			t.Errorf("%s: %v", tc.extra, err)
		}
	}
	cfg, err := ParseConfig([]byte("{" + base + `,"agent":"one","adapters":{"one":{"model":" m ","effort":" high ","other":true},"two": { "other" : true }}}`))
	if err != nil {
		t.Fatal(err)
	}
	if d := cfg.AgentDefaults["one"]; d.Model != "m" || d.Effort != "high" {
		t.Fatalf("defaults: %+v", d)
	}
	if string(cfg.Adapters["one"]) != `{"other":true}` || string(cfg.Adapters["two"]) != `{ "other" : true }` {
		t.Fatalf("sections: %q %q", cfg.Adapters["one"], cfg.Adapters["two"])
	}
}
