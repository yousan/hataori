package host

// Temporary experiment for #552, removed before merge: is a terminal
// command's output that the controlling side has not read yet lost once the
// command's side closes?

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

const experimentOut = "cwd=/somewhere\nenv=ONE=1\nenv=TWO=two words\nenv=EMPTY=\n"

// startPrinter starts a command on a new terminal that prints
// experimentOut and exits, and returns the controlling side and the
// command's side, which the caller still holds a copy of.
func startPrinter(t *testing.T) (*exec.Cmd, *os.File, *os.File) {
	t.Helper()
	m, s, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "printf '%s' \"$OUT\"")
	cmd.Env = []string{"OUT=" + experimentOut}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s, s, s
	cmd.SysProcAttr = &unix.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, m, s
}

func readAll(m *os.File) (string, error) {
	var b strings.Builder
	_, err := io.Copy(&b, m)
	return strings.ReplaceAll(b.String(), "\r\n", "\n"), err
}

// As the host does now: its copy of the command's side is closed at once,
// and a reader of the controlling side runs while the command prints and
// exits. (Reading only after the command has ended hangs on macOS: its
// exit waits until its output has been read.)
func TestExperimentConcurrentReader(t *testing.T) {
	lost, hung := 0, 0
	for i := 0; i < 1000; i++ {
		cmd, m, s := startPrinter(t)
		s.Close()
		done := make(chan string, 1)
		var rerr error
		go func() {
			got, err := readAll(m)
			rerr = err
			done <- got
		}()
		waited := make(chan struct{})
		go func() { cmd.Wait(); close(waited) }()
		select {
		case got := <-done:
			if got != experimentOut {
				lost++
				if lost <= 3 {
					t.Logf("run %d: read %q, %v", i, got, rerr)
				}
			}
		case <-time.After(5 * time.Second):
			hung++
		}
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
			t.Logf("run %d: the command did not end", i)
		}
		m.Close()
	}
	t.Logf("RESULT concurrent reader: %d of 1000 lost output, %d hung", lost, hung)
}
