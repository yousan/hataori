package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/machineio"
	"github.com/yousan/hataori/internal/store"
)

func (r *Runner) openIO(lv *live) (result error) {
	defer func() {
		if result != nil {
			lv.io = nil
		}
	}()
	if lv.io != nil {
		return nil
	}
	lv.io = &machineio.Local{Dir: lv.dir, Host: r.own()}
	lv.sources = map[string]machineio.Source{}
	if r.Store == nil {
		return nil
	} // isolated codec/read fixtures
	var machine string
	if err := r.Store.DB.QueryRow(`SELECT t.machine FROM threads t JOIN sessions s ON s.thread_id=t.id WHERE s.id=?`, lv.id).Scan(&machine); err != nil {
		return err
	}
	if machine != store.LocalMachine {
		var err error
		if r.IO != nil {
			lv.io, err = r.IO(context.Background(), lv.id)
		} else {
			lv.io, err = r.remoteSession(context.Background(), lv.id)
		}
		if err != nil {
			return err
		}
		// The codec's worker paths remain metadata. Local-only holder helpers
		// receive the main's logical state directory, never those paths.
		lv.dir = StateDir(r.Root, lv.id)
		lv.remote = true
	}
	rows, err := r.Store.DB.Query(`SELECT path,handle,generation FROM session_io_sources WHERE session_id=?`, lv.id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var s machineio.Source
		if err := rows.Scan(&s.Name, &s.Handle, &s.Generation); err != nil {
			return err
		}
		lv.sources[s.Name] = s
	}
	return rows.Err()
}

func digestRead(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (r *Runner) sourceFor(lv *live, path string) (machineio.Source, error) {
	if err := r.openIO(lv); err != nil {
		return machineio.Source{}, err
	}
	if s, ok := lv.sources[path]; ok {
		return s, nil
	}
	s, err := lv.io.Source(context.Background(), path)
	if err != nil {
		return s, err
	}
	lv.sources[path] = s
	return s, nil
}

func (r *Runner) readBytes(lv *live, path string, from, to int64) ([]byte, machineio.Source, error) {
	s, err := r.sourceFor(lv, path)
	if err != nil {
		return nil, s, err
	}
	if from < 0 || to < from {
		return nil, s, errors.New("runner: invalid retained range")
	}
	var out []byte
	for off := from; off < to; {
		n := int(min(int64(machineio.MaxRead), to-off))
		b, err := lv.io.Read(context.Background(), s, off, n)
		if err != nil {
			return nil, s, err
		}
		if len(b.Data) == 0 {
			return nil, s, &machineio.ReadError{State: "history_lost"}
		}
		out = append(out, b.Data...)
		off += int64(len(b.Data))
	}
	return out, s, nil
}

func (r *Runner) readLines(lv *live, path string, off int64) ([][]byte, int64, readRange, error) {
	s, err := r.sourceFor(lv, path)
	if err != nil {
		return nil, off, readRange{}, err
	}
	var data []byte
	// A protocol chunk is bounded, while an agent line can span chunks.
	// Stop at one complete batch, or pause an oversized unfinished line.
	for len(data) < 1<<20 {
		b, err := lv.io.Read(context.Background(), s, off+int64(len(data)), machineio.MaxRead)
		if err != nil {
			return nil, off, readRange{}, err
		}
		data = append(data, b.Data...)
		end := complete(data, 0)
		if end > 0 || len(b.Data) == 0 {
			proof := readRange{path: path, start: off, end: off + end, source: s, digest: digestRead(data[:end])}
			return lines(data, 0, end), off + end, proof, nil
		}
	}
	return nil, off, readRange{}, errors.New("runner: unfinished source line exceeds the read limit")
}

func (r *Runner) pauseIO(lv *live, err error) {
	if err == nil && lv.readErr == nil {
		return
	}
	if err != nil && lv.readErr != nil && err.Error() == lv.readErr.Error() {
		return
	}
	lv.readErr = err
	reason := ""
	if err != nil {
		reason = err.Error()
		r.Log.Warn("session I/O paused", "session", lv.id, "reason", reason)
	}
	if r.Store != nil {
		if _, e := r.Store.DB.Exec(`UPDATE sessions SET io_error=? WHERE id=?`, reason, lv.id); e != nil {
			r.Log.Error("recording session I/O pause", "session", lv.id, "err", e)
		}
	}
}

// warmIO preflights all retained ranges before decoding any. Replay has no
// effects; its hook replies may later reconcile the original waiting calls.
func (r *Runner) warmIO(lv *live) ([]agent.HookReply, error) {
	starts, reads, err := r.storedReads(lv)
	if err != nil {
		return nil, err
	}
	first := map[string]int64{}
	for _, rd := range reads {
		if _, ok := first[rd.path]; !ok {
			first[rd.path] = rd.start
		}
	}
	var plan []readRange
	add := func(p string, start, end int64) {
		if f, ok := first[p]; ok && f < end {
			end = f
		}
		if end > start {
			plan = append(plan, readRange{path: p, start: start, end: end})
		}
	}
	add("", lv.start, lv.offset)
	paths := make([]string, 0, len(lv.files))
	for p := range lv.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		add(p, starts[p], lv.files[p])
	}
	plan = append(plan, reads...)
	data := make([][]byte, len(plan))
	for i, rd := range plan {
		b, s, err := r.readBytes(lv, rd.path, rd.start, rd.end)
		if err != nil {
			return nil, err
		}
		if rd.digest != "" && (s != rd.source || digestRead(b) != rd.digest) {
			return nil, errors.New("runner: retained read identity or content changed")
		}
		if rd.path != "@screen" && len(b) > 0 && b[len(b)-1] != '\n' {
			return nil, errors.New("runner: retained line was truncated")
		}
		data[i] = b
	}
	var replies []agent.HookReply
	for _, rd := range plan {
		if rd.path == "@screen" {
			lv.codec.Open()
			break
		}
	}
	for i, rd := range plan {
		if rd.path == "@screen" {
			replies = append(replies, lv.codec.Decode(agent.Screen, data[i]).Reply...)
			continue
		}
		for _, line := range bytes.SplitAfter(data[i], []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			src := agent.File
			if rd.path == "" {
				src = source(line)
			}
			a := lv.codec.Decode(src, bytes.TrimSuffix(line, []byte("\n")))
			replies = append(replies, a.Reply...)
		}
	}
	return replies, nil
}

func saveProof(ctx context.Context, tx *store.Tx, id, n int64, rd readRange) error {
	if rd.source.Generation == "" {
		return nil
	} // immutable historical rows
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_io_sources(session_id,path,handle,generation) VALUES(?,?,?,?)
		ON CONFLICT(session_id,path) DO UPDATE SET handle=excluded.handle,generation=excluded.generation`, id, rd.path, rd.source.Handle, rd.source.Generation); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO session_read_proofs(session_id,n,digest,handle,generation) VALUES(?,?,?,?,?)`, id, n, rd.digest, rd.source.Handle, rd.source.Generation)
	return err
}

func (r *Runner) replayReplies(lv *live) {
	waiting, err := lv.io.Waiting(context.Background())
	if err != nil {
		return
	}
	open := map[string]bool{}
	for _, w := range waiting {
		open[w.ID] = true
	}
	var again agent.Actions
	for _, rep := range lv.replayReplies {
		if open[rep.Call] {
			again.Reply = append(again.Reply, rep)
		}
	}
	if err := r.act(lv, again); err == nil {
		lv.replayReplies = nil
	}
}
