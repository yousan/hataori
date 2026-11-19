package plugin

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/store"
)

// The environment a plugin hataori starts gets, besides Env.
const (
	EnvSocket   = "HATAORI_SOCKET"          // the control socket's path
	EnvToken    = "HATAORI_TOKEN"           // the token of this start; it binds the connection to the plugin's grant
	EnvID       = "HATAORI_PLUGIN_ID"       // the plugin's id
	EnvState    = "HATAORI_PLUGIN_STATE"    // its state directory, kept across starts, such as for its event position
	EnvSettings = "HATAORI_PLUGIN_SETTINGS" // the file holding its config entry's "settings", a JSON object
	EnvProtocol = "HATAORI_PROTOCOL"        // the control protocol the daemon speaks, such as "3.12"
)

// Env is the fixed list of the daemon's environment variables a plugin
// gets, as an agent does. Nothing else of the daemon's environment
// reaches it.
var Env = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE",
	"TZ", "TMPDIR", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME"}

// The defaults of Runner's limits.
const (
	DefaultBackoff    = time.Second
	DefaultMaxBackoff = time.Minute
	DefaultFailures   = 5
	DefaultWindow     = 10 * time.Minute
	DefaultGrace      = 10 * time.Second
	DefaultLineMax    = 4096
)

// MaxExecutable is the size, in bytes, of the largest executable started.
const MaxExecutable = 1 << 30

// Tokens binds connections to callers: the control socket's server
// (wire.Server).
type Tokens interface {
	Issue(api control.API) (string, error)
	Restore(token string, api control.API) error
	Revoke(token string)
}

// Runner runs an instance's plugins (design, section 7.3). It starts each
// one that is not external as a child process, in a process group of its
// own, and starts it again after a backoff when it exits; after Failures
// exits within Window it leaves it stopped and tells the owner once. When
// the context of Start ends, it stops every plugin: Stopping, then SIGTERM
// to its process group, then SIGKILL after Grace. A plugin's failure never
// stops the daemon.
//
// Before every start, the manifest's protocol must be one the daemon
// speaks and the executable's SHA-256 the config's. The bytes that are
// hashed are the bytes that run: the executable is read once into memory,
// hashed there, and written to a new file in a directory of the daemon's
// own (state/plugin-run/<id>/, mode 0700), under a new random name at
// each start, which is the file started. A file put in the place of the
// executable after it was read is not run.
type Runner struct {
	Grants []Grant
	State  string // the instance's state directory, absolute
	Socket string // the control socket's path
	Speaks string // the control protocol the daemon speaks, such as "3.12"
	Tokens Tokens
	As     func(control.Caller) control.API // the core's API as seen by a caller, bound for the hooks (BindAs)
	// ReadToken reads an external plugin's token file, a path relative to
	// the config directory.
	ReadToken func(file string) ([]byte, error)
	// Outages keeps whether a plugin stays stopped, so that the owner is
	// told once per outage, also across restarts of the daemon.
	Outages *store.Store
	// Tell tells the owner, once per key whatever the number of calls:
	// the direct message of the login alert (section 4.7). An error is
	// tried again. Nil: the log line only.
	Tell func(ctx context.Context, key, text string) error
	// Stopping tells a running plugin that it is stopping, before it gets
	// SIGTERM; it has until ctx ends, the grace period (Dispatcher.Stopping,
	// plugin_stopping). Nil tells nothing.
	Stopping func(ctx context.Context, g Grant)
	Log      *slog.Logger

	Backoff, MaxBackoff time.Duration // the wait before the first start again, doubled each time up to MaxBackoff
	Failures            int           // exits within Window after which a plugin stays stopped
	Window              time.Duration // a run as long as this ends the plugin's outage and its count of exits
	Grace               time.Duration // between SIGTERM and SIGKILL
	LineMax             int           // the longest line of a plugin's output logged; longer ones are cut

	// Environ is the daemon's environment; os.Environ when nil.
	Environ func() []string
	// afterRead, when a test sets it, runs between reading the executable
	// and starting it.
	afterRead func(g Grant)

	wg     sync.WaitGroup
	mu     sync.Mutex
	groups map[int]string // the process groups of the plugins running now: their ids
}

