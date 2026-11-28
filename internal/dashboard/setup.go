package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/yousan/hataori/internal/desktopsetup"
	"github.com/yousan/hataori/internal/nativehttp"
)

// SetupHandler serves only the daemon's existing assets and the restricted
// first-run API on a separately checked native listener. It has no control API,
// Caller, browser sign-in, terminal, threads, proxy or arbitrary mux extension.
// Startup owns the root lock until every handler has finished and the listener
// has closed. Normal Native listener routes are deliberately unchanged.
func SetupHandler(cfg nativehttp.Config, setup *desktopsetup.Setup) http.Handler {
	files := http.FileServerFS(page())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if !setupRequestOK(r) {
			http.Error(w, "Private setup request refused", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case NativeIdentityPath:
			writeJSON(w, map[string]any{"instance": cfg.Instance, "version": cfg.Version, "native": true, "setup": true})
		case "/api/setup":
			if r.Method != http.MethodPost {
				writeJSON(w, setup.Info())
				return
			}
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
			if err != nil || setup.Commit(r.Context(), body) != nil {
				http.Error(w, "Setup validation failed; existing files were preserved", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]bool{"complete": true})
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			setup.Published()
		case "/":
			http.ServeFileFS(w, r, page(), "index.html")
		default:
			files.ServeHTTP(w, r)
		}
	})
}

func setupRequestOK(r *http.Request) bool {
	if r.Host != "instance" || r.URL.IsAbs() || r.URL.RawPath != "" || path.Clean(r.URL.Path) != r.URL.Path || r.Header.Get("Upgrade") != "" {
		return false
	}
	for name, values := range r.Header {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "forwarded" || lower == "via" || lower == "x-real-ip" || strings.HasPrefix(lower, "x-forwarded-") {
			return false
		}
		if lower == "origin" && (len(values) != 1 || values[0] != NativeOrigin) {
			return false
		}
	}
	if r.Method == http.MethodPost {
		return r.URL.Path == "/api/setup" && r.Header.Get("Origin") == NativeOrigin && r.Header.Get("Content-Type") == "application/json"
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/assets/") || r.URL.Path == NativeIdentityPath || r.URL.Path == "/api/setup"
}

// ServeSetup joins all restricted HTTP handlers on completion/cancellation;
// callers release the daemon root lock only after it returns.
func ServeSetup(ctx context.Context, ln *nativehttp.Listener, handler http.Handler) error {
	var admission sync.Mutex
	var active sync.WaitGroup
	closing := false
	joined := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admission.Lock()
		if closing {
			admission.Unlock()
			http.Error(w, "Setup is closing", http.StatusServiceUnavailable)
			return
		}
		active.Add(1)
		admission.Unlock()
		defer active.Done()
		handler.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: joined, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 16 * 1024, BaseContext: func(_ net.Listener) context.Context { return ctx }}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	var err, serveErr error
	select {
	case <-ctx.Done():
		admission.Lock()
		closing = true
		admission.Unlock()
		err = srv.Shutdown(context.Background())
		ln.Close()
		serveErr = <-done
	case serveErr = <-done:
		admission.Lock()
		closing = true
		admission.Unlock()
		srv.Close()
		ln.Close()
	}
	active.Wait()
	if serveErr == http.ErrServerClosed {
		serveErr = nil
	}
	return errors.Join(err, serveErr)
}
