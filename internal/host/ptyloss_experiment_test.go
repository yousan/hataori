package host

// Temporary experiment for #552, removed before merge: is a terminal
// command's output that the controlling side has not read yet lost once the
// command's side closes?

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
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
// and the command has ended before the controlling side is read.
func TestExperimentClosedAtOnceReadAfterExit(t *testing.T) {
	lost := 0
	for i := 0; i < 50; i++ {
		cmd, m, s := startPrinter(t)
		s.Close()
		cmd.Wait()
		time.Sleep(50 * time.Millisecond)
		got, err := readAll(m)
		m.Close()
		if got != experimentOut {
			lost++
			if lost == 1 {
				t.Logf("read %q, %v", got, err)
			}
		}
	}
	t.Logf("RESULT closed-at-once: %d of 50 lost output", lost)
}

// The host keeps its copy of the command's side open until the command has
// ended and nothing is left to read, then closes it.
func TestExperimentKeptOpenReadAfterExit(t *testing.T) {
	lost := 0
	for i := 0; i < 50; i++ {
		cmd, m, s := startPrinter(t)
		cmd.Wait()
		time.Sleep(50 * time.Millisecond)
		outq, errOutq := unix.IoctlGetInt(int(s.Fd()), unix.TIOCOUTQ)
		inq, errInq := unix.IoctlGetInt(int(m.Fd()), fionread())
		if i == 0 {
			t.Logf("RESULT after exit, unread: TIOCOUTQ on the command's side %d (%v), FIONREAD on the controlling side %d (%v)", outq, errOutq, inq, errInq)
		}
		done := make(chan string)
		go func() {
			got, err := readAll(m)
			if err != nil && !errors.Is(err, unix.EIO) && i == 0 {
				t.Logf("read ended with %v", err)
			}
			done <- got
		}()
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if n, err := unix.IoctlGetInt(int(s.Fd()), unix.TIOCOUTQ); err != nil || n == 0 {
				break
			}
		}
		if i == 0 {
			outq, errOutq = unix.IoctlGetInt(int(s.Fd()), unix.TIOCOUTQ)
			t.Logf("RESULT once read: TIOCOUTQ %d (%v)", outq, errOutq)
		}
		s.Close()
		var got string
		select {
		case got = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the read did not end once the command's side was closed")
		}
		m.Close()
		if got != experimentOut {
			lost++
			if lost == 1 {
				t.Logf("read %q", got)
			}
		}
	}
	t.Logf("RESULT kept-open: %d of 50 lost output", lost)
}

func fionread() uint {
	if runtime.GOOS == "darwin" {
		return 0x4004667f
	}
	return 0x541b
}
