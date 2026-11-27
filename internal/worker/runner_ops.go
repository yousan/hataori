package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/workerproto"
)

// describe reports only this receiver-owned run. Unknown host state never
// becomes gone by a timeout, and a worker PID is never returned as authority.
func (r *Resident) describe(ctx context.Context, t workerproto.Target) (workerproto.RunMetadata, *workerproto.Refusal) {
	run := r.state.Runs[targetKey(t)]
	if run == nil || r.checkMarker(run) != nil {
		return workerproto.RunMetadata{}, failure("wrong_target", "owned run cannot be established")
	}
	if run.Agent.Agent == "" {
		return workerproto.RunMetadata{}, failure("not_ready", "run has no agent launch metadata")
	}
	s := run.Spec
	b := workerproto.RunMetadata{AgentConfig: run.Agent, Dir: s.Dir, StateDir: s.StateDir, ConfigDir: s.ConfigDir, Model: s.Model, Effort: s.Effort, Resume: s.ResumeID, Instructions: s.Instructions, NoProjectSettings: s.NoProjectSettings, Terminal: run.Terminal, State: run.State, Turn: run.Turn, Waiting: []workerproto.WaitingHook{}}
	if b.State == "prepared" {
		b.State = "reserved"
	}
	if b.State == "starting" || b.State == "preparing" {
		b.State = "unknown"
	}
	if b.State == "running" {
		c, err := r.owned(run)
		if err != nil {
			if r.ended(run) {
				b.State = "ended"
			} else {
				b.State = "unknown"
			}
		} else {
			defer c.Close()
			calls, err := c.Waiting()
			if err != nil {
				return b, failure("unavailable", "waiting hooks cannot be checked")
			}
			for _, w := range calls {
				b.Waiting = append(b.Waiting, workerproto.WaitingHook{ID: w.ID, AtMS: w.At.UnixMilli(), Payload: w.Payload})
			}
			if run.Terminal {
				n, at, err := c.Attached(ctx)
				if err != nil {
					return b, failure("unavailable", "attachment state cannot be checked")
				}
				b.Attached = n
				if !at.IsZero() {
					b.DetachedMS = at.UnixMilli()
				}
			}
		}
	}
	if st, ok, err := r.ownedStatus(run); err != nil {
		return b, failure("unavailable", "host status cannot be read")
	} else if ok {
		b.Status = &workerproto.RunStatus{Exited: st.Exited, Code: st.Code, Signal: st.Signal, Stopped: st.Stopped, StopNote: st.StopNote, Error: st.Error, Shutdown: st.Shutdown}
	}
	return b, nil
}

// Retained data is never discarded on a committed main offset. The bounded
// run budget pauses reads explicitly instead of truncating rebuild history.
const maxRunHistory = 64 << 20

type sourceRecord struct {
	Target workerproto.Target `json:"target"`
	Source workerproto.Source `json:"source"`
	Path   string             `json:"path"`
	Device uint64             `json:"device"`
	Inode  uint64             `json:"inode"`
	Size   int64              `json:"size"`
}

func (r *Resident) historyPath(s *sourceRecord) string {
	return filepath.Join(r.runDir(s.Target), "history-"+s.Source.Handle)
}

// sourcePaths is read from the session's real host log. The main supplies an
// opaque digest only; it cannot turn a string into an arbitrary worker path.
func (r *Resident) sourcePaths(t workerproto.Target) (map[string]string, error) {
	run := r.state.Runs[targetKey(t)]
	out := map[string]string{"events": filepath.Join(r.hostDir(t), host.EventsFile)}
	if run.Terminal {
		out["terminal"] = filepath.Join(r.hostDir(t), host.TerminalLog)
	}
	discovery, ok := r.adapters[run.Agent].(agent.FollowSources)
	if !ok {
		return out, nil
	}
	f, err := openSource(out["events"])
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxRunHistory+1))
	if err != nil || len(b) > maxRunHistory {
		return nil, errors.New("worker: source discovery unavailable")
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte(`{"hook":`)) {
			continue
		}
		for _, path := range discovery.FollowSources(agent.Hook, line) {
			if !filepath.IsAbs(path) || len(path) > 4096 {
				continue
			}
			out[workerproto.FileSource(path)] = path
			if len(out) > 128 {
				return nil, errors.New("worker: source limit")
			}
		}
	}
	return out, nil
}

