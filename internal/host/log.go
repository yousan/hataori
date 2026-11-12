package host

import (
	"io"
	"os"
	"sync"
)

// boundedLog is an append-only file kept under limit bytes: when a write
// would pass the limit, the oldest part is dropped so that about half the
// limit remains.
type boundedLog struct {
	mu    sync.Mutex
	path  string
	limit int64
	f     *os.File
	size  int64
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
	n := len(p)
	if int64(len(p)) > l.limit { // only its end fits
		p = p[int64(len(p))-l.limit/2:]
	}
	if l.size+int64(len(p)) > l.limit {
		// Keep at most half the limit of the old content, and as much as
		// fits beside p.
		if err := l.trim(min(l.limit/2, l.limit-int64(len(p)))); err != nil {
			return 0, err
		}
	}
	m, err := l.f.Write(p)
	l.size += int64(m)
	if err != nil {
		return m, err
	}
	return n, nil
}

// trim keeps the last keep bytes of the file.
func (l *boundedLog) trim(keep int64) error {
	if keep < 0 {
		keep = 0
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
	return nil
}

func (l *boundedLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
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
