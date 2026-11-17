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
	for _, p := range paths {
		name := filepath.Base(p)
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
		copied, n, err := f.copyFile(real, filepath.Join(fmt.Sprint(e.Pos), fmt.Sprint(len(files)+len(notices))), lim.MaxFileBytes)
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
// the copy's path. It opens real without following a link at its end and
// checks that it is still the file that was resolved, and it copies at
// most max bytes (0: no limit); a longer file gives errTooBig and its size.
func (f *FrontEnd) copyFile(real, name string, max int64) (string, int64, error) {
	if f.FileDir == "" {
		return "", 0, errors.New("no FileDir to copy into")
	}
	src, err := os.OpenFile(real, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
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
	if again, err := filepath.EvalSymlinks(real); err != nil || again != real {
		return "", 0, errors.New("the path changed while it was checked")
	}
	if max > 0 && st.Size() > max {
		return "", st.Size(), errTooBig
	}
	dst := filepath.Join(f.FileDir, name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", 0, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", 0, err
	}
	limit := int64(math.MaxInt64)
	if max > 0 {
		limit = max + 1
	}
	n, err := io.Copy(out, io.LimitReader(src, limit))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	if max > 0 && n > max {
		return "", n, errTooBig // it grew after the check
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
		if rel, err := filepath.Rel(rr, real); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "." {
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
