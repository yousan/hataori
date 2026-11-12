// Package relay logs an agent in again for the instance's owner, who is
// usually not at the host (design, section 4.7). It runs the agent's own
// login command as a child of the daemon, on pipes, hands the sign-in link
// to the owner and the owner's code to the command, and reports the result.
//
// The link and the code exist only in memory and in what the owner's
// client is given. Nothing here writes either to the database, the event
// log, a log line or a file; the log says only that a code arrived and how
// long it was. The relay is offered to in-process clients only, and only
// to the owner: it is not part of the control API.
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
)

// Errors of the relay's operations.
var (
	ErrForbidden  = errors.New("relay: only the owner may log the agent in")
	ErrNoLogin    = errors.New("relay: the agent cannot be logged in from here")
	ErrNotWaiting = errors.New("relay: not waiting for a code")
	ErrNotACode   = errors.New("relay: a code is one word without spaces")
)

// Outcome is how a login flow ended.
type Outcome string

// The outcomes.
const (
	LoggedIn Outcome = "logged in"  // the command ended cleanly and the agent's status says logged in
	Failed   Outcome = "failed"     // it did not; Detail says how
	Expired  Outcome = "expired"    // no code came in time; the command was stopped
	Restored Outcome = "login back" // the outage closed some other way, such as a login at the host
	Stopped  Outcome = "stopped"    // a new flow replaced it, or the relay ended
)

// Update is what the owner's client is told about a flow. Each flow sends
// one Update with Link (unless the command prints none), any number with
// Refused, and one last with Done.
type Update struct {
	Agent    string
	Link     string    // the sign-in link
	ShowCode string    // a code to show beside it, if the agent gives one
	Expires  time.Time // with Link: until when a code is taken
	Refused  string    // the command's own line refusing a code
	Done     Outcome   // set on the last Update of a flow
	Detail   string    // with Failed: why
}

// Relay runs login flows for the agents of one instance.
type Relay struct {
	Store    *store.Store
	Adapters map[string]agent.Adapter // by registered name
	Owner    control.Principal
	Notify   func(Update)  // the owner's client; called from the relay's goroutines
	Timeout  time.Duration // for the owner's code; 10 minutes when zero
	Off      bool          // the config switched the relay off: outages do not start flows
	Log      *slog.Logger

	mu    sync.Mutex
	flows map[string]*flow // by agent
}

func (r *Relay) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return 10 * time.Minute
}

// flow is one run of an agent's login command.
type flow struct {
	agent   string
	login   agent.LoginFlow
	adapter agent.Adapter
	cmd     *exec.Cmd
	stdin   io.WriteCloser

	mu      sync.Mutex
	waiting bool // the command asked for a code and has not had one since
	done    bool
	end     Outcome // why the relay ended it, if it did
	exited  chan struct{}
}

