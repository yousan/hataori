package workspace

import "golang.org/x/sys/unix"

// renameNoReplace renames from to to, failing when to exists in any form,
// also as an empty directory.
func renameNoReplace(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_EXCL)
}
