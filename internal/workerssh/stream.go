package workerssh

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yousan/hataori/internal/workerproto"
)

// Stream is a single worker protocol conversation, independent of other
// streams sharing the SSH master. Protocol decodes with receiver-owned targets;
// the transport never grants a current session or interprets application data.
type Stream struct {
	net.Conn
	Protocol   *workerproto.Connection
	reader     *bufio.Reader
	readMu     sync.Mutex
	once       sync.Once
	closed     atomic.Bool
	worker     *Worker
	generation *generation
}

func (s *Stream) Read(p []byte) (int, error) {
	s.readMu.Lock()
	n, err := s.reader.Read(p)
	s.readMu.Unlock()
	if err != nil {
		s.noticeError()
	}
	return n, err
}
func (s *Stream) Write(p []byte) (int, error) {
	n, err := s.Conn.Write(p)
	if err != nil {
		s.noticeError()
	}
	return n, err
}
func (s *Stream) noticeError() {
	if s.closed.Load() {
		return
	}
	select {
	case <-s.generation.ended:
		s.worker.masterEnded(s.generation)
	default:
	}
	// EOF can be a normal end of one session stream. Verify the shared link
	// independently rather than turning a sibling's close into an outage.
	select {
	case s.worker.wake <- struct{}{}:
	default:
	}
}
func (s *Stream) Close() error {
	var err error
	s.once.Do(func() {
		s.closed.Store(true)
		err = s.Conn.Close()
		s.worker.mu.Lock()
		delete(s.generation.streams, s)
		s.worker.mu.Unlock()
	})
	return err
}

func clientHello(binding workerproto.Binding) workerproto.Hello {
	seen := map[string]bool{}
	caps := []string{}
	for _, op := range workerproto.Operations() {
		if !seen[op.Capability] {
			caps = append(caps, op.Capability)
			seen[op.Capability] = true
		}
	}
	return workerproto.Hello{Version: workerproto.Version, Binding: binding, Capabilities: caps, Required: []string{"health", "receipts", "lifecycle"}}
}

