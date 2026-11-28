package relay

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/machineio"
	"github.com/yousan/hataori/internal/session"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/workerproto"
)

type WorkerLoginEndpoint struct {
	Binding workerproto.Binding
	Open    func(context.Context) (net.Conn, *workerproto.Connection, error)
}

type workerLoginFlow struct {
	mu              sync.Mutex
	account         session.WorkerLogin
	outage          int64
	login           *machineio.Login
	cancel          context.CancelFunc
	done            chan struct{}
	waiting, noCode bool
	stopping        bool
	link, showCode  string
	expires         time.Time
}

func (r *Relay) workerAccount(ctx context.Context, name string, outage int64) (session.WorkerLogin, bool, error) {
	l, ok, err := session.WorkerOutage(ctx, r.Store.DB, outage)
	if err != nil {
		return l, false, errors.New("relay: cannot read worker outage")
	}
	if ok && l.Agent != name {
		return l, true, ErrNoLogin
	}
	return l, ok, nil
}

func (r *Relay) workerLoginEvent(ctx context.Context, kind control.EventKind, name string, outage int64) (bool, error) {
	l, ok, err := r.workerAccount(ctx, name, outage)
	if err != nil || !ok {
		return ok, err
	}
	if kind == session.KindLoginRestored {
		r.stopWorkerLogin(outage)
		return true, nil
	}
	if !r.Off && r.Notify != nil {
		err = r.startWorker(ctx, l, outage)
	}
	return true, err
}

func (r *Relay) startWorker(ctx context.Context, expected session.WorkerLogin, outage int64) error {
	r.startMu.Lock()
	defer r.startMu.Unlock()
	current, ok, err := session.WorkerOutage(ctx, r.Store.DB, outage)
	if err != nil || !ok || !current.Open || current != expected {
		return ErrBack
	}
	endpoint, ok := r.Workers[current.Machine]
	if !ok || endpoint.Open == nil {
		return ErrNoLogin
	}
	var pinned string
	var configured bool
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT installation,configured FROM machines WHERE key=?`, current.Machine).Scan(&pinned, &configured); err != nil || !configured || pinned == "" || pinned != endpoint.Binding.Worker {
		return ErrNoLogin
	}
	// The configuration name comes only from this outage's affected sessions,
	// not from a DM, worker output or another machine's equal path spelling.
	rows, err := r.Store.DB.QueryContext(ctx, `SELECT DISTINCT x.agent_config FROM login_outage_threads o
		JOIN sessions s ON s.thread_id=o.thread_id JOIN threads t ON t.id=s.thread_id
		JOIN session_machine_runs x ON x.session_id=s.id AND x.machine=t.machine
		WHERE o.outage_id=? AND t.machine=? AND s.agent=? AND json_valid(s.spec)
		AND coalesce(json_extract(s.spec,'$.spec.ConfigDir'),'')=?`, outage, current.Machine, current.Agent, current.Dir)
	if err != nil {
		return ErrNoLogin
	}
	var config string
	count := 0
	for rows.Next() {
		if err = rows.Scan(&config); err != nil {
			break
		}
		count++
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil || count != 1 || !workerproto.ValidID(config) {
		return ErrNoLogin
	}
	if r.stopWorkerLogin(outage) != nil {
		return ErrNoLogin
	}
	if r.timeout() > 10*time.Minute {
		return ErrNoLogin
	}
	flowCtx, cancel := context.WithTimeout(ctx, r.timeout())
	l, err := machineio.StartLogin(flowCtx, endpoint.Binding, endpoint.Open, workerproto.AgentConfig{Agent: current.Agent, Config: config}, current.Dir)
	if err != nil {
		cancel()
		return ErrNoLogin
	}
	f := &workerLoginFlow{account: current, outage: outage, login: l, cancel: cancel, done: make(chan struct{}), expires: time.Now().Add(r.timeout())}
	r.mu.Lock()
	if r.workerFlows == nil {
		r.workerFlows = map[int64]*workerLoginFlow{}
	}
	r.workerFlows[outage] = f
	r.mu.Unlock()
	go r.readWorkerLogin(flowCtx, f)
	return nil
}

func (r *Relay) workerCode(outage int64, code string) error {
	if code == "" || len(code) > 16384 || strings.IndexFunc(code, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }) >= 0 {
		return ErrNotACode
	}
	r.mu.Lock()
	f := r.workerFlows[outage]
	r.mu.Unlock()
	if f == nil {
		return ErrNotWaiting
	}
	f.mu.Lock()
	if !f.waiting || f.noCode {
		f.mu.Unlock()
		return ErrNotWaiting
	}
	f.waiting = false
	f.mu.Unlock()
	if f.login.Code(code) != nil {
		return ErrNotWaiting
	}
	return nil
}

func (r *Relay) readWorkerLogin(ctx context.Context, f *workerLoginFlow) {
	defer close(f.done)
	defer f.cancel()
	defer f.login.Close()
	defer func() {
		r.mu.Lock()
		if r.workerFlows[f.outage] == f {
			delete(r.workerFlows, f.outage)
		}
		r.mu.Unlock()
	}()
	result := ""
	var endErr error
	for {
		d, err := f.login.Next()
		if err != nil {
			endErr = err
			break
		}
		if result != "" {
			endErr = machineio.ErrLoginLost
			break
		}
		if d.Result != "" {
			result = d.Result
			continue
		}
		f.mu.Lock()
		f.noCode = d.NoCode
		if d.WantsCode && !d.NoCode {
			f.waiting = true
		}
		if d.Link != "" {
			f.link = d.Link
		}
		if d.ShowCode != "" {
			f.showCode = d.ShowCode
		}
		u := Update{Agent: f.account.Agent, Outage: f.outage, Link: f.link, ShowCode: f.showCode, NoCode: f.noCode, Expires: f.expires, Refused: d.Refused}
		if d.Refused != "" {
			u.Link, u.ShowCode = "", ""
		}
		f.mu.Unlock()
		r.notify(u)
	}
	f.mu.Lock()
	f.waiting = false
	stopping := f.stopping
	f.link, f.showCode = "", ""
	f.mu.Unlock()
	u := Update{Agent: f.account.Agent, Outage: f.outage, Done: Failed, Detail: "the worker login did not finish and verify its status"}
	if ctx.Err() != nil || stopping {
		u.Done, u.Detail = Stopped, ""
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			u.Done = Expired
		}
	} else if endErr == io.EOF && result == "logged_in" {
		err := r.Store.Update(ctx, func(tx *store.Tx) error {
			closed, err := session.CloseWorkerLoginOutage(ctx, tx, f.outage, f.account)
			if err == nil && !closed {
				return sql.ErrNoRows
			}
			return err
		})
		if err == nil {
			u.Done, u.Detail = LoggedIn, ""
		}
	}
	r.notify(u)
}

func (r *Relay) stopWorkerLogin(outage int64) error {
	r.mu.Lock()
	f := r.workerFlows[outage]
	r.mu.Unlock()
	if f != nil {
		f.mu.Lock()
		f.stopping, f.waiting = true, false
		f.mu.Unlock()
		err := f.login.Cancel()
		f.cancel()
		f.login.Close()
		<-f.done
		return err
	}
	return nil
}

func (r *Relay) stopWorkerLogins() {
	r.mu.Lock()
	ids := make([]int64, 0, len(r.workerFlows))
	for id := range r.workerFlows {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	for _, id := range ids {
		r.stopWorkerLogin(id)
	}
}