// Start binds each external plugin's token to its caller, then starts the
// other plugins, each in its own goroutine, until ctx ends. An external
// plugin's token that cannot be read, or that another external plugin's
// file holds too, is an error naming the key, and starts nothing. Wait
// waits for every plugin to have stopped.
func (r *Runner) Start(ctx context.Context) error {
	seen := map[string]string{} // token: the key of the file that holds it
	for i, g := range r.Grants {
		if !g.External {
			continue
		}
		b, err := r.ReadToken(g.TokenFile)
		if err != nil {
			return fmt.Errorf("config: key %q: plugin %q: %w", fmt.Sprintf("plugins[%d].token_file", i), g.ID, err)
		}
		token := strings.TrimSpace(string(b))
		key := fmt.Sprintf("plugins[%d].token_file", i)
		if token == "" {
			return fmt.Errorf("config: key %q: plugin %q: the token file is empty", key, g.ID)
		}
		if other, ok := seen[token]; ok {
			return fmt.Errorf("config: keys %q and %q: two plugins' token files hold the same token", other, key)
		}
		seen[token] = key
		if err := r.Tokens.Restore(token, r.As(g.Caller())); err != nil {
			return fmt.Errorf("plugin %q: %w", g.ID, err)
		}
		r.Log.Info("plugin: an external plugin may connect", "plugin", g.ID)
	}
	for _, g := range r.Grants {
		if g.External {
			continue
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.run(ctx, g)
		}()
	}
	return nil
}

// Wait returns once every plugin Start started has stopped.
func (r *Runner) Wait() { r.wg.Wait() }