func (w *Worker) handshake(ctx context.Context, g *generation) (*Stream, error) {
	ctx, cancel := context.WithTimeout(ctx, w.options.ProbeTimeout)
	defer cancel()
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "unix", g.forward)
	if err != nil {
		return nil, unavailable(g.diagnostic.cause(Forward))
	}
	s := &Stream{Conn: connection, reader: bufio.NewReaderSize(connection, workerproto.MaxFrame), worker: w, generation: g}
	w.mu.Lock()
	if w.current != g || w.ctx.Err() != nil {
		w.mu.Unlock()
		connection.Close()
		return nil, unavailable(Transport)
	}
	g.streams[s] = true
	w.mu.Unlock()
	success := false
	defer func() {
		if !success {
			s.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	connection.SetDeadline(deadline)
	local := clientHello(w.config.Binding)
	if err := writeFrame(connection, local); err != nil {
		return nil, unavailable(g.diagnostic.cause(Forward))
	}
	data, err := readFrame(s.reader)
	if err != nil {
		var failure *Error
		if errors.As(err, &failure) {
			return nil, err
		}
		return nil, unavailable(g.diagnostic.cause(Forward))
	}
	peer, err := workerproto.DecodeHello(data)
	if err != nil {
		// Only classify an untrusted diagnostic; malformed/refused handshakes
		// always stay unavailable and can never grant a binding or readiness.
		var fields map[string]json.RawMessage
		var code string
		if json.Unmarshal(data, &fields) == nil {
			json.Unmarshal(fields["code"], &code)
		}
		if code == "binding" {
			return nil, unavailable(Binding)
		}
		return nil, unavailable(Protocol)
	}
	protocol, err := workerproto.Negotiate(w.config.Binding, local, peer)
	if err != nil {
		var refusal *workerproto.Refusal
		if errors.As(err, &refusal) && refusal.Code == "binding" {
			return nil, unavailable(Binding)
		}
		return nil, unavailable(Protocol)
	}
	select {
	case <-g.ended:
		return nil, unavailable(g.diagnostic.cause(Transport))
	default:
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !stop() {
		return nil, ctx.Err()
	}
	connection.SetDeadline(time.Time{})
	s.Protocol = protocol
	success = true
	return s, nil
}

func readFrame(reader *bufio.Reader) ([]byte, error) {
	data, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(data) > workerproto.MaxFrame {
		return nil, unavailable(Protocol)
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}
func writeFrame(writer io.Writer, value any) error {
	data, err := workerproto.Encode(value)
	if err != nil {
		return err
	}
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func (w *Worker) probe(g *generation) error { return w.probeContext(w.ctx, g) }
func (w *Worker) probeContext(parent context.Context, g *generation) error {
	ctx, cancel := context.WithTimeout(parent, w.options.ProbeTimeout)
	defer cancel()
	stream, err := w.handshake(ctx, g)
	if err != nil {
		return err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	stream.SetDeadline(deadline)
	query := func(op string) (workerproto.Message, error) {
		request := workerproto.Message{Kind: "request", ID: "transport-" + op, Binding: w.config.Binding, Op: op, Body: json.RawMessage(`{}`)}
		if err := writeFrame(stream.Conn, request); err != nil {
			return workerproto.Message{}, unavailable(g.diagnostic.cause(Forward))
		}
		data, err := readFrame(stream.reader)
		if err != nil {
			var failure *Error
			if errors.As(err, &failure) {
				return workerproto.Message{}, err
			}
			return workerproto.Message{}, unavailable(g.diagnostic.cause(Forward))
		}
		reply, err := stream.Protocol.Decode(data, nil)
		if err != nil {
			var refusal *workerproto.Refusal
			if errors.As(err, &refusal) && refusal.Code == "binding" {
				return reply, unavailable(Binding)
			}
			return reply, unavailable(Protocol)
		}
		if reply.ID != request.ID || reply.Op != request.Op || reply.Kind != "reply" {
			return reply, unavailable(Reconciliation)
		}
		return reply, nil
	}
	reply, err := query("health")
	if err != nil {
		return err
	}
	var observation Observation
	if json.Unmarshal(reply.Body, &observation.Health) != nil || !observation.Health.Ready {
		return unavailable(Reconciliation)
	}
	reply, err = query("inventory")
	if err != nil {
		return err
	}
	if json.Unmarshal(reply.Body, &observation.Inventory) != nil || !observation.Inventory.Complete || observation.Inventory.Boot != observation.Health.Boot {
		return unavailable(Reconciliation)
	}
	var expected []workerproto.Target
	if w.config.Expected != nil {
		expected, err = w.config.Expected(ctx)
		if err != nil {
			return unavailable(Reconciliation)
		}
	}
	if err := checkScopedInventory(expected, w.config.AttachmentTarget, observation.Inventory); err != nil {
		return unavailable(Reconciliation)
	}
	if w.config.Reconcile != nil {
		if err := w.config.Reconcile(ctx, observation); err != nil {
			return unavailable(Reconciliation)
		}
	}
	if ctx.Err() != nil {
		return unavailable(Reconciliation)
	}
	return nil
}

// An owner attachment checks only its pinned run. The main still rejects every
// unregistered report; neither mode adopts targets reported by its peer.
func checkScopedInventory(expected []workerproto.Target, target *workerproto.Target, inventory workerproto.Inventory) error {
	if target == nil {
		return workerproto.CheckInventory(expected, inventory)
	}
	if workerproto.CheckTarget(*target, *target) != nil {
		return unavailable(Reconciliation)
	}
	for _, h := range inventory.Hosts {
		if h.Target == *target {
			return nil
		}
	}
	return unavailable(Reconciliation)
}
