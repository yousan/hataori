package chat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/surface"
)

// noticePart is the first part of the lines about files not posted, after
// the messages that carry the files.
const noticePart = 50

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
		st, err := os.Stat(real)
		switch {
		case err != nil || !st.Mode().IsRegular():
			notices = append(notices, "File "+name+" was not posted: it cannot be read.")
		case lim.MaxFileBytes > 0 && st.Size() > lim.MaxFileBytes:
			notices = append(notices, fmt.Sprintf("File %s was not posted: %s is over the limit of %s.", name, size(st.Size()), size(lim.MaxFileBytes)))
		default:
			files = append(files, surface.File{Name: name, Path: real})
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
		rows = append(rows, Row{Event: e.Pos, Part: i / per, Conv: conv, Message: m})
	}
	if len(files) == 0 && strings.TrimSpace(caption) != "" && len(notices) > 0 {
		rows = append(rows, Row{Event: e.Pos, Part: 0, Conv: conv, Message: surface.Message{Text: caption}})
	}
	for j, n := range notices {
		rows = append(rows, Row{Event: e.Pos, Part: noticePart + j, Conv: conv, Message: surface.Message{Text: n, Quiet: true}})
	}
	return rows
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
