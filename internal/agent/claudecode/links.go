package claudecode

import (
	"os"
	"path/filepath"
)

// resolveLinks is p with the links in the part of it that exists resolved;
// false when they cannot be, as for a link that leads nowhere, which a
// write would follow.
func resolveLinks(p string) (string, bool) {
	rest := ""
	for q := p; ; {
		if _, err := os.Lstat(q); err == nil {
			r, err := filepath.EvalSymlinks(q)
			if err != nil {
				return "", false
			}
			return filepath.Join(r, rest), true
		}
		parent := filepath.Dir(q)
		if parent == q {
			return p, true
		}
		rest = filepath.Join(filepath.Base(q), rest)
		q = parent
	}
}
