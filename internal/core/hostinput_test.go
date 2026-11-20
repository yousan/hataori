package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/hostterm"
	"github.com/yousan/hataori/internal/people"
)

// typedMarker is typed in the tests; it must be stored nowhere.
const typedMarker = "hataori-typed-marker-91d2"

// fakeInput is a fake lister of one tmux pane, %1.
type fakeInput struct {
	mu      sync.Mutex
	gone    bool          // tmux no longer lists the pane
	ownTmux bool          // tmux reports hataori's command for it
	ownPS   bool          // the process table shows hataori's process on it
	fail    error         // reading the pane fails
	slow    bool          // a check of the process table waits until its context ends
	hold    chan struct{} // Keys says so here, then waits until its context ends
	checks  []bool
	keys    [][]control.HostKey
	buffers int
	dropped []string
}

func (f *fakeInput) Check(ctx context.Context, pane string, _ []int, procs bool) (*control.TmuxPane, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, procs)
	if f.slow && procs && len(f.checks) > 1 {
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		return nil, false, ctx.Err()
	}
	if f.fail != nil {
		return nil, false, f.fail
	}
	if pane != "%1" || f.gone {
		return nil, false, nil
	}
	return &control.TmuxPane{Pane: "%1", Session: "work", Window: "2", Index: "0"}, f.ownTmux || procs && f.ownPS, nil
}

func (f *fakeInput) Keys(ctx context.Context, _, _ string, keys []control.HostKey) error {
	f.mu.Lock()
	f.keys = append(f.keys, keys)
	hold := f.hold
	f.mu.Unlock()
	if hold != nil {
		hold <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (f *fakeInput) NewBuffer() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buffers++
	return fmt.Sprintf("buf-%d", f.buffers), nil
}

func (f *fakeInput) DropBuffer(_ context.Context, b string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped = append(f.dropped, b)
	return nil
}

func (f *fakeInput) set(fn func(*fakeInput)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeInput) sent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.keys)
}

// operator is the owner's caller that may type into host panes.
var operator = control.Caller{Principal: "owner", May: control.OpHost | control.OpHostInput}

// inputFixture is a core with host input on, through in.
func inputFixture(t *testing.T, in *fakeInput) *fixture {
	t.Helper()
	f := newFixture(t, fakeOwner{})
	f.core.HostTerms = &fakeHostTerms{}
	f.core.HostInput = in
	return f
}

// logLines are the lines of the log that hold s.
func logLines(f *fixture, s string) []string {
	var out []string
	for _, l := range strings.Split(f.log.String(), "\n") {
		if strings.Contains(l, s) {
			out = append(out, l)
		}
	}
	return out
}

// Every caller but the owner's with OpHostInput, and every call with host
// input off, is refused, logged, and runs nothing.
func TestOperateHostRefusals(t *testing.T) {
	cases := map[string]struct {
		caller control.Caller
		setup  func(*Core)
		reason string
	}{
		"host input off":                 {operator, func(c *Core) { c.HostInput = nil }, "host input is off"},
		"host terminals off":             {operator, func(c *Core) { c.HostTerms = nil }, "host input is off"},
		"another person":                 {control.Caller{Principal: "someone", May: operator.May}, nil, "not the owner"},
		"a guest":                        {control.Caller{From: "chat:U1", May: operator.May}, nil, "not the owner"},
		"the owner without OpHostInput":  {control.Caller{Principal: "owner", May: control.OpHost | all}, nil, "the caller may not type"},
		"the owner with OpHost only":     {control.Caller{Principal: "owner", May: control.OpHost}, nil, "the caller may not type"},
		"no person, every other op too":  {control.Caller{May: ^control.Ops(0)}, nil, "not the owner"},
		"an instance with no owner":      {control.Caller{May: operator.May}, func(c *Core) { c.People = people.New(nil, "", nil) }, "not the owner"},
		"another person, every op there": {control.Caller{Principal: "someone", May: ^control.Ops(0)}, nil, "not the owner"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := &fakeInput{}
			f := inputFixture(t, in)
			if tc.setup != nil {
				tc.setup(f.core)
			}
			op, err := f.core.As(tc.caller).OperateHost(context.Background(), "%1")
			if !errors.Is(err, ErrForbidden) || op != nil {
				t.Fatalf("%v, %v", op, err)
			}
			lines := logLines(f, "op=OperateHost")
			if len(lines) != 1 || !strings.Contains(lines[0], "control call refused") || !strings.Contains(lines[0], tc.reason) {
				t.Errorf("log %q", f.log.String())
			}
			if len(in.checks) != 0 {
				t.Errorf("a refused caller had tmux read %d times", len(in.checks))
			}
		})
	}
}

