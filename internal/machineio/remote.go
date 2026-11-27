package machineio

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/holder"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/workerproto"
)

// Remote must be constructed from the main's durable binding/target. Open
// performs the independently configured transport's verified worker hello.
type Remote struct {
	Binding workerproto.Binding
	Target  workerproto.Target
	Open    func(context.Context) (net.Conn, *workerproto.Connection, error)
	order   atomic.Uint64
}

func operationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (r *Remote) Request(op, key string, body any) (workerproto.Message, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return workerproto.Message{}, err
	}
	return workerproto.Message{Kind: "request", ID: operationID(), Binding: r.Binding, Target: &r.Target, Op: op, Key: key, Body: b}, nil
}

// Call makes one attempt. No request, receipt query, terminal key or operation
// is implicitly retried. A caller recovering an effect supplies its original
// durable request and reconciles a receipt against that request.
func (r *Remote) Call(ctx context.Context, m workerproto.Message) (workerproto.Message, error) {
	if m.Binding != r.Binding || m.Target == nil || *m.Target != r.Target {
		return workerproto.Message{}, errors.New("session RPC: caller target differs")
	}
	data, err := workerproto.Encode(m)
	if err != nil {
		return workerproto.Message{}, &holder.NotSent{Err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, p, err := r.Open(ctx)
	if err != nil {
		return workerproto.Message{}, &holder.NotSent{Err: err}
	}
	defer c.Close()
	// Session I/O requires the 1.2 runner contract before any effect,
	// including safe-start flags unknown to older residents.
	required := m
	required.Op, required.Key, required.Body = "describe", "", json.RawMessage(`{}`)
	probe, _ := workerproto.Encode(required)
	if _, err := p.Decode(probe, &r.Target); err != nil {
		return workerproto.Message{}, &holder.NotSent{Err: err}
	}
	if _, err := p.Decode(data, &r.Target); err != nil {
		return workerproto.Message{}, &holder.NotSent{Err: err}
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	c.SetDeadline(deadline)
	if _, err := io.Copy(c, bytes.NewReader(data)); err != nil {
		return workerproto.Message{}, fmt.Errorf("session RPC: %w", ErrUnknown)
	}
	reader := bufio.NewReaderSize(c, workerproto.MaxFrame)
	b, err := reader.ReadSlice('\n')
	if err != nil || len(b) > workerproto.MaxFrame {
		return workerproto.Message{}, fmt.Errorf("session RPC: %w", ErrUnknown)
	}
	reply, err := p.Decode(b, &r.Target)
	if err != nil {
		return workerproto.Message{}, err
	}
	if reply.ID != m.ID || reply.Op != m.Op || reply.Key != m.Key {
		return workerproto.Message{}, errors.New("session RPC: response does not match request")
	}
	if reply.Kind == "refusal" {
		var refusal workerproto.Refusal
		json.Unmarshal(reply.Body, &refusal)
		// The resident can refuse a lost reply after recording an intent or
		// performing the effect. An envelope refusal is no no-write proof.
		return workerproto.Message{}, &refusal
	}
	if reply.Kind != "reply" {
		return workerproto.Message{}, errors.New("session RPC: unexpected response kind")
	}
	return reply, nil
}

func (r *Remote) query(ctx context.Context, op string, body, out any) error {
	m, err := r.Request(op, "", body)
	if err != nil {
		return err
	}
	reply, err := r.Call(ctx, m)
	if err != nil {
		return err
	}
	return json.Unmarshal(reply.Body, out)
}

// Effect correlates the durable result with exactly the original request.
func (r *Remote) Effect(ctx context.Context, m workerproto.Message) (workerproto.Effect, error) {
	reply, err := r.Call(ctx, m)
	if err != nil {
		return workerproto.Effect{}, err
	}
	var rec workerproto.Receipt
	if err := json.Unmarshal(reply.Body, &rec); err != nil {
		return workerproto.Effect{}, err
	}
	if err := workerproto.CheckReceipt(m, rec); err != nil {
		return workerproto.Effect{}, err
	}
	switch rec.State {
	case "complete":
		return *rec.Result, nil
	case "refused":
		return workerproto.Effect{}, &holder.NotSent{Err: rec.Refusal}
	default:
		return workerproto.Effect{}, ErrUnknown
	}
}

func (r *Remote) Info(ctx context.Context) (workerproto.RunMetadata, error) {
	var info workerproto.RunMetadata
	err := r.query(ctx, "describe", workerproto.Empty{}, &info)
	return info, err
}
func (r *Remote) Metadata(ctx context.Context) (Metadata, error) {
	m, err := r.Info(ctx)
	if err != nil {
		return Metadata{}, err
	}
	return Metadata{Spec: agent.Spec{Dir: m.Dir, StateDir: m.StateDir, ConfigDir: m.ConfigDir, Model: m.Model, Effort: m.Effort, ResumeID: m.Resume, Instructions: m.Instructions, NoProjectSettings: m.NoProjectSettings, Approvals: agent.AllowAll}, Terminal: m.Terminal}, nil
}
func (r *Remote) Source(ctx context.Context, name string) (Source, error) {
	var s workerproto.Source
	err := r.query(ctx, "follow", workerproto.Follow{Source: workerproto.FileSource(name)}, &s)
	if err != nil {
		return Source{}, err
	}
	if s.Source != workerproto.FileSource(name) {
		return Source{}, errors.New("session read: unexpected source")
	}
	return Source{Name: name, Handle: s.Handle, Generation: s.Generation}, nil
}
func (r *Remote) Read(ctx context.Context, s Source, off int64, limit int) (Range, error) {
	if off < 0 || limit <= 0 || limit > MaxRead {
		return Range{}, errors.New("session read: invalid range")
	}
	q := workerproto.Read{Source: workerproto.Source{Source: workerproto.FileSource(s.Name), Handle: s.Handle, Generation: s.Generation}, Offset: uint64(off), Order: r.order.Add(1), Limit: uint32(limit)}
	var b workerproto.Bytes
	if err := r.query(ctx, "read", q, &b); err != nil {
		return Range{}, err
	}
	if err := workerproto.CheckRange(q, b); err != nil {
		return Range{}, err
	}
	if b.State != "data" && b.State != "empty" {
		return Range{}, &ReadError{State: b.State}
	}
	return Range{Source: s, Offset: off, Data: b.Data}, nil
}
func (r *Remote) Write(ctx context.Context, key, turn string, chunks [][]byte) error {
	b := bytes.Join(chunks, nil)
	if len(b) == 0 {
		return nil
	}
	m, err := r.Request("input", key, workerproto.Input{Turn: turn, Data: b})
	if err != nil {
		return err
	}
	_, err = r.Effect(ctx, m)
	return err
}
func (r *Remote) Answer(ctx context.Context, key, turn, request string, data []byte) error {
	m, err := r.Request("answer", key, workerproto.Answer{Turn: turn, Request: request, Data: data})
	if err != nil {
		return err
	}
	_, err = r.Effect(ctx, m)
	return err
}
func (r *Remote) Waiting(ctx context.Context) ([]host.WaitingCall, error) {
	m, err := r.Info(ctx)
	if err != nil {
		return nil, err
	}
	out := []host.WaitingCall{}
	for _, h := range m.Waiting {
		out = append(out, host.WaitingCall{ID: h.ID, At: time.UnixMilli(h.AtMS), Payload: h.Payload})
	}
	return out, nil
}
func (r *Remote) Alive(ctx context.Context) (holder.Liveness, error) {
	m, err := r.Info(ctx)
	if err != nil {
		return holder.Liveness{}, err
	}
	var s *host.Status
	if m.Status != nil {
		st := m.Status
		s = &host.Status{Exited: st.Exited, Code: st.Code, Signal: st.Signal, Stopped: st.Stopped, StopNote: st.StopNote, Error: st.Error, Shutdown: st.Shutdown}
	}
	state := holder.Unknown
	switch m.State {
	case "running":
		state = holder.Running
	case "ended":
		state = holder.Exited
	case "gone":
		state = holder.Gone
	}
	return holder.Liveness{State: state, Status: s}, nil
}
func (r *Remote) End(ctx context.Context, key string, interrupt, terminate time.Duration) (holder.StopResult, error) {
	m, err := r.Request("end_host", key, workerproto.Empty{})
	if err != nil {
		return holder.StopResult{}, err
	}
	_, err = r.Effect(ctx, m)
	return holder.StopResult{}, err
}
