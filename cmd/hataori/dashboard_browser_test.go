package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
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

// fakeDevTools is a DevTools endpoint that answers each Runtime.evaluate
// with the next of answers (an error as Chrome sends it, or nil for true),
// and true once they are used up. It returns a browser connected to it.
func fakeDevTools(t *testing.T, answers ...[]byte) *browser {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			var req struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			if err := c.ReadJSON(&req); err != nil {
				return
			}
			var answer map[string]any
			if req.Method == "Runtime.evaluate" && len(answers) > 0 {
				json.Unmarshal(answers[0], &answer)
				answers = answers[1:]
			}
			if answer == nil {
				answer = map[string]any{"result": map[string]any{"result": map[string]any{"type": "boolean", "value": true}}}
			}
			answer["id"] = req.ID
			if c.WriteJSON(answer) != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &browser{t: t, conn: conn, timeout: 5 * time.Second, poll: time.Millisecond}
}

// TestBrowserUntilAfterTheTargetNavigated checks that until tries again
// when DevTools answers an evaluation with "Inspected target navigated or
// closed", as Chrome does for one sent while the page moves to another
// document, and that any other DevTools error still fails at once.
func TestBrowserUntilAfterTheTargetNavigated(t *testing.T) {
	navigated, err := os.ReadFile("testdata/devtools/chrome-138/evaluate-navigated.json")
	if err != nil {
		t.Fatal(err)
	}
	fakeDevTools(t, navigated, navigated).until("the condition after two navigations", "true")

	b := fakeDevTools(t, []byte(`{"error":{"code":-32000,"message":"Cannot do that"}}`))
	recorder := &browserRecorder{message: make(chan string, 1)}
	b.t = recorder
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.until("the condition", "true")
	}()
	<-done
	select {
	case message := <-recorder.message:
		if !strings.Contains(message, "Cannot do that") {
			t.Errorf("until failed with %q, want the DevTools error", message)
		}
	default:
		t.Fatal("until went on after a DevTools error that is not a navigation")
	}
}
