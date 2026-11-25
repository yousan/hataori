package main

import (
	"os/exec"
	"path/filepath"

	"github.com/yousan/hataori/internal/binary"
)

// ownNames are the names by which hataori's own processes are told: those of
// the binary at exe, the name this process was started under, and the
// command's own, which a link made after this process started may have. The
// error says that names may be missing.
func ownNames(exe, arg0 string) ([]string, error) {
	as, _ := exec.LookPath(arg0)
	names, err := binary.Names(exe, as)
	if arg0 != "" {
		names = append(names, filepath.Base(arg0))
	}
	return append(names, "hataori"), err
}
