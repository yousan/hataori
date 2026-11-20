package host

import (
	"io"
	"os"
	"sync"
)

// boundedLog is an append-only file kept under limit bytes: when a write
// would pass the limit, the oldest part is dropped so that about half the
// limit remains.
//
// It counts the bytes written to it since it was opened: the end of the file
// is always at position pos, so a byte keeps its position when the file is
// trimmed. Followers get every byte written after they started following.
type boundedLog struct {
	mu        sync.Mutex
	path      string
	limit     int64
	f         *os.File
	size      int64
	pos       uint64 // bytes written since the log was opened
	followers map[*follower]struct{}
	closed    bool
	// trimmed, when set, is told the position of the file's first byte
	// after each trim; lock, when set, is held from before the trim until
	// trimmed has returned.
	trimmed func(start uint64)
	lock    func() (unlock func())
}

func openBounded(path string, limit int64) (*boundedLog, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &boundedLog{path: path, limit: limit, f: f, size: st.Size()}, nil
}

func (l *boundedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, all := len(p), p
	keep := min(l.limit/2, l.limit-int64(len(p)))
	if int64(len(p)) > l.limit {
		// Only its end fits. Nothing old stays in front of it, or the file
		// would skip the bytes dropped between.
		p, keep = p[int64(len(p))-l.limit/2:], 0
	}
	if l.size+int64(len(p)) > l.limit {
		// Keep at most half the limit of the old content, and as much as
		// fits beside p.
		if err := l.trim(keep); err != nil {
			return 0, err
		}
	}
	m, err := l.f.Write(p)
	l.size += int64(m)
	written := n - (len(p) - m) // what the file now ends with, dropped part included
	l.pos += uint64(written)
	l.deliver(all[:written])
	if err != nil {
		return written, err
	}
	return n, nil
}

// trim keeps the last keep bytes of the file.
func (l *boundedLog) trim(keep int64) error {
	if keep < 0 {
		keep = 0
	}
	if l.lock != nil {
		defer l.lock()()
	}
	r, err := os.Open(l.path)
	if err != nil {
		return err
	}
	defer r.Close()
	if _, err := r.Seek(l.size-keep, io.SeekStart); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	w, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	l.f.Close()
	l.f, l.size = f, keep
	if l.trimmed != nil {
		l.trimmed(l.pos - uint64(keep))
	}
	return nil
}

// since returns what was written to the log after position after, as far
// as the file still keeps it, and the position at its end. The file always
// ends with this run's bytes, at position pos, so a trim does not change
// where a position is. cut is set when some of the bytes after after were
// dropped by a trim; data then starts at the oldest one kept. A position
// past the end, as from another run, gets nothing, and the end.
func (l *boundedLog) since(after uint64) (data []byte, pos uint64, cut bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	pos = l.pos
	if after >= pos {
		return nil, pos, false, nil
	}
	n := pos - after
	// The last min(size, pos) bytes of the file are this run's; any before
	// them were left by an earlier run.
	kept := min(uint64(l.size), pos)
	if n > kept {
		n, cut = kept, true
	}
	f, err := os.Open(l.path)
	if err != nil {
		return nil, after, false, err
	}
	defer f.Close()
	data = make([]byte, n)
	if _, err := f.ReadAt(data, l.size-int64(n)); err != nil {
		return nil, after, false, err
	}
	return data, pos, cut, nil
}

func (l *boundedLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	for f := range l.followers {
		f.end()
	}
	l.followers = nil
	return l.f.Close()
}

// lineLog appends whole lines to a file, one writer at a time, so lines
// from different sources never interleave.
type lineLog struct {
	mu sync.Mutex
	f  *os.File
}

func openLines(path string) (*lineLog, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &lineLog{f: f}, nil
}

// append writes the parts and a newline as one line.
func (l *lineLog) append(parts ...[]byte) error {
	var buf []byte
	for _, p := range parts {
		buf = append(buf, p...)
	}
	buf = append(buf, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.f.Write(buf)
	return err
}

func (l *lineLog) Close() error { return l.f.Close() }