// A pane in which hataori's own process runs is refused at the start,
// logged, and not operated; a pane that is not listed is not found.
func TestOperateHostRefusesHataorisOwnPane(t *testing.T) {
	ctx := context.Background()
	for _, own := range []func(*fakeInput){func(f *fakeInput) { f.ownTmux = true }, func(f *fakeInput) { f.ownPS = true }} {
		in := &fakeInput{}
		own(in)
		f := inputFixture(t, in)
		a := f.core.As(operator)
		if _, err := a.OperateHost(ctx, "%1"); !errors.Is(err, ErrForbidden) {
			t.Fatalf("hataori's own pane: %v", err)
		}
		if l := logLines(f, "op=OperateHost"); len(l) != 1 || !strings.Contains(l[0], "pane=%1") || !strings.Contains(l[0], "hataori's processes") {
			t.Errorf("log %q", f.log.String())
		}
		if !slices.Equal(in.checks, []bool{true}) {
			t.Errorf("the start's check read the process table: %v", in.checks)
		}
		in.set(func(f *fakeInput) { f.ownTmux, f.ownPS = false, false })
		op, err := a.OperateHost(ctx, "%1") // nothing was left operated
		if err != nil {
			t.Fatal(err)
		}
		op.End()
		<-op.Done()
	}
	f := inputFixture(t, &fakeInput{})
	if _, err := f.core.As(operator).OperateHost(ctx, "%2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a pane that is not listed: %v", err)
	}
}

// A second operation of a pane being operated is ErrBusy, naming the
// person and since when; once the first ends, the pane is free.
func TestOperateHostBusy(t *testing.T) {
	ctx := context.Background()
	f := inputFixture(t, &fakeInput{})
	a := f.core.As(operator)
	first, err := a.OperateHost(ctx, "%1")
	if err != nil {
		t.Fatal(err)
	}
	since := f.core.hostOps.byPane["%1"].since.Format(time.RFC3339)
	_, err = a.OperateHost(ctx, "%1")
	if !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "owner") || !strings.Contains(err.Error(), since) {
		t.Fatalf("a second operation: %v", err)
	}
	first.End()
	<-first.Done()
	second, err := a.OperateHost(ctx, "%1")
	if err != nil {
		t.Fatalf("after the first ended: %v", err)
	}
	second.End()
	<-second.Done()
}

