package host

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func init() {
	// flood <lines>: wait for a line, write numbered lines, then "flooded",
	// then answer every input line.
	extraRoles["flood"] = func(args []string) int {
		n, _ := strconv.Atoi(args[0])
		r := bufio.NewScanner(os.Stdin)
		r.Scan()
		w := bufio.NewWriterSize(os.Stdout, 64<<10)
		for i := range n {
			fmt.Fprintf(w, "%08d\n", i)
		}
		w.WriteString("flooded\n")
		w.Flush()
		for r.Scan() {
			fmt.Printf("got %s\n", r.Text())
		}
		return 0
	}
}

// readUntil reads s until everything it has sent ends with suffix, checks
// that each chunk continues the one before, and returns what it read.
func readUntil(t *testing.T, s *TerminalStream, pos uint64, suffix string) ([]byte, uint64) {
	t.Helper()
	var got []byte
	s.conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	defer s.conn.SetReadDeadline(time.Time{})
	for !bytes.HasSuffix(got, []byte(suffix)) {
		ch, err := s.Next()
		if err != nil {
			t.Fatalf("after %d bytes: %v", len(got), err)
		}
		if ch.Reset || ch.Pos != pos+uint64(len(ch.Data)) {
			t.Fatalf("chunk at %d with %d bytes (reset %v) does not continue %d", ch.Pos, len(ch.Data), ch.Reset, pos)
		}
		got = append(got, ch.Data...)
		pos = ch.Pos
	}
	return got, pos
}

func follow(t *testing.T, dir, run string, after uint64) (*TerminalStream, TerminalChunk) {
	t.Helper()
	s, err := FollowTerminal(dir, run, after)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ch, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	return s, ch
}

