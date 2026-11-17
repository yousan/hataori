package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

// fileRows returns the messages that hand over the files of a file_out
// event: the files, as many per message as the service takes, the caption
// with the first; then one line for each file that is not posted.
func (f *FrontEnd) fileRows(ctx context.Context, e control.Event, conv surface.Conversation, session int64, paths []string, caption string) []Row {
	var roots []string
	var rootErr error
	if f.FileRoots != nil {
		roots, rootErr = f.FileRoots(ctx, e.Thread, session)
	} else {
		rootErr = fmt.Errorf("no file roots")
	}
	lim := f.Driver.Limits()
	var files []surface.File
	var notices []string
	for i, p := range paths {
		name := filepath.Base(p)
		// A copy made when the event was read before is what is posted.
		copyName := filepath.Join(fmt.Sprint(e.Pos), fmt.Sprint(i))
		if dst := filepath.Join(f.FileDir, copyName); f.FileDir != "" {
			if _, err := os.Stat(dst); err == nil {
				files = append(files, surface.File{Name: name, Path: dst})
				continue
			}
		}
		if rootErr != nil {
			notices = append(notices, "File "+name+" was not posted: its place cannot be checked.")
			continue
		}
		real, ok := within(p, roots)
		if !ok {
			notices = append(notices, "File "+name+" was not posted: it is outside the workspace and the session's directory.")
			continue
		}
		// What is posted is a copy taken now, so the file cannot be
		// changed or replaced by a link between the check and the post.
		copied, n, err := f.copyFile(real, roots, copyName, lim.MaxFileBytes)
		switch {
		case errors.Is(err, errTooBig):
			notices = append(notices, fmt.Sprintf("File %s was not posted: %s is over the limit of %s.", name, size(n), size(lim.MaxFileBytes)))
		case err != nil:
			f.Log.Warn("chat: a file the agent handed over cannot be read", "file", p, "err", err)
			notices = append(notices, "File "+name+" was not posted: it cannot be read.")
		default:
			files = append(files, surface.File{Name: name, Path: copied})
		}
	}
	per := lim.MaxFiles
	if per <= 0 {
		per = 1
	}
	var rows []Row
	for i := 0; i < len(files); i += per {
		m := surface.Message{Files: files[i:min(i+per, len(files))]}
		if i == 0 {
			m.Text = caption
		}
		rows = append(rows, Row{Event: e.Pos, Part: len(rows), Conv: conv, Message: m})
	}
	if len(files) == 0 && strings.TrimSpace(caption) != "" && len(notices) > 0 {
		rows = append(rows, Row{Event: e.Pos, Part: len(rows), Conv: conv, Message: surface.Message{Text: caption}})
	}
	for _, n := range notices {
		rows = append(rows, Row{Event: e.Pos, Part: len(rows), Conv: conv, Message: surface.Message{Text: n, Quiet: true}})
	}
	return rows
}

var errTooBig = errors.New("over the size limit")

// copyFile copies the regular file real into FileDir under name and returns
// the copy's path. It opens real without blocking and without following a
// link at its end, then checks that the file it opened is real, inside
// roots; it copies at most max bytes (0: no limit), and a longer file gives
// errTooBig and its size. Where the system has no /proc to name the file
// opened, the file is refused.
func (f *FrontEnd) copyFile(real string, roots []string, name string, max int64) (string, int64, error) {
	if f.FileDir == "" {
		return "", 0, errors.New("no FileDir to copy into")
	}
	dst := filepath.Join(f.FileDir, name)
	src, err := os.OpenFile(real, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", 0, err
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return "", 0, err
	}
	if !st.Mode().IsRegular() {
		return "", 0, errors.New("not a regular file")
	}
	// What was opened, as the kernel names it, so that a directory on the
	// way replaced by a link meanwhile is seen.
	// Without /proc this cannot be checked, and the file is refused.
	opened, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", src.Fd()))
	if err != nil || opened != real {
		return "", 0, errors.New("the path changed while it was checked")
	}
	if _, ok := within(opened, roots); !ok {
		return "", 0, errors.New("the file opened is outside the roots")
	}
	if max > 0 && st.Size() > max {
		return "", st.Size(), errTooBig
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".copy-*")
	if err != nil {
		return "", 0, err
	}
	// A copy that fails is left in FileDir under its temporary name.
	limit := int64(math.MaxInt64)
	if max > 0 {
		limit = max + 1
	}
	n, err := io.Copy(tmp, io.LimitReader(src, limit))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	if max > 0 && n > max {
		return "", n, errTooBig // it grew after the check
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", 0, err
	}
	return dst, n, nil
}

// within returns p with its symbolic links resolved, and whether that is
// inside one of roots (resolved too).
func within(p string, roots []string) (string, bool) {
	if !filepath.IsAbs(p) {
		return "", false
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", false
	}
	for _, r := range roots {
		rr, err := filepath.EvalSymlinks(r)
		if err != nil {
			continue
		}
		if rel, err := filepath.Rel(rr, real); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return real, true
		}
	}
	return "", false
}

func size(n int64) string {
	const mib = 1 << 20
	if n >= mib {
		return fmt.Sprintf("%.1f MiB", float64(n)/mib)
	}
	return fmt.Sprintf("%d KiB", (n+1023)/1024)
}
