package dashboard

import (
	"bufio"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A passkey route's request body that does not arrive within the body
// timeout is answered and the connection freed, so a client that sends a
// byte now and then cannot hold it.
func TestAPasskeyRequestBodyHasADeadline(t *testing.T) {
	old := bodyTimeout
	bodyTimeout = 300 * time.Millisecond
	t.Cleanup(func() { bodyTimeout = old })
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "state"), 0o700)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	s := start(t, Config{Web: &Web{Proxy: "127.0.0.1", URL: "https://dash.example.com"}, Root: root})
	c, err := net.Dial("tcp", s.web.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("POST " + PasskeySignInBeginPath + " HTTP/1.1\r\nHost: dash.example.com\r\nOrigin: https://dash.example.com\r\n" +
		"Content-Type: application/json\r\nContent-Length: 100\r\n\r\n{"))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("no answer to a body that never arrives: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a body that never arrives: %s, want 400", resp.Status)
	}
}