// PluginOf returns the id of the running plugin whose process group pid
// is in, or "": the guard against a plugin that connects to the socket
// without its token, and so as the owner (wire.Server.NotOwner). It is no
// boundary: a process can leave its group.
func (r *Runner) PluginOf(pid int) string {
	r.mu.Lock()
	empty := len(r.groups) == 0
	r.mu.Unlock()
	if empty {
		return ""
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.groups[pgid]
}

func (r *Runner) group(pgid int, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id == "" {
		delete(r.groups, pgid)
		return
	}
	if r.groups == nil {
		r.groups = map[int]string{}
	}
	r.groups[pgid] = id
}

func (r *Runner) environ() []string {
	if r.Environ != nil {
		return r.Environ()
	}
	return os.Environ()
}

func or[T comparable](v, d T) T {
	var zero T
	if v == zero {
		return d
	}
	return v
}

// run starts g again and again until ctx ends or it stays stopped.
func (r *Runner) run(ctx context.Context, g Grant) {
	backoff := or(r.Backoff, DefaultBackoff)
	window := or(r.Window, DefaultWindow)
	var exits []time.Time
	for ctx.Err() == nil {
		began := time.Now()
		why, err := r.once(ctx, g)
		if ctx.Err() != nil {
			return
		}
		if why != "" { // nothing was started, and starting again would not change that
			r.stayStopped(ctx, g, why)
			return
		}
		now := time.Now()
		if now.Sub(began) >= window {
			exits, backoff = nil, or(r.Backoff, DefaultBackoff)
			r.closeOutage(ctx, g)
		}
		exits = append(exits, now)
		for len(exits) > 0 && now.Sub(exits[0]) > window {
			exits = exits[1:]
		}
		if len(exits) >= or(r.Failures, DefaultFailures) {
			r.stayStopped(ctx, g, fmt.Sprintf("it exited %d times within %v, the last time: %v", len(exits), window, err))
			return
		}
		r.Log.Warn("plugin exited; it starts again", "plugin", g.ID, "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, or(r.MaxBackoff, DefaultMaxBackoff))
	}
}

// once starts g and waits until it exits, or stops it when ctx ends. It
// returns why it started nothing, when the protocol or the executable is
// not the one the config allows; else how the run ended.
func (r *Runner) once(ctx context.Context, g Grant) (string, error) {
	if why := r.checkProtocol(g); why != "" {
		r.Log.Error("plugin not started", "plugin", g.ID, "reason", why)
		return why, nil
	}
	data, err := readExecutable(g.Path)
	if err != nil {
		r.Log.Error("plugin not started", "plugin", g.ID, "path", g.Path, "err", err)
		return fmt.Sprintf("its executable cannot be read: %v", err), nil
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != g.SHA256 {
		r.Log.Error("plugin not started: its executable is not the one the config names", "plugin", g.ID, "path", g.Path,
			"expected", g.SHA256, "actual", got)
		return fmt.Sprintf("the SHA-256 of its executable is %s, the config says %s", got, g.SHA256), nil
	}
	if r.afterRead != nil {
		r.afterRead(g)
	}
	exe, settings, state, err := r.prepare(g, data)
	if err != nil {
		r.Log.Error("plugin not started", "plugin", g.ID, "err", err)
		return "", err
	}
	defer os.Remove(exe)
	token, err := r.Tokens.Issue(r.As(g.Caller()))
	if err != nil {
		return "", err
	}
	// Revoked as soon as the plugin exits, below, and on every other way
	// out: its connections close, and the token is refused from then on.
	defer r.Tokens.Revoke(token)

	env := []string{EnvSocket + "=" + r.Socket, EnvToken + "=" + token, EnvID + "=" + g.ID,
		EnvState + "=" + state, EnvSettings + "=" + settings, EnvProtocol + "=" + r.Speaks}
	allowed := map[string]bool{}
	for _, k := range Env {
		allowed[k] = true
	}
	for _, kv := range r.environ() {
		if k, _, _ := strings.Cut(kv, "="); allowed[k] {
			env = append(env, kv)
		}
	}
	cmd := &exec.Cmd{Path: exe, Args: []string{g.Path}, Env: env, Dir: filepath.Dir(g.Path), SysProcAttr: procAttr()}
	outR, outW, err := os.Pipe()
	if err != nil {
		return "", err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return "", err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	err = startRetrying(cmd)
	outW.Close()
	errW.Close()
	var lines sync.WaitGroup
	lines.Add(2)
	go func() { defer lines.Done(); r.output(g, "stdout", outR, token) }()
	go func() { defer lines.Done(); r.output(g, "stderr", errR, token) }()
	defer func() {
		// The output ends when every process holding the pipes is gone; one
		// that left the plugin's group is not waited for long.
		done := make(chan struct{})
		go func() { lines.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			outR.Close()
			errR.Close()
			<-done
		}
		outR.Close()
		errR.Close()
	}()
	if err != nil {
		r.Log.Error("plugin not started", "plugin", g.ID, "err", err)
		return "", err
	}
	pgid := cmd.Process.Pid
	r.group(pgid, g.ID)
	r.Log.Info("plugin started", "plugin", g.ID, "pid", pgid)
	// A run that lasts the window ends the plugin's outage, while it runs.
	stable := time.AfterFunc(or(r.Window, DefaultWindow), func() { r.closeOutage(context.WithoutCancel(ctx), g) })
	defer stable.Stop()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err = <-exited:
	case <-ctx.Done():
		err = r.stop(g, pgid, exited)
	}
	r.Tokens.Revoke(token)
	// Whatever the plugin started and left in its group goes with it.
	syscall.Kill(-pgid, syscall.SIGKILL)
	r.group(pgid, "")
	return "", err
}

// stop stops the running plugin g, whose process group is pgid.
func (r *Runner) stop(g Grant, pgid int, exited <-chan error) error {
	grace := or(r.Grace, DefaultGrace)
	if r.Stopping != nil {
		// It has the grace period, and SIGTERM follows whether it returned.
		sctx, cancel := context.WithTimeout(context.Background(), grace)
		told := make(chan struct{})
		go func() { defer close(told); r.Stopping(sctx, g) }()
		select {
		case <-told:
		case <-sctx.Done():
		}
		cancel()
	}
	syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case err := <-exited:
		r.Log.Info("plugin stopped", "plugin", g.ID)
		return err
	case <-time.After(grace):
	}
	syscall.Kill(-pgid, syscall.SIGKILL)
	err := <-exited
	r.Log.Warn("plugin killed: it did not stop on SIGTERM", "plugin", g.ID, "grace", grace)
	return err
}

// startRetrying starts cmd, again a few times when the kernel says the
// file is busy: another fork of the daemon may still hold the copy open
// for writing for a moment after it was written.
func startRetrying(cmd *exec.Cmd) error {
	var err error
	for i := range 20 {
		if err = cmd.Start(); !errors.Is(err, syscall.ETXTBSY) {
			return err
		}
		time.Sleep(time.Duration(i+1) * 10 * time.Millisecond)
	}
	return err
}

// checkProtocol says why g's manifest asks for a protocol the daemon does
// not speak, or nothing.
func (r *Runner) checkProtocol(g Grant) string {
	speaks, ok := parseProtocol(r.Speaks)
	if !ok {
		return fmt.Sprintf("the daemon's protocol %q cannot be read", r.Speaks)
	}
	if g.Protocol.Major != speaks.Major || g.Protocol.Minor > speaks.Minor {
		return fmt.Sprintf("its manifest needs protocol %s, the daemon speaks %s", g.Protocol, speaks)
	}
	return ""
}

// readExecutable reads the regular file at path, at most MaxExecutable
// bytes, without blocking on a named pipe in its place.
func readExecutable(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxExecutable+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxExecutable {
		return nil, fmt.Errorf("larger than %d bytes", MaxExecutable)
	}
	return data, nil
}

// prepare writes what g's start needs: the executable's bytes and its
// settings, in the daemon's own directory for g, and g's state directory.
// It returns their paths.
func (r *Runner) prepare(g Grant, data []byte) (exe, settings, state string, err error) {
	run := filepath.Join(r.State, "plugin-run", g.ID)
	state = filepath.Join(r.State, "plugins", g.ID)
	for _, d := range []string{filepath.Dir(run), run, filepath.Dir(state), state} {
		if err := privateDir(d); err != nil {
			return "", "", "", err
		}
	}
	// A new name at each start, so that no file of an earlier start, or a
	// name known before, is the one started.
	old, _ := filepath.Glob(filepath.Join(run, "exec-*"))
	for _, f := range old {
		os.Remove(f)
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", "", err
	}
	exe = filepath.Join(run, "exec-"+hex.EncodeToString(b[:]))
	if err := writeNew(exe, data, 0o500); err != nil {
		return "", "", "", fmt.Errorf("copy of the executable: %w", err)
	}
	settings = filepath.Join(run, "settings.json")
	if err := writeNew(settings, g.Settings, 0o400); err != nil {
		return "", "", "", fmt.Errorf("settings: %w", err)
	}
	return exe, settings, state, nil
}

// privateDir makes dir, mode 0700, or makes an existing one that mode.
func privateDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return os.Chmod(dir, 0o700)
}

// writeNew writes data to a new file next to path, then renames it to
// path: path is never a file that was there before, nor one being written.
func writeNew(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".new"
	os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// output logs what the plugin writes to one stream, a line at a time, each
// marked with the plugin's id and cut at LineMax bytes: a longer line is
// logged as soon as LineMax bytes of it came, and the rest of it is
// dropped. The plugin's token is never logged, even when it prints it.
func (r *Runner) output(g Grant, stream string, in io.Reader, token string) {
	max := or(r.LineMax, DefaultLineMax)
	br := bufio.NewReaderSize(in, max)
	log := func(line []byte, cut bool) {
		text := string(bytes.TrimRight(line, "\r\n"))
		if token != "" {
			text = strings.ReplaceAll(text, token, "[token]")
			if cut { // a token the cut went through: its first part
				for k := len(token) - 1; k > 0; k-- {
					if strings.HasSuffix(text, token[:k]) {
						text = text[:len(text)-k] + "[token]"
						break
					}
				}
			}
		}
		text = strings.ToValidUTF8(text, "\uFFFD")
		if text != "" || cut {
			r.Log.Info("plugin output", "plugin", g.ID, "stream", stream, "line", text, "cut", cut)
		}
	}
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			log(line, true)
			for errors.Is(err, bufio.ErrBufferFull) { // the rest of the line is dropped
				_, err = br.ReadSlice('\n')
			}
		} else {
			log(line, false)
		}
		if err != nil {
			return
		}
	}
}

