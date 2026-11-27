package main

import (
	"bufio"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/yousan/hataori/internal/surface"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

// A daemon in a process of its own (which a test can kill) reaches the
// test's fake surface through this bridge: the service lives in the test,
// as a real one outlives its bot, and the daemon's driver calls it over a
// Unix socket. While no daemon is attached, the service delivers nothing,
// as a real one delivers nothing to a bot that is offline; Since still
// returns what was posted.
const remoteSurface = "remote-surface"

// service is the test's side: the fake driver and the socket it serves.
type service struct {
	driver   *fakesurface.Driver
	sock     string
	mu       sync.Mutex
	attached *gob.Encoder // the stream to the attached daemon, if any
}

// Message, Choice and Command make the service the fake driver's sink: an
// inbound message goes to the attached daemon, or nowhere.
func (s *service) Message(in surface.Inbound) { s.push(event{Message: &in}) }
func (s *service) Choice(c surface.Selection) { s.push(event{Choice: &c}) }
func (s *service) Command(c surface.Command)  { s.push(event{Command: &c}) }

type event struct {
	Message *surface.Inbound
	Choice  *surface.Selection
	Command *surface.Command
}

func (s *service) push(e event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attached != nil {
		s.attached.Encode(e)
	}
}

// startService serves d on a socket until the test ends.
func startService(t *testing.T, d *fakesurface.Driver) *service {
	t.Helper()
	dir, err := os.MkdirTemp("", "hataori-svc-") // a short path, for the socket
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s := &service{driver: d, sock: filepath.Join(dir, "s")}
	ln, err := net.Listen("unix", s.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := rpc.NewServer()
	srv.RegisterName("Surface", &calls{d})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx, s); close(done) }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(srv, c)
		}
	}()
	t.Cleanup(func() { ln.Close(); cancel(); <-done })
	return s
}

// serve takes one connection: 'E' is the daemon's inbound stream, 'R' its
// calls.
func (s *service) serve(srv *rpc.Server, c net.Conn) {
	r := bufio.NewReader(c)
	kind, err := r.ReadByte()
	if err != nil {
		c.Close()
		return
	}
	if kind == 'R' {
		srv.ServeConn(bufConn{r, c})
		return
	}
	enc := gob.NewEncoder(c)
	s.mu.Lock()
	s.attached = enc
	s.mu.Unlock()
	r.ReadByte() // blocks until the daemon goes away
	s.mu.Lock()
	if s.attached == enc {
		s.attached = nil
	}
	s.mu.Unlock()
	c.Close()
}

// bufConn reads through the reader that has taken the connection's first
// byte.
type bufConn struct {
	r *bufio.Reader
	net.Conn
}

func (b bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (s *service) isAttached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attached != nil
}

// calls are the driver's calls, served to the daemon.
type calls struct{ d *fakesurface.Driver }

type PostArgs struct {
	Conv surface.Conversation
	Key  string
	M    surface.Message
}
type EditArgs struct {
	Ref surface.MessageRef
	M   surface.Message
}
type StatusArgs struct {
	Ref surface.MessageRef
	S   surface.Status
}
type ThreadArgs struct {
	Conv surface.Conversation
	T    surface.ThreadMeta
}
type OpenArgs struct {
	Parent surface.Conversation
	Title  string
}
type SinceArgs struct {
	Conv   surface.Conversation
	Cursor string
}

var bg = context.Background()

