package host

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// A host started by an earlier hataori answers the screen request as an
// unknown operation: Screen reads its log as it did before, from the
// file's byte from on (#520).
func TestScreenOfAHostWithoutTheRequest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, TerminalLog), []byte("dialog\r\nmore"), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := SocketPath(dir)
	os.MkdirAll(filepath.Dir(sock), 0o700)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close(); os.Remove(sock) })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var req request
				if json.NewDecoder(bufio.NewReader(c)).Decode(&req) != nil {
					return
				}
				// What handle answers for an operation it does not know.
				json.NewEncoder(c).Encode(response{Error: `unknown operation "` + req.Op + `"`})
			}()
		}
	}()
	data, next := Screen(dir, 0)
	if string(data) != "dialog\r\nmore" || next != 12 {
		t.Fatalf("Screen from 0: %q %d", data, next)
	}
	data, next = Screen(dir, 8)
	if string(data) != "more" || next != 12 {
		t.Fatalf("Screen from 8: %q %d", data, next)
	}
	if data, next = Screen(dir, 12); data != nil || next != 12 {
		t.Fatalf("Screen from the end: %q %d", data, next)
	}
}
