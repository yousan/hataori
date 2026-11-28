package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/workerproto"
)

func (r *Resident) openLogin(ctx context.Context, p *workerproto.Connection, data []byte) (workerproto.Message, workerproto.AgentConfig, *loginAttempt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var route workerproto.Message
	json.Unmarshal(data, &route)
	reply := workerproto.Message{Kind: "refusal", ID: "invalid", Binding: r.binding, Op: "login_start"}
	if workerproto.ValidID(route.ID) {
		reply.ID = route.ID
	}
	deny := func() (workerproto.Message, workerproto.AgentConfig, *loginAttempt) {
		reply.Body, _ = json.Marshal(failure("denied", "login attempt is unavailable"))
		return reply, workerproto.AgentConfig{}, nil
	}
	if p == nil || p.Hello().Binding != r.binding || r.closed || !r.ready || r.visibleRoot() != nil {
		return deny()
	}
	m, err := p.Decode(data, nil)
	if err != nil || m.Kind != "request" || m.Op != "login_start" {
		return deny()
	}
	var b workerproto.Login
	json.Unmarshal(m.Body, &b)
	a, ok := r.adapters[b.AgentConfig]
	if !ok || r.loginAttempts[b.AgentConfig] != nil {
		return deny()
	}
	cfg := r.configs[b.AgentConfig]
	guard := r.options.LoginGuard
	if len(guard) == 0 {
		guard = []string{r.options.HostCommand.Path, "worker", "login-guard"}
	}
	timeout := r.options.LoginTimeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	f, err := startLoginAttempt(ctx, a, agent.Spec{Dir: r.dir, ConfigDir: cfg.ConfigDir, NoProjectSettings: true}, guard, timeout)
	if err != nil {
		return deny()
	}
	if r.loginAttempts == nil {
		r.loginAttempts = map[workerproto.AgentConfig]*loginAttempt{}
	}
	r.loginAttempts[b.AgentConfig] = f
	reply.Kind, reply.ID = "reply", m.ID
	reply.Body, _ = json.Marshal(workerproto.LoginAttempt{Attempt: b.Attempt})
	return reply, b.AgentConfig, f
}

// serveLogin consumes one connection, never Dispatch's durable request path.
// Losing the connection closes the child guard; no login state survives it.
func (r *Resident) serveLogin(ctx context.Context, c net.Conn, reader *bufio.Reader, p *workerproto.Connection, data []byte) {
	reply, key, f := r.openLogin(ctx, p, data)
	if f == nil {
		sendFrame(c, reply)
		return
	}
	defer func() {
		f.stop()
		r.mu.Lock()
		if r.loginAttempts[key] == f {
			delete(r.loginAttempts, key)
		}
		r.mu.Unlock()
	}()
	if sendFrame(c, reply) != nil {
		return
	}
	var attempt workerproto.LoginAttempt
	json.Unmarshal(reply.Body, &attempt)
	var sendMu sync.Mutex
	var cancelling atomic.Bool
	release := func() {
		f.stop()
		r.mu.Lock()
		if r.loginAttempts[key] == f {
			delete(r.loginAttempts, key)
		}
		r.mu.Unlock()
	}
	send := func(m workerproto.Message) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return sendFrame(c, m)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-done:
		}
	}()
	writerDone := make(chan struct{})
	defer func() { c.Close(); f.stop(); <-writerDone }()
	go func() {
		defer close(writerDone)
		defer func() {
			if !cancelling.Load() {
				c.Close()
			}
		}()
		for u := range f.updates {
			if cancelling.Load() {
				return
			}
			d := workerproto.LoginData{Link: u.Step.Link, ShowCode: u.Step.ShowCode, WantsCode: u.Step.WantsCode && !u.NoCode, NoCode: u.NoCode, Refused: u.Step.Refused}
			if u.Done {
				release() // completion is also a remote cleanup barrier
				d.Result = "failed"
				if u.LoggedIn {
					d.Result = "logged_in"
				}
			}
			if !workerproto.ValidLoginData(d) {
				return
			}
			payload, _ := json.Marshal(d)
			m := reply
			m.Kind = "stream"
			m.Body, _ = json.Marshal(workerproto.LoginStream{Attempt: attempt.Attempt, State: "data", Data: payload})
			if send(m) != nil {
				return
			}
			if u.Done {
				m.Body, _ = json.Marshal(workerproto.LoginStream{Attempt: attempt.Attempt, State: "complete"})
				send(m)
				return
			}
		}
	}()
	requests := make(chan workerproto.Message, 1)
	go func() {
		defer close(requests)
		defer c.Close()
		defer f.cancel()
		for {
			c.SetReadDeadline(time.Now().Add(10 * time.Minute))
			data, err := readFrame(reader)
			if err != nil {
				return
			}
			m, err := p.Decode(data, nil)
			if err != nil || m.Kind != "request" {
				return
			}
			if m.Op == "login_cancel" {
				var b workerproto.LoginAttempt
				json.Unmarshal(m.Body, &b)
				if b.Attempt != attempt.Attempt {
					return
				}
				cancelling.Store(true)
				f.cancel() // independent of a CLI stdin write that may be blocked
			}
			select {
			case requests <- m:
			default:
				return // bounded inbound queue; no blocked reader hides EOF
			}
		}
	}()
	for m := range requests {
		switch m.Op {
		case "login_input":
			var input workerproto.LoginInput
			json.Unmarshal(m.Body, &input)
			if input.Attempt != attempt.Attempt {
				return
			}
			if f.code(string(input.Data)) != nil {
				if cancelling.Load() {
					continue
				}
				return
			}
		case "login_cancel":
			var cancel workerproto.LoginAttempt
			json.Unmarshal(m.Body, &cancel)
			if cancel.Attempt != attempt.Attempt {
				return
			}
			release()
			m.Kind = "reply"
			m.Body, _ = json.Marshal(attempt)
			send(m) // acknowledgement means the old command/reservation is gone
			return
		default:
			return
		}
		m.Kind = "reply"
		m.Body, _ = json.Marshal(attempt)
		if send(m) != nil {
			return
		}
	}
}