// Each way an operation ends closes Done with its reason, deletes the
// operation's buffer, frees the pane, makes Send ErrEnded, and writes one
// start line and one end line naming the person, the pane and the reason.
func TestOperateHostEnds(t *testing.T) {
	defer func(d time.Duration) { hostCheckEvery = d }(hostCheckEvery)
	cases := map[string]struct {
		want  string
		fast  bool // checks every 20 ms instead of hourly
		idle  time.Duration
		cause func(t *testing.T, op control.HostOperation, cancel func(), stop chan struct{}, in *fakeInput)
	}{
		"End": {want: control.HostEnded, cause: func(_ *testing.T, op control.HostOperation, _ func(), _ chan struct{}, _ *fakeInput) {
			op.End()
		}},
		"its context ends": {want: control.HostClosed, cause: func(_ *testing.T, _ control.HostOperation, cancel func(), _ chan struct{}, _ *fakeInput) {
			cancel()
		}},
		"the daemon stops": {want: control.HostStopping, cause: func(_ *testing.T, _ control.HostOperation, _ func(), stop chan struct{}, _ *fakeInput) {
			close(stop)
		}},
		"the daemon stops, and so the context": {want: control.HostStopping, cause: func(_ *testing.T, _ control.HostOperation, cancel func(), stop chan struct{}, _ *fakeInput) {
			close(stop)
			cancel()
		}},
		"idle": {want: control.HostIdle, idle: 150 * time.Millisecond, cause: func(t *testing.T, op control.HostOperation, _ func(), _ chan struct{}, _ *fakeInput) {
			start := time.Now()
			time.Sleep(100 * time.Millisecond)
			if err := op.Send(context.Background(), []control.HostKey{{Key: "Up"}}); err != nil {
				t.Fatal(err)
			}
			<-op.Done()
			if d := time.Since(start); d < 250*time.Millisecond {
				t.Errorf("a key did not start the idle period again: ended after %v", d)
			}
		}},
		"the pane goes, seen by the check": {want: control.HostGone, fast: true, cause: func(_ *testing.T, _ control.HostOperation, _ func(), _ chan struct{}, in *fakeInput) {
			in.set(func(f *fakeInput) { f.gone = true })
		}},
		"hataori's process starts, seen in the process table": {want: control.HostOwn, fast: true, cause: func(_ *testing.T, _ control.HostOperation, _ func(), _ chan struct{}, in *fakeInput) {
			in.set(func(f *fakeInput) { f.ownPS = true })
		}},
		"the pane goes, seen before a batch": {want: control.HostGone, cause: func(t *testing.T, op control.HostOperation, _ func(), _ chan struct{}, in *fakeInput) {
			in.set(func(f *fakeInput) { f.gone = true })
			if err := op.Send(context.Background(), []control.HostKey{{Key: "Enter"}}); !errors.Is(err, control.ErrEnded) {
				t.Errorf("a batch to a pane that is gone: %v", err)
			}
		}},
		"hataori's command, seen before a batch": {want: control.HostOwn, cause: func(t *testing.T, op control.HostOperation, _ func(), _ chan struct{}, in *fakeInput) {
			in.set(func(f *fakeInput) { f.ownTmux = true })
			if err := op.Send(context.Background(), []control.HostKey{{Key: "Enter"}}); !errors.Is(err, control.ErrEnded) {
				t.Errorf("a batch to hataori's own pane: %v", err)
			}
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			hostCheckEvery = time.Hour
			if tc.fast {
				hostCheckEvery = 20 * time.Millisecond
			}
			in := &fakeInput{}
			f := inputFixture(t, in)
			stop := make(chan struct{})
			f.core.Stopping = stop
			f.core.HostInputIdle = time.Hour
			if tc.idle > 0 {
				f.core.HostInputIdle = tc.idle
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a := f.core.As(operator)
			op, err := a.OperateHost(ctx, "%1")
			if err != nil {
				t.Fatal(err)
			}
			if op.Idle() != f.core.HostInputIdle || op.Reason() != "" {
				t.Errorf("Idle %v, Reason %q", op.Idle(), op.Reason())
			}
			sentBefore := in.sent()
			tc.cause(t, op, cancel, stop, in)
			select {
			case <-op.Done():
			case <-time.After(10 * time.Second):
				t.Fatal("the operation did not end")
			}
			if op.Reason() != tc.want {
				t.Errorf("reason %q, want %q", op.Reason(), tc.want)
			}
			if err := op.Send(context.Background(), []control.HostKey{{Key: "Up"}}); !errors.Is(err, control.ErrEnded) {
				t.Errorf("Send after the end: %v", err)
			}
			if tc.want != control.HostIdle && in.sent() != sentBefore {
				t.Errorf("keys were sent: %v", in.keys)
			}
			if !slices.Contains(in.dropped, "buf-1") {
				t.Errorf("the operation's buffer was not deleted: %v", in.dropped)
			}
			started, ended := logLines(f, "host input started"), logLines(f, "host input ended")
			const where = "person=owner pane=%1 session=work window=2 index=0"
			if len(started) != 1 || !strings.Contains(started[0], where) {
				t.Errorf("start lines %q", started)
			}
			if len(ended) != 1 || !strings.Contains(ended[0], where) || !strings.Contains(ended[0], fmt.Sprintf("reason=%q", tc.want)) &&
				!strings.Contains(ended[0], "reason="+tc.want) {
				t.Errorf("end lines %q", ended)
			}
			again, err := a.OperateHost(context.Background(), "%1")
			if err == nil {
				defer func() { again.End(); <-again.Done() }()
			}
			if tc.want == control.HostStopping {
				if err == nil {
					t.Error("an operation starts while the daemon stops")
				}
			} else if tc.want != control.HostGone && tc.want != control.HostOwn && err != nil {
				t.Errorf("the pane is not free again: %v", err)
			}
		})
	}
}

// A batch is not sent when the pane's command cannot be read, nor when it
// holds a key name off the list; the operation goes on. A batch that is
// sent is checked by the command tmux reports, without the process table.
func TestOperateHostBatches(t *testing.T) {
	ctx := context.Background()
	in := &fakeInput{}
	f := inputFixture(t, in)
	f.core.HostInputIdle = time.Hour
	op, err := f.core.As(operator).OperateHost(ctx, "%1")
	if err != nil {
		t.Fatal(err)
	}
	defer op.End()
	in.set(func(f *fakeInput) { f.fail = errors.New("tmux: server exited unexpectedly") })
	if err := op.Send(ctx, []control.HostKey{{Text: typedMarker}}); err == nil || errors.Is(err, control.ErrEnded) {
		t.Errorf("a batch whose pane cannot be read: %v", err)
	}
	in.set(func(f *fakeInput) { f.fail = nil })
	for _, bad := range [][]control.HostKey{{{Key: "Up"}, {Key: typedMarker}}, {{}}, {{Key: "Up", Text: "x"}}, {{Key: "Space"}}} {
		if err := op.Send(ctx, bad); err == nil || strings.Contains(err.Error(), typedMarker) {
			t.Errorf("batch %v: %v", bad, err)
		}
	}
	if in.sent() != 0 {
		t.Fatalf("sent %v", in.keys)
	}
	in.checks = nil
	keys := []control.HostKey{{Key: "C-c"}, {Text: typedMarker, Paste: true}}
	if err := op.Send(ctx, keys); err != nil {
		t.Fatal(err)
	}
	if in.sent() != 1 || !slices.Equal(in.keys[0], keys) || !slices.Equal(in.checks, []bool{false}) {
		t.Errorf("sent %v after checks %v", in.keys, in.checks)
	}
	select {
	case <-op.Done():
		t.Fatalf("the operation ended: %s", op.Reason())
	default:
	}
	if strings.Contains(f.log.String(), typedMarker) {
		t.Error("the log holds what was typed")
	}
}

// Against a tmux server of the test's own: the text reaches the pane, and
// is found in no log line, event, database row or file under the root.
func TestOperateHostStoresNothingTyped(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("tmux is not installed; CI must run this test")
		}
		t.Skip("no tmux")
	}
	tmpdir, err := os.MkdirTemp("/tmp", "hc") // short on macOS too: a socket path is limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpdir) })
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", tmpdir)
	sock := []string{"-L", fmt.Sprintf("hataori-core-input-test-%d", os.Getpid()), "-f", "/dev/null"}
	tmux := func(args ...string) (string, error) {
		out, err := exec.Command("tmux", slices.Concat(sock, args)...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("tmux %v: %v: %s", args, err, out)
		}
		return string(out), nil
	}
	dir := t.TempDir()
	got, ready := filepath.Join(dir, "got"), filepath.Join(dir, "ready")
	// The pane's terminal is put in raw mode, and only then says it is
	// ready: keys typed before stty would be read in cooked mode, where
	// Enter's \r arrives as \n (#514).
	if _, err := tmux("new-session", "-d", "-s", "typing", "sh", "-c",
		"stty raw -echo; : > "+ready+"; head -c "+fmt.Sprint(len(typedMarker)+1)+" > "+got+"; exec sleep 600"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tmux("kill-server") })
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pane's terminal is not in raw mode after 10 seconds")
		}
	}
	out, err := tmux("display-message", "-p", "-t", "typing:0", "#{pane_id}")
	if err != nil {
		t.Fatal(err)
	}
	pane := strings.TrimSpace(out)

	f := newFixture(t, fakeOwner{})
	lister := &hostterm.Lister{Tmux: sock, Own: "hataori-never-runs", Buffers: "hataori-input-coretest-"}
	f.core.HostTerms, f.core.HostInput = lister, lister
	ctx := context.Background()
	f.running(t, "t1") // a thread, so there are events and rows to look in
	op, err := f.core.As(operator).OperateHost(ctx, pane)
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Send(ctx, []control.HostKey{{Text: typedMarker}, {Key: "Enter"}}); err != nil {
		t.Fatal(err)
	}
	op.End()
	<-op.Done()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, _ := os.ReadFile(got)
		if string(b) == typedMarker+"\r" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pane read %q", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if b, _ := tmux("list-buffers", "-F", "#{buffer_name}"); strings.Contains(b, "hataori-input-") {
		t.Errorf("a buffer is left: %q", b)
	}

	if strings.Contains(f.log.String(), typedMarker) {
		t.Error("a log line holds what was typed")
	}
	if len(logLines(f, "host input started")) != 1 || len(logLines(f, "host input ended")) != 1 {
		t.Errorf("log %s", f.log.String())
	}
	evs, err := f.owner.ThreadEvents(ctx, 1, 0, control.MaxEvents)
	if err != nil || len(evs) == 0 {
		t.Fatal(len(evs), err)
	}
	for _, e := range evs {
		if bytes.Contains(e.Data, []byte(typedMarker)) {
			t.Errorf("event %s holds what was typed", e.Kind)
		}
	}
	tables, err := f.core.Store.DB.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var n string
		tables.Scan(&n)
		names = append(names, n)
	}
	tables.Close()
	for _, n := range names {
		rows, err := f.core.Store.DB.QueryContext(ctx, `SELECT * FROM "`+n+`"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Scan(ptrs...)
			if strings.Contains(fmt.Sprint(vals...), typedMarker) {
				t.Errorf("a row of %s holds what was typed", n)
			}
		}
		rows.Close()
	}
	files := 0
	filepath.WalkDir(f.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		files++
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(typedMarker)) {
			t.Errorf("%s holds what was typed", p)
		}
		return nil
	})
	if files == 0 {
		t.Error("no file under the root to look in")
	}
}

// When the operation ends during a batch, the batch's context ends, so no
// further key of it is sent.
func TestOperateHostEndStopsABatch(t *testing.T) {
	in := &fakeInput{hold: make(chan struct{})}
	f := inputFixture(t, in)
	op, err := f.core.As(operator).OperateHost(context.Background(), "%1")
	if err != nil {
		t.Fatal(err)
	}
	sent := make(chan error, 1)
	go func() { sent <- op.Send(context.Background(), []control.HostKey{{Key: "Up"}, {Key: "Enter"}}) }()
	<-in.hold
	op.End()
	select {
	case err := <-sent:
		if err == nil {
			t.Error("a batch stopped by the end is no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the batch went on after the operation ended")
	}
	select {
	case <-op.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the operation did not end")
	}
}

// A check of the process table that hangs does not hold up the end: the
// watcher still sees the context end at once.
func TestOperateHostSlowCheck(t *testing.T) {
	defer func(d time.Duration) { hostCheckEvery = d }(hostCheckEvery)
	hostCheckEvery = 10 * time.Millisecond
	in := &fakeInput{slow: true}
	f := inputFixture(t, in)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	op, err := f.core.As(operator).OperateHost(ctx, "%1")
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		in.mu.Lock()
		n := len(in.checks)
		in.mu.Unlock()
		if n > 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no check after the start")
		}
	}
	time.Sleep(50 * time.Millisecond) // the check hangs now
	start := time.Now()
	cancel()
	select {
	case <-op.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a hanging check holds up the end")
	}
	if d := time.Since(start); d > time.Second || op.Reason() != control.HostClosed {
		t.Errorf("ended after %v with %q", d, op.Reason())
	}
}

// StopHostInput ends every operation with "stopping" and returns once each
// has logged its end.
func TestStopHostInput(t *testing.T) {
	in := &fakeInput{}
	f := inputFixture(t, in)
	op, err := f.core.As(operator).OperateHost(context.Background(), "%1")
	if err != nil {
		t.Fatal(err)
	}
	f.core.StopHostInput(5 * time.Second)
	select {
	case <-op.Done():
	default:
		t.Fatal("StopHostInput returned before the operation finished")
	}
	if op.Reason() != control.HostStopping || len(logLines(f, "host input ended")) != 1 || !slices.Contains(in.dropped, "buf-1") {
		t.Errorf("reason %q, log %q, dropped %v", op.Reason(), f.log.String(), in.dropped)
	}
	f.core.StopHostInput(time.Second) // none left: returns at once
	if _, err := f.core.As(operator).OperateHost(context.Background(), "%1"); err == nil {
		t.Error("an operation starts after StopHostInput")
	}
}