func (r *Resident) followSource(t workerproto.Target, name string) (workerproto.Source, *workerproto.Refusal) {
	if run := r.state.Runs[targetKey(t)]; run == nil || r.checkMarker(run) != nil {
		return workerproto.Source{}, failure("wrong_target", "owned run cannot be established")
	}
	if r.state.Sources == nil {
		r.state.Sources = map[string]*sourceRecord{}
	}
	key := targetKey(t) + "-" + name
	if old := r.state.Sources[key]; old != nil {
		return old.Source, nil
	}
	paths, err := r.sourcePaths(t)
	if err != nil {
		return workerproto.Source{}, failure("unavailable", "adapter sources cannot be read")
	}
	path, ok := paths[name]
	if !ok {
		return workerproto.Source{}, failure("denied", "source was not reported by this session's adapter")
	}
	f, err := openSource(path)
	if err != nil {
		return workerproto.Source{}, failure("missing", "reported source cannot be opened")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return workerproto.Source{}, failure("unreadable", "reported source is not a regular file")
	}
	id, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return workerproto.Source{}, failure("unavailable", "source identity is unknown")
	}
	s := &sourceRecord{Target: t, Source: workerproto.Source{Source: name, Handle: newID(), Generation: newID()}, Path: path, Device: uint64(id.Dev), Inode: uint64(id.Ino)}
	h, err := r.root.OpenFile(r.historyPath(s), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return workerproto.Source{}, failure("unavailable", "retained history cannot be created")
	}
	err = h.Sync()
	cerr := h.Close()
	if err != nil || cerr != nil || syncDir(r.root, r.runDir(t)) != nil {
		return workerproto.Source{}, failure("unavailable", "retained history cannot be committed")
	}
	r.state.Sources[key] = s
	if err := r.save(); err != nil {
		r.ready = false
		return workerproto.Source{}, failure("unavailable", "source registration cannot be committed")
	}
	return s.Source, nil
}

func (r *Resident) readSource(t workerproto.Target, b workerproto.Read) (workerproto.Bytes, *workerproto.Refusal) {
	out := workerproto.Bytes{Source: b.Source, Order: b.Order, Offset: b.Offset, Next: b.Offset, State: "unavailable"}
	if run := r.state.Runs[targetKey(t)]; run == nil || r.checkMarker(run) != nil {
		return out, failure("wrong_target", "owned run cannot be established")
	}
	s := r.state.Sources[targetKey(t)+"-"+b.Source.Source]
	if s == nil || s.Target != t || s.Source != b.Source {
		return out, failure("wrong_target", "source is not registered for this run")
	}
	bad := func(state string) (workerproto.Bytes, *workerproto.Refusal) { out.State = state; return out, nil }
	history, err := r.root.ReadFile(r.historyPath(s))
	if err != nil || int64(len(history)) < s.Size {
		return bad("history_lost")
	}
	history = history[:s.Size]
	// Already retained ranges remain readable even after source replacement.
	end := b.Offset + uint64(b.Limit)
	if end > uint64(s.Size) {
		f, err := openSource(s.Path)
		if err != nil {
			if os.IsNotExist(err) {
				return bad("missing")
			}
			return bad("unreadable")
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			return bad("unreadable")
		}
		id, ok := st.Sys().(*syscall.Stat_t)
		if !ok || uint64(id.Dev) != s.Device || uint64(id.Ino) != s.Inode {
			return bad("replaced")
		}
		if st.Size() < s.Size || uint64(st.Size()) < b.Offset {
			return bad("truncated")
		}
		end = min(end, uint64(st.Size()))
		var retained int64
		for _, v := range r.state.Sources {
			if v.Target == t {
				retained += v.Size
			}
		}
		if end > maxRunHistory || retained+int64(end)-s.Size > maxRunHistory {
			return out, failure("limit", "retained run history budget is exhausted")
		}
		data := make([]byte, end)
		if _, err := io.ReadFull(f, data); err != nil {
			return bad("truncated")
		}
		if !bytes.Equal(data[:s.Size], history) {
			return bad("replaced")
		}
		if int64(end) > s.Size {
			h, err := r.root.OpenFile(r.historyPath(s), os.O_RDWR, 0600)
			if err != nil {
				return bad("history_lost")
			}
			_, err = h.WriteAt(data[s.Size:], s.Size)
			if err == nil {
				err = h.Sync()
			}
			cerr := h.Close()
			if err != nil || cerr != nil {
				return bad("unavailable")
			}
			s.Size = int64(end)
			if r.save() != nil {
				r.ready = false
				return bad("unavailable")
			}
			history = data
		}
	}
	if b.Offset > uint64(len(history)) {
		return bad("truncated")
	}
	end = min(end, uint64(len(history)))
	out.Data = bytes.Clone(history[b.Offset:end])
	out.Next = end
	out.State = "empty"
	if len(out.Data) > 0 {
		out.State = "data"
	}
	return out, nil
}

// Nonblocking open prevents a reported FIFO/device from holding the resident
// mutex before its type can be checked. Only regular files become sources.
func openSource(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("worker: source is not a regular file")
	}
	return f, nil
}