// Run starts a flow when a login outage opens, and ends a waiting flow
// when the outage closes some other way, until ctx ends. Outages that
// opened before Run are left alone: the owner starts a flow for them.
func (r *Relay) Run(ctx context.Context) error {
	var head uint64
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT coalesce(max(pos), 0) FROM events`).Scan(&head); err != nil {
		return err
	}
	evs := make(chan control.Event)
	errc := make(chan error, 1)
	go func() {
		errc <- r.Store.Follow(ctx, control.Filter{}, head, evs)
		close(evs)
	}()
	defer r.stopAll()
	for e := range evs {
		var d struct {
			Agent string `json:"agent"`
		}
		json.Unmarshal(e.Data, &d)
		switch e.Kind {
		case session.KindLoginLost:
			if !r.Off {
				if err := r.start(ctx, d.Agent); err != nil && !errors.Is(err, ErrNoLogin) {
					r.Log.Error("login relay", "agent", d.Agent, "err", err)
				}
			}
		case session.KindLoginRestored:
			r.end(d.Agent, Restored)
		}
	}
	if err := <-errc; err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// Start starts a new login flow for the agent, as caller, ending any flow
// of the agent that runs. It is the owner's way to try again.
func (r *Relay) Start(ctx context.Context, caller control.Principal, agentName string) error {
	if caller != r.Owner || r.Owner == "" {
		r.Log.Warn("login relay refused", "op", "start", "principal", string(caller))
		return ErrForbidden
	}
	return r.start(ctx, agentName)
}

// Code hands a code to the agent's login command, as caller. It is refused,
// and nothing is written, unless the command has asked for a code and the
// code is one word without spaces.
func (r *Relay) Code(caller control.Principal, agentName, code string) error {
	if caller != r.Owner || r.Owner == "" {
		r.Log.Warn("login relay refused", "op", "code", "principal", string(caller))
		return ErrForbidden
	}
	if code == "" || strings.IndexFunc(code, func(c rune) bool { return unicode.IsSpace(c) || !unicode.IsPrint(c) }) >= 0 {
		return ErrNotACode
	}
	r.mu.Lock()
	f := r.flows[agentName]
	r.mu.Unlock()
	if f == nil {
		return ErrNotWaiting
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.waiting || f.done {
		return ErrNotWaiting
	}
	b, err := f.login.LoginCode(code)
	if err != nil {
		return ErrNotACode
	}
	f.waiting = false
	r.Log.Info("login code arrived", "agent", agentName, "length", len(code))
	if _, err := f.stdin.Write(b); err != nil {
		return fmt.Errorf("relay: hand the code to the login command: %w", err)
	}
	return nil
}

func (r *Relay) start(ctx context.Context, agentName string) error {
	ad := r.Adapters[agentName]
	lf, ok := ad.(agent.LoginFlow)
	if !ok {
		return ErrNoLogin
	}
	l, err := lf.LoginLaunch(agent.Spec{})
	if err != nil {
		return fmt.Errorf("relay: %w", err)
	}
	if len(l.Argv) == 0 || l.Terminal {
		return fmt.Errorf("relay: %s: the login command must run on pipes", agentName)
	}
	r.end(agentName, Stopped)

	cmd := exec.Command(l.Argv[0], l.Argv[1:]...)
	cmd.Env = l.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // its own group, so that all of it can be stopped
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw // the refusal may come on either
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("relay: start the login command: %w", err)
	}
	f := &flow{agent: agentName, login: lf, adapter: ad, cmd: cmd, stdin: stdin, exited: make(chan struct{})}
	r.mu.Lock()
	if r.flows == nil {
		r.flows = map[string]*flow{}
	}
	r.flows[agentName] = f
	r.mu.Unlock()
	r.Log.Info("login relay started", "agent", agentName)

	go func() {
		err := cmd.Wait()
		pw.Close()
		r.finish(f, err)
	}()
	go r.read(f, pr)
	go func() {
		select {
		case <-time.After(r.timeout()):
			r.endFlow(f, Expired)
		case <-f.exited:
		case <-ctx.Done():
			r.endFlow(f, Stopped)
		}
	}()
	return nil
}

// read decodes what the command prints. A line the command has not ended
// yet, such as a prompt, is decoded as well.
func (r *Relay) read(f *flow, out io.Reader) {
	var pending []byte
	buf := make([]byte, 4096)
	for {
		n, err := out.Read(buf)
		pending = append(pending, buf[:n]...)
		for {
			i := bytes.IndexByte(pending, '\n')
			if i < 0 {
				break
			}
			r.line(f, pending[:i])
			pending = pending[i+1:]
		}
		if len(pending) > 0 {
			if st := f.login.LoginDecode(pending); st.WantsCode {
				r.line(f, pending)
				pending = nil
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *Relay) line(f *flow, l []byte) {
	st := f.login.LoginDecode(l)
	f.mu.Lock()
	if f.done {
		f.mu.Unlock()
		return
	}
	if st.WantsCode {
		f.waiting = true
	}
	f.mu.Unlock()
	switch {
	case st.Link != "":
		r.notify(Update{Agent: f.agent, Link: st.Link, ShowCode: st.ShowCode, Expires: time.Now().Add(r.timeout())})
	case st.Refused != "":
		r.notify(Update{Agent: f.agent, Refused: st.Refused})
	}
}

// finish decides a flow whose command ended.
func (r *Relay) finish(f *flow, waitErr error) {
	f.mu.Lock()
	end := f.end
	f.done = true
	f.mu.Unlock()
	close(f.exited)
	r.mu.Lock()
	if r.flows[f.agent] == f {
		delete(r.flows, f.agent)
	}
	r.mu.Unlock()
	if end != "" { // the relay stopped it
		r.Log.Info("login relay ended", "agent", f.agent, "outcome", string(end))
		r.notify(Update{Agent: f.agent, Done: end})
		return
	}
	u := Update{Agent: f.agent, Done: Failed}
	switch {
	case waitErr != nil:
		u.Detail = "the login command failed: " + waitErr.Error()
	default:
		h, err := f.adapter.Probe(context.Background())
		switch {
		case err != nil:
			u.Detail = "the status check failed: " + err.Error()
		case !h.LoggedIn:
			u.Detail = "the login command ended, but the agent is not logged in"
		default:
			u.Done = LoggedIn
			if err := r.Store.Update(context.Background(), func(tx *store.Tx) error {
				_, err := session.CloseLoginOutage(context.Background(), tx, f.agent)
				return err
			}); err != nil {
				r.Log.Error("login relay: closing the outage", "agent", f.agent, "err", err)
			}
		}
	}
	r.Log.Info("login relay ended", "agent", f.agent, "outcome", string(u.Done))
	r.notify(u)
}

// end stops the agent's running flow, if any, as outcome.
func (r *Relay) end(agentName string, outcome Outcome) {
	r.mu.Lock()
	f := r.flows[agentName]
	r.mu.Unlock()
	if f != nil {
		r.endFlow(f, outcome)
	}
}

// endFlow stops the command's whole process group and waits for it to end.
func (r *Relay) endFlow(f *flow, outcome Outcome) {
	f.mu.Lock()
	if f.done || f.end != "" {
		f.mu.Unlock()
		<-f.exited
		return
	}
	f.end = outcome
	f.mu.Unlock()
	syscall.Kill(-f.cmd.Process.Pid, syscall.SIGKILL)
	<-f.exited
}

func (r *Relay) stopAll() {
	r.mu.Lock()
	var fs []*flow
	for _, f := range r.flows {
		fs = append(fs, f)
	}
	r.mu.Unlock()
	for _, f := range fs {
		r.endFlow(f, Stopped)
	}
}

func (r *Relay) notify(u Update) {
	if r.Notify != nil {
		r.Notify(u)
	}
}
