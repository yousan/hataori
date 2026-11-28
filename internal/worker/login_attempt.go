package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/yousan/hataori/internal/agent"
)

const loginLineLimit = 16 * 1024

var errLoginNotWaiting = errors.New("worker login: not waiting for a code")

// loginUpdate never enters the resident journal or a session host. The caller
// must send it only on the attempt's live connection, without diagnostic logs.
type loginUpdate struct {
	Step     agent.LoginStep
	NoCode   bool
	Done     bool
	LoggedIn bool
}

// loginAttempt owns one child and its private pipes. It retains no request,
// receipt, person identity or bot credentials. Closing the guard pipe ends
// only this child's group, without looking up or signalling a saved PID.
type loginAttempt struct {
	mu      sync.Mutex
	login   agent.LoginFlow
	stdin   io.WriteCloser
	command *loginCommand
	cancel  context.CancelFunc
	ctx     context.Context
	done    chan struct{}
	updates chan loginUpdate
	noCode  bool
	waiting bool
	ended   bool
}

// startLoginAttempt requires an EOF guard on every platform: a resident crash
// closes the inherited pipe even when the CLI does not watch standard input.
// guard is the installed executable's login-guard command, never peer data.
func startLoginAttempt(parent context.Context, a agent.Adapter, spec agent.Spec, guard []string, timeout time.Duration) (*loginAttempt, error) {
	lf, ok := a.(agent.LoginFlow)
	verifier, canVerify := a.(agent.WorkerLoginStatus)
	if !ok || !canVerify || len(guard) == 0 || timeout <= 0 || timeout > 10*time.Minute || parent.Err() != nil {
		return nil, errors.New("worker login: unavailable")
	}
	launch, err := lf.LoginLaunch(spec)
	if err != nil || len(launch.Argv) == 0 || launch.Terminal || len(launch.Files) != 0 {
		return nil, errors.New("worker login: invalid local launch")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	_, codeErr := lf.LoginCode("")
	f := &loginAttempt{login: lf, cancel: cancel, ctx: ctx,
		done: make(chan struct{}), updates: make(chan loginUpdate, 16), noCode: errors.Is(codeErr, agent.ErrNoCodeBack)}
	w := &loginOutput{flow: f}
	command, err := startLoginCommand(ctx, launch, spec.Dir, guard, w)
	if err != nil {
		cancel()
		return nil, err
	}
	f.command, f.stdin = command, command.stdin
	go func() {
		defer close(f.done)
		defer close(f.updates)
		defer cancel()
		clean := command.wait(ctx)
		// Wait also joins the output copier. Decode a final unterminated line
		// before reporting completion, without treating text as success.
		w.flush()
		f.mu.Lock()
		f.waiting, f.ended = false, true
		f.mu.Unlock()
		loggedIn := false
		if clean && ctx.Err() == nil {
			probeCtx, stop := context.WithTimeout(ctx, 2*time.Second)
			status, e := verifier.LoginStatusLaunch(spec)
			if e == nil {
				cmd, e := startLoginCommand(probeCtx, status, spec.Dir, guard, &loginStatusOutput{remaining: loginLineLimit, cancel: stop})
				if e == nil {
					loggedIn = cmd.wait(probeCtx)
				}
			}
			stop()
		}
		// A slow sink must not block process cleanup. Reserve one slot for
		// this terminal update by admitting at most 15 intermediate updates.
		f.updates <- loginUpdate{NoCode: f.noCode, Done: true, LoggedIn: loggedIn}
	}()
	return f, nil
}

func (f *loginAttempt) stop() {
	f.cancel()
	f.command.lifetime.Close()
	<-f.done
}

// code accepts one printable word only when this exact live command requested
// it. The adapter formats the bytes; neither the word nor errors are logged.
func (f *loginAttempt) code(code string) error {
	if code == "" || len(code) > loginLineLimit || strings.IndexFunc(code, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }) >= 0 {
		return errLoginNotWaiting
	}
	f.mu.Lock()
	if f.noCode || !f.waiting || f.ended || f.ctx.Err() != nil {
		f.mu.Unlock()
		return errLoginNotWaiting
	}
	b, err := f.login.LoginCode(code)
	if err != nil || len(b) == 0 || len(b) > loginLineLimit {
		f.mu.Unlock()
		return errLoginNotWaiting
	}
	f.waiting = false
	f.mu.Unlock()
	// A blocked stdin write is bounded by the attempt deadline. stop closes
	// the guard pipe independently, so it can always end the child.
	if _, err := f.stdin.Write(b); err != nil {
		return errors.New("worker login: input was not confirmed")
	}
	return nil
}

type loginOutput struct {
	mu      sync.Mutex
	flow    *loginAttempt
	pending []byte
}

func (w *loginOutput) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(b)
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		take := len(b)
		if i >= 0 {
			take = i
		}
		if len(w.pending)+take > loginLineLimit {
			w.flow.cancel()
			return 0, errors.New("worker login: output limit")
		}
		w.pending = append(w.pending, b[:take]...)
		b = b[take:]
		if i >= 0 {
			w.line(w.pending)
			w.pending = nil
			b = b[1:]
		} else if w.flow.login.LoginDecode(w.pending).WantsCode {
			w.line(w.pending)
			w.pending = nil
		}
	}
	return n, nil
}

func (w *loginOutput) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) > 0 {
		w.line(w.pending)
		w.pending = nil
	}
}

func (w *loginOutput) line(b []byte) {
	f := w.flow
	st := f.login.LoginDecode(b)
	if st == (agent.LoginStep{}) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ended {
		return
	}
	if st.WantsCode && !f.noCode {
		f.waiting = true
	}
	if len(f.updates) >= cap(f.updates)-1 {
		f.cancel()
		return
	}
	f.updates <- loginUpdate{Step: st, NoCode: f.noCode}
}
