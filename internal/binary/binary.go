// Package binary finds the names hataori's own binary runs under.
package binary

import (
	"os"
	"path/filepath"
	"slices"
)

// Names returns the command names that hataori's binary at exe runs under: a
// process is named after what it was started as, which is the binary's file
// or any link to it. The names are those on the way from exe, and from each
// of others that leads to the same file, to that file, and every other name
// of the file in a directory on the way: a version's binary and the link to
// it are next to each other (design, section 6.1).
func Names(exe string, others ...string) []string {
	names := []string{filepath.Base(exe)}
	file, err := os.Stat(exe)
	if err != nil {
		return names
	}
	add := func(name string) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	seen := map[string]bool{} // directories
	for _, p := range append([]string{exe}, others...) {
		if !sameFile(p, file) {
			continue
		}
		for range maxLinks {
			add(filepath.Base(p))
			if dir := filepath.Dir(p); !seen[dir] {
				seen[dir] = true
				for _, name := range namesIn(dir, file) {
					add(name)
				}
			}
			to, err := os.Readlink(p)
			if err != nil {
				break // the file itself
			}
			if !filepath.IsAbs(to) {
				// Not joined to the directory as written: ".." is the parent
				// of what a linked directory leads to.
				dir, err := filepath.EvalSymlinks(filepath.Dir(p))
				if err != nil {
					break
				}
				to = filepath.Join(dir, to)
			}
			p = to
		}
	}
	return names
}

// maxLinks bounds the links followed from one path to the file.
const maxLinks = 40

// sameFile reports whether path is file or a link to it.
func sameFile(path string, file os.FileInfo) bool {
	fi, err := os.Stat(path)
	return err == nil && os.SameFile(fi, file)
}

// namesIn are the names of file in dir.
func namesIn(dir string, file os.FileInfo) []string {
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if !e.IsDir() && sameFile(filepath.Join(dir, e.Name()), file) {
			names = append(names, e.Name())
		}
	}
	return names
}
