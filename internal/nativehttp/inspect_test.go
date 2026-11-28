package nativehttp

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestInspectPinsOnlyVerifiedDashboard(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "matching", true: "wrong-server-identity"}[mismatch], func(t *testing.T) {
			cfg := fixture(t)
			ln := listenFixture(t, cfg)
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/native" || r.Host != "instance" {
					t.Errorf("unexpected request: %s %s", r.Host, r.URL.Path)
				}
				id := cfg.Instance
				if mismatch {
					id = "another-installation"
				}
				json.NewEncoder(w).Encode(map[string]any{"instance": id, "version": cfg.Version, "native": true})
			})}
			defer srv.Close()
			done := make(chan error, 1)
			go func() { done <- srv.Serve(ln) }()
			defer func() { srv.Close(); <-done }()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			got, err := Inspect(ctx, cfg.Root, cfg.Version)
			if mismatch {
				if err == nil {
					t.Fatal("adopted a mismatching server")
				}
			} else {
				canonical, canonicalErr := filepath.EvalSymlinks(cfg.Root)
				if canonicalErr != nil || err != nil || got.Root != canonical || got.Instance != cfg.Instance || got.Version != cfg.Version || got.RuntimeDir != cfg.RuntimeDir {
					t.Fatalf("binding: %+v %v; canonical root %q %v", got, err, canonical, canonicalErr)
				}
			}
			if _, err := Inspect(ctx, cfg.Root, "incompatible-version"); err == nil {
				t.Fatal("adopted an incompatible daemon")
			}
		})
	}
}