// stayStopped leaves g stopped: one log line, the outage kept, and the
// owner told once for it, tried again until that succeeds or ctx ends.
func (r *Runner) stayStopped(ctx context.Context, g Grant, why string) {
	r.Log.Error("plugin stays stopped", "plugin", g.ID, "reason", why)
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	var opened int64
	err := r.Outages.Update(dctx, func(tx *store.Tx) error {
		if _, err := tx.ExecContext(dctx, `INSERT INTO plugin_outages (plugin, opened_at) VALUES (?, ?) ON CONFLICT DO NOTHING`,
			g.ID, time.Now().UnixMilli()); err != nil {
			return err
		}
		return tx.QueryRowContext(dctx, `SELECT opened_at FROM plugin_outages WHERE plugin = ?`, g.ID).Scan(&opened)
	})
	if err != nil {
		r.Log.Error("plugin: recording its outage", "plugin", g.ID, "err", err)
		return
	}
	if r.Tell == nil {
		return
	}
	key := "plugin-stopped:" + g.ID + ":" + strconv.FormatInt(opened, 10)
	text := fmt.Sprintf("The plugin %s is stopped and is not started again until hataori restarts: %s.", g.ID, why)
	for wait := or(r.Backoff, DefaultBackoff); ; wait = min(2*wait, or(r.MaxBackoff, DefaultMaxBackoff)) {
		tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := r.Tell(tctx, key, text)
		cancel()
		if err == nil {
			return
		}
		r.Log.Warn("plugin: the owner could not be told that it stays stopped; trying again", "plugin", g.ID, "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// closeOutage ends g's outage: it runs again.
func (r *Runner) closeOutage(ctx context.Context, g Grant) {
	if _, err := r.Outages.DB.ExecContext(ctx, `DELETE FROM plugin_outages WHERE plugin = ?`, g.ID); err != nil {
		r.Log.Error("plugin: ending its outage", "plugin", g.ID, "err", err)
	}
}
