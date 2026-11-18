package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

// browserRecorder captures a fatal message from a browser running in a goroutine.
type browserRecorder struct {
	message chan string
}

func (r *browserRecorder) Helper() {}
func (r *browserRecorder) Fatal(args ...any) {
	r.message <- fmt.Sprint(args...)
	runtime.Goexit()
}
func (r *browserRecorder) Fatalf(format string, args ...any) {
	r.message <- fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// TestBrowserUntilDuringNavigation checks that polling survives exceptions across
// navigation and reports the last exception and HTML when it times out.
func TestBrowserUntilDuringNavigation(t *testing.T) {
	b := newBrowser(t)
	// Two pages of a server of the test's own: the first navigates itself to
	// the second, which fills #x only after a while. A page may not navigate
	// itself to a data: URL, so they are served over HTTP.
	pages := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/first":
			fmt.Fprint(w, `<!doctype html><body><p>waiting on the first page</p><script>setTimeout(() => { location.href = "/second"; }, 100)</script></body>`)
		case "/second":
			fmt.Fprint(w, `<!doctype html><body><p>waiting on the second page</p><script>setTimeout(() => { document.body.innerHTML = '<div id="x">ok</div>'; }, 500)</script></body>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer pages.Close()
	// Poll while the first page navigates to the second, before either has
	// the element: the evaluation throws before and during the navigation.
	b.call("Page.navigate", map[string]any{"url": pages.URL + "/first"})
	b.until("the element after navigation", `document.querySelector('#x').textContent === 'ok'`)

	b.timeout = 200 * time.Millisecond
	recorder := &browserRecorder{message: make(chan string, 1)}
	b.t = recorder
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.until("the missing element", `document.querySelector('#missing').textContent === 'ok'`)
	}()
	<-done
	select {
	case message := <-recorder.message:
		for _, want := range []string{"the missing element", "Uncaught", "TypeError", `<div id="x">ok</div>`} {
			if !strings.Contains(message, want) {
				t.Errorf("timeout message %q does not contain %q", message, want)
			}
		}
	default:
		t.Fatal("until returned without failing")
	}
}
