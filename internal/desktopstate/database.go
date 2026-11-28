package desktopstate

import (
	"errors"
	"os"

	"github.com/yousan/hataori/internal/nativehttp"
)

// CheckMainDatabase checks every pathname SQLite may open before setup or a
// schema probe opens it. No orphan recovery file licenses creating a new DB.
// Checked 0700 ancestors still protect legacy 0644 SQLite files. Neither a
// symlink/hardlink nor an ambiguous spelling can redirect recovery elsewhere.
func CheckMainDatabase(state *os.Root) (bool, error) {
	const name = "hataori.db"
	if err := RefuseCollision(state.Name(), name); err != nil {
		return false, err
	}
	f, err := nativehttp.OpenPrivateDatabase(state, name)
	exists := err == nil
	if exists {
		f.Close()
	} else if !os.IsNotExist(err) {
		return false, err
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		entry := name + suffix
		if err := RefuseCollision(state.Name(), entry); err != nil {
			return false, err
		}
		f, err := nativehttp.OpenPrivateDatabase(state, entry)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		f.Close()
		if !exists {
			return false, errors.New("desktop: orphan database recovery file was preserved")
		}
	}
	return exists, nil
}
