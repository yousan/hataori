package host

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// HookEnvVar names the variable that tells `hataori hook` where its host
// listens. The caller puts HookEnv(stateDir) into Spec.Env; the host adds
// nothing to the command's environment by itself.
const HookEnvVar = "HATAORI_HOST_SOCKET"

// HookEnv returns the environment entry for the hooks of the command that
// a host in stateDir runs.
func HookEnv(stateDir string) string { return HookEnvVar + "=" + defaultSocket(stateDir) }

// A call is written to events.ndjson as one line:
//
//	{"hook":"<id>","wait":<true|false>,"payload":<payload>}
//
// The payload is copied as it is, without being read: when it is JSON the
// line is JSON. Trailing newlines are dropped and any other CR or LF byte
// becomes a space, so the call stays one line; inside a JSON value those
// bytes can only be whitespace.

// CallLine is a call as the host writes it to events.ndjson, without the
// newline. A reader that learns of a waiting call another way (Waiting,
// after a restart) can build the same line from its id and payload.
func CallLine(id string, wait bool, payload []byte) []byte {
	head := fmt.Sprintf(`{"hook":%q,"wait":%t,"payload":`, id, wait)
	return append(append([]byte(head), oneLine(payload)...), '}')
}

// WaitingCall is a call that waits for an answer.
type WaitingCall struct {
	ID      string `json:"id"`
	Payload []byte `json:"payload"`
}

type callTable struct {
	mu      sync.Mutex
	prefix  string
	next    uint64
	waiting map[string]*waitingCall
	order   []string
}

type waitingCall struct {
	payload []byte
	answer  chan []byte // buffered, one answer
}

func (t *callTable) newID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.prefix == "" {
		t.prefix = strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	t.next++
	return t.prefix + "-" + strconv.FormatUint(t.next, 10)
}

func (t *callTable) add(id string, payload []byte) *waitingCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.waiting == nil {
		t.waiting = map[string]*waitingCall{}
	}
	w := &waitingCall{payload: payload, answer: make(chan []byte, 1)}
	t.waiting[id] = w
	t.order = append(t.order, id)
	return w
}

func (t *callTable) remove(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.waiting, id)
	for i, o := range t.order {
		if o == id {
			t.order = append(t.order[:i], t.order[i+1:]...)
			break
		}
	}
}

func (t *callTable) list() []WaitingCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []WaitingCall{}
	for _, id := range t.order {
		out = append(out, WaitingCall{ID: id, Payload: t.waiting[id].payload})
	}
	return out
}

// answer hands data to the waiting call id, once.
func (t *callTable) answer(id string, data []byte) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	w, ok := t.waiting[id]
	if !ok {
		return false
	}
	delete(t.waiting, id)
	for i, o := range t.order {
		if o == id {
			t.order = append(t.order[:i], t.order[i+1:]...)
			break
		}
	}
	w.answer <- append([]byte(nil), data...)
	return true
}

// oneLine makes a payload one line: trailing newlines dropped, other CR and
// LF bytes turned into spaces.
func oneLine(p []byte) []byte {
	p = bytes.TrimRight(p, "\r\n")
	if bytes.IndexAny(p, "\r\n") < 0 {
		return p
	}
	out := append([]byte(nil), p...) // byte by byte: other bytes stay as they are
	for i, b := range out {
		if b == '\r' || b == '\n' {
			out[i] = ' '
		}
	}
	return out
}

// call records a hook call and, for a waiting one, answers it when the
// answer arrives or gives up when the hook goes away.
func (h *host) call(c net.Conn, req request) response {
	id := h.calls.newID()
	payload := oneLine(req.Data)
	var w *waitingCall
	if req.Wait {
		w = h.calls.add(id, payload) // before the line, so an answer to it finds it
	}
	if err := h.events.append(CallLine(id, req.Wait, payload)); err != nil {
		h.calls.remove(id)
		return response{Error: err.Error()}
	}
	if w == nil {
		return response{OK: true, ID: id}
	}
	gone := make(chan struct{})
	go func() { // the hook sends nothing more; EOF means it gave up
		io.Copy(io.Discard, c)
		close(gone)
	}()
	select {
	case a := <-w.answer:
		return response{OK: true, ID: id, Data: a}
	case <-gone:
		h.calls.remove(id)
		return response{Error: "the hook went away"}
	case <-h.done:
		h.calls.remove(id)
		return response{Error: "the command has exited"}
	}
}

// Waiting returns the calls that wait for an answer, oldest first.
func (c *Client) Waiting() ([]WaitingCall, error) {
	resp, err := c.call(request{Op: "waiting"})
	return resp.Calls, err
}

// Answer answers the waiting call id; the hook prints data and exits 0. An
// id that is not waiting is refused.
func (c *Client) Answer(id string, data []byte) error {
	_, err := c.call(request{Op: "answer", ID: id, Data: data})
	return err
}

// Exit statuses of `hataori hook`. 2 is avoided: agents commonly read it
// as "block this action".
const (
	HookOK      = 0  // recorded; with --wait, the answer was printed
	HookGone    = 3  // no host, or it went away before answering
	HookTimeout = 4  // no answer (or no host reached) before --timeout
	HookOutput  = 5  // the answer arrived but could not be printed
	HookUsage   = 64 // bad arguments
)

// ErrNoHost and ErrTimeout are the failures of Call.
var (
	ErrNoHost  = errors.New("host: no host to take the call")
	ErrTimeout = errors.New("host: no answer before the deadline")
)

// Call hands payload to the host listening on socket. Without wait it
// returns once the call is recorded; with wait, the answer, or ErrTimeout
// after timeout.
func Call(socket string, payload []byte, wait bool, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout) // covers connecting too
	conn, err := net.DialTimeout("unix", socket, min(5*time.Second, timeout))
	if err != nil {
		return nil, callError(err)
	}
	defer conn.Close()
	conn.SetDeadline(deadline)
	if err := json.NewEncoder(conn).Encode(request{Op: "call", Wait: wait, Data: payload}); err != nil {
		return nil, callError(err)
	}
	var resp response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return nil, callError(err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("%w: %s", ErrNoHost, resp.Error)
	}
	return resp.Data, nil
}

func callError(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ErrTimeout
	}
	return fmt.Errorf("%w: %v", ErrNoHost, err)
}

// HookUsageText documents `hataori hook`.
const HookUsageText = `usage: hataori hook [--wait] [--timeout DURATION]

Reads a payload on stdin and hands it to the session host named by
$` + HookEnvVar + `. With --wait, prints the answer to the call and exits.

Exit status: 0 recorded (with --wait: answered, answer printed);
3 no host, or it went away; 4 nothing before --timeout (default 1h);
5 the answer could not be printed; 64 bad arguments. Nothing is printed
unless an answer arrived.
`

// HookMain is `hataori hook`.
func HookMain(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	wait := fs.Bool("wait", false, "")
	timeout := fs.Duration("timeout", time.Hour, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *timeout <= 0 {
		fmt.Fprint(stderr, HookUsageText)
		return HookUsage
	}
	socket := getenv(HookEnvVar)
	if socket == "" {
		return HookGone
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return HookGone
	}
	answer, err := Call(socket, payload, *wait, *timeout)
	switch {
	case errors.Is(err, ErrTimeout):
		return HookTimeout
	case err != nil:
		return HookGone
	}
	if n, err := stdout.Write(answer); err != nil || n != len(answer) {
		return HookOutput
	}
	return HookOK
}