func TestFollowTerminalAcrossTrim(t *testing.T) {
	const limit = 16 << 10
	dir := startHost(t, Spec{Argv: self("flood", "20000"), Env: []string{}, Terminal: true, Rows: 30, Cols: 100, LogLimit: limit})
	a, first := follow(t, dir, "", 0)
	if !first.Reset || first.Run == "" || first.Cols != 100 || first.Rows != 30 {
		t.Fatalf("first chunk: reset %v, run %q, size %dx%d", first.Reset, first.Run, first.Cols, first.Rows)
	}
	if first.Pos != uint64(len(first.Data)) {
		t.Fatalf("nothing is trimmed yet, but the reset holds %d bytes ending at %d", len(first.Data), first.Pos)
	}
	run := first.Run
	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Input([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	rest, end := readUntil(t, a, first.Pos, "flooded\r\n")
	all := append(first.Data, rest...) // every byte of this run, by position
	if uint64(len(all)) != end {
		t.Fatalf("read %d bytes, the stream is at %d", len(all), end)
	}
	kept := read(t, dir, TerminalLog)
	if len(kept) >= len(all) || len(kept) > limit {
		t.Fatalf("the log was not trimmed: %d bytes kept of %d", len(kept), len(all))
	}
	if !bytes.HasSuffix(all, kept) {
		t.Fatal("the kept log is not the end of what was followed")
	}

	// From a position still kept: exactly the bytes after it.
	for _, after := range []uint64{end - uint64(len(kept)), end - 1000, end - 1, end} {
		_, ch := follow(t, dir, run, after)
		if ch.Reset || ch.Pos != end || !bytes.Equal(ch.Data, all[after:]) {
			t.Fatalf("after %d: reset %v, pos %d, %d bytes; want %d bytes up to %d", after, ch.Reset, ch.Pos, len(ch.Data), end-after, end)
		}
	}

	// From a position no longer kept, from another run, from 0 or from the
	// future: the end of the log, marked as a reset.
	for _, tc := range []struct {
		run   string
		after uint64
	}{{run, end - uint64(len(kept)) - 1}, {run, 1}, {"another run", end - 10}, {run, 0}, {run, end + 1}} {
		_, ch := follow(t, dir, tc.run, tc.after)
		if !ch.Reset || ch.Run != run || ch.Pos != end || !bytes.Equal(ch.Data, kept) {
			t.Fatalf("%q after %d: reset %v, run %q, pos %d, %d bytes; want the %d kept bytes as a reset", tc.run, tc.after, ch.Reset, ch.Run, ch.Pos, len(ch.Data), len(kept))
		}
	}

	// A follower that joined late gets the same new output as one that
	// followed from the start.
	b, _ := follow(t, dir, run, end-100)
	if err := c.Input([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	fromA, posA := readUntil(t, a, end, "got ping\r\n")
	fromB, posB := readUntil(t, b, end, "got ping\r\n")
	if !bytes.Equal(fromA, fromB) || posA != posB {
		t.Fatalf("followers differ: %q at %d and %q at %d", fromA, posA, fromB, posB)
	}
}

func TestFollowerSendsNothingToTheTerminal(t *testing.T) {
	dir := startHost(t, Spec{Argv: self("echo"), Env: []string{}, Terminal: true})
	conn, err := net.Dial("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	json.NewEncoder(conn).Encode(request{Op: "follow"})
	var first response
	if err := json.NewDecoder(conn).Decode(&first); err != nil || !first.OK {
		t.Fatalf("follow: %+v, %v", first, err)
	}
	// Requests and raw bytes on the follow connection.
	json.NewEncoder(conn).Encode(request{Op: "input", Data: []byte("hello\n")})
	conn.Write([]byte("boom\n"))
	time.Sleep(200 * time.Millisecond)

	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Input([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the answer to ping", func() bool { return bytes.Contains(read(t, dir, TerminalLog), []byte("got ping")) })
	time.Sleep(200 * time.Millisecond)
	log := read(t, dir, TerminalLog)
	if bytes.Contains(log, []byte("hello")) || bytes.Contains(log, []byte("boom")) {
		t.Fatalf("what the follower sent reached the terminal:\n%q", log)
	}
}

func TestSlowFollowerIsCutOff(t *testing.T) {
	// Far more than followLimit and the socket's buffers.
	dir := startHost(t, Spec{Argv: self("flood", "800000"), Env: []string{}, Terminal: true, LogLimit: 64 << 10})
	s, _ := follow(t, dir, "", 0)
	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Input([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	// The follower reads nothing, and the command's output still reaches
	// the log in full.
	waitFor(t, "the command to finish its output", func() bool {
		return bytes.HasSuffix(read(t, dir, TerminalLog), []byte("00799999\r\nflooded\r\n"))
	})
	if Gone(dir) {
		t.Fatal("the command ended; it should still be answering input")
	}
	// The follower was cut off: its stream ends with an error, not the end.
	s.conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	var n int
	for {
		ch, err := s.Next()
		if err == nil {
			n += len(ch.Data)
			continue
		}
		var he *hostError
		if errors.Is(err, io.EOF) || (!errors.As(err, &he) && !errors.Is(err, io.ErrUnexpectedEOF)) {
			t.Fatalf("after %d bytes the stream ended with %v; want it cut off", n, err)
		}
		if he != nil && !strings.Contains(he.msg, "fell behind") {
			t.Fatalf("cut off with %q", he.msg)
		}
		break
	}
	if n >= 800000*10 {
		t.Fatalf("the slow follower got all %d bytes", n)
	}
	// Following again from the start gives the end of the log as a reset.
	_, ch := follow(t, dir, "", 0)
	if !ch.Reset {
		t.Fatal("following again: no reset")
	}
}

func TestFollowOnPipes(t *testing.T) {
	dir := startHost(t, Spec{Argv: self("echo"), Env: []string{}})
	if _, err := FollowTerminal(dir, "", 0); err == nil || !strings.Contains(err.Error(), "no terminal") {
		t.Fatalf("follow on pipes: %v", err)
	}
}

func TestFollowerGetsTheEnd(t *testing.T) {
	// More than the socket's buffers hold, and less than followLimit.
	dir := startHost(t, Spec{Argv: self("flood", "50000"), Env: []string{}, Terminal: true, LogLimit: 16 << 10})
	s, first := follow(t, dir, "", 0)
	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Input([]byte("go\n\x04")); err != nil { // then end of input: the command exits
		t.Fatal(err)
	}
	// Start reading only after the command has ended: what is still held
	// for the follower must reach it before the host exits.
	waitFor(t, "the command to exit", func() bool {
		b, err := os.ReadFile(dir + "/" + PIDFile)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && pid > 0 && syscall.Kill(pid, 0) != nil
	})
	time.Sleep(200 * time.Millisecond)
	s.conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	got, pos := first.Data, first.Pos
	for {
		ch, err := s.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("after %d bytes: %v", len(got), err)
		}
		got, pos = append(got, ch.Data...), ch.Pos
	}
	if uint64(len(got)) != pos || !bytes.Contains(got[max(0, len(got)-64):], []byte("flooded\r\n")) {
		t.Fatalf("the stream ended at %d with %d bytes, last %q", pos, len(got), got[max(0, len(got)-64):])
	}
}

func TestOversizedWriteKeepsTheLogContinuous(t *testing.T) {
	l, err := openBounded(t.TempDir()+"/log", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var all []byte
	for i, n := range []int{60, 30, 250, 10} {
		p := bytes.Repeat([]byte{byte('a' + i)}, n)
		for j := range p {
			p[j] += byte(j % 2) // so that a shift shows
		}
		if _, err := l.Write(p); err != nil {
			t.Fatal(err)
		}
		all = append(all, p...)
		b, err := os.ReadFile(l.path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasSuffix(all, b) || l.pos != uint64(len(all)) {
			t.Fatalf("after write %d: the log is not the end of what was written", i)
		}
	}
}