func (c *calls) Post(a PostArgs, r *surface.MessageRef) (err error) {
	*r, err = c.d.Post(bg, a.Conv, a.Key, a.M)
	return
}
func (c *calls) Edit(a EditArgs, _ *bool) error             { return c.d.Edit(bg, a.Ref, a.M) }
func (c *calls) Delete(a surface.MessageRef, _ *bool) error { return c.d.Delete(bg, a) }
func (c *calls) SetStatus(a StatusArgs, _ *bool) error      { return c.d.SetStatus(bg, a.Ref, a.S) }
func (c *calls) SetThread(a ThreadArgs, _ *bool) error      { return c.d.SetThread(bg, a.Conv, a.T) }
func (c *calls) OpenThread(a OpenArgs, r *surface.Conversation) (err error) {
	*r, err = c.d.OpenThread(bg, a.Parent, a.Title)
	return
}
func (c *calls) OpenDirect(who string, r *surface.Conversation) (err error) {
	*r, err = c.d.OpenDirect(bg, who)
	return
}
func (c *calls) Since(a SinceArgs, r *[]surface.Inbound) (err error) {
	*r, err = c.d.Since(bg, a.Conv, a.Cursor)
	return
}
func (c *calls) Limits(_ bool, r *surface.Limits) error { *r = c.d.Limits(); return nil }

// remote is the daemon's side: a surface.Driver that calls the service.
type remote struct {
	sock string
	mu   sync.Mutex
	c    *rpc.Client
}

func remoteDriver(_ *os.Root, settings []byte) (surface.Driver, error) {
	var s struct{ Socket string }
	if err := json.Unmarshal(settings, &s); err != nil || s.Socket == "" {
		return nil, errors.New("remote surface: no socket")
	}
	return &remote{sock: s.Socket}, nil
}

func (d *remote) call(method string, args, reply any) error {
	d.mu.Lock()
	if d.c == nil {
		conn, err := net.Dial("unix", d.sock)
		if err != nil {
			d.mu.Unlock()
			return err
		}
		conn.Write([]byte{'R'})
		d.c = rpc.NewClient(conn)
	}
	c := d.c
	d.mu.Unlock()
	return c.Call("Surface."+method, args, reply)
}

func (d *remote) Run(ctx context.Context, sink surface.Sink) error {
	conn, err := net.Dial("unix", d.sock)
	if err != nil {
		return err
	}
	conn.Write([]byte{'E'})
	go func() { <-ctx.Done(); conn.Close() }()
	if r, ok := sink.(surface.Reconnected); ok {
		r.Connected()
	}
	dec := gob.NewDecoder(conn)
	for {
		var e event
		if err := dec.Decode(&e); err != nil {
			return ctx.Err()
		}
		switch {
		case e.Message != nil:
			sink.Message(*e.Message)
		case e.Choice != nil:
			sink.Choice(*e.Choice)
		case e.Command != nil:
			sink.Command(*e.Command)
		}
	}
}

func (d *remote) Since(_ context.Context, conv surface.Conversation, cursor string) ([]surface.Inbound, error) {
	var r []surface.Inbound
	return r, d.call("Since", SinceArgs{conv, cursor}, &r)
}
func (d *remote) OpenThread(_ context.Context, parent surface.Conversation, title string) (surface.Conversation, error) {
	var r surface.Conversation
	return r, d.call("OpenThread", OpenArgs{parent, title}, &r)
}
func (d *remote) OpenDirect(_ context.Context, who string) (surface.Conversation, error) {
	var r surface.Conversation
	return r, d.call("OpenDirect", who, &r)
}
func (d *remote) Post(_ context.Context, conv surface.Conversation, key string, m surface.Message) (surface.MessageRef, error) {
	var r surface.MessageRef
	return r, d.call("Post", PostArgs{conv, key, m}, &r)
}
func (d *remote) Edit(_ context.Context, ref surface.MessageRef, m surface.Message) error {
	return d.call("Edit", EditArgs{ref, m}, new(bool))
}
func (d *remote) Delete(_ context.Context, ref surface.MessageRef) error {
	return d.call("Delete", ref, new(bool))
}
func (d *remote) SetStatus(_ context.Context, ref surface.MessageRef, s surface.Status) error {
	return d.call("SetStatus", StatusArgs{ref, s}, new(bool))
}
func (d *remote) SetThread(_ context.Context, conv surface.Conversation, t surface.ThreadMeta) error {
	return d.call("SetThread", ThreadArgs{conv, t}, new(bool))
}
func (d *remote) Limits() surface.Limits {
	var r surface.Limits
	d.call("Limits", false, &r)
	return r
}
