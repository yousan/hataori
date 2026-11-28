package desktopstate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"

	"github.com/yousan/hataori/internal/nativehttp"
	"github.com/yousan/hataori/internal/store"
)

const setupDBMagic = "hataori desktop setup database v1\n"

func setupDBMarker(state *os.Root, name string, create bool) (bool, error) {
	if err := RefuseCollision(state.Name(), name); err != nil {
		return false, err
	}
	if create {
		f, err := nativehttp.OpenPrivateFile(state, name, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		if err == nil {
			_, err = io.WriteString(f, setupDBMagic)
			if err == nil {
				err = f.Sync()
			}
			f.Close()
			return err == nil, err
		}
		if !errors.Is(err, os.ErrExist) {
			return false, err
		}
	}
	f, err := nativehttp.OpenPrivateFile(state, name, os.O_RDONLY)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 128))
	if err != nil || string(data) != setupDBMagic {
		return false, errors.New("desktop setup: unknown database provenance was preserved")
	}
	return true, nil
}

// InitializeMainIdentity runs only while setup holds the daemon's permanent
// root lock. Existing installations are read without migration or identity
// replacement. A private reservation permits retrying only an interrupted
// setup-owned SQLite initialization, never an established/truncated database.
func InitializeMainIdentity(ctx context.Context, root string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	r, err := nativehttp.OpenPrivateRoot(root)
	if err != nil {
		return "", err
	}
	defer r.Close()
	state, err := nativehttp.OpenPrivateDir(r, "state", false)
	if err != nil {
		return "", err
	}
	defer state.Close()
	exists, err := CheckMainDatabase(state)
	if err != nil {
		return "", err
	}
	reserved, err := setupDBMarker(state, "desktop.setup-database", false)
	if err != nil {
		return "", err
	}
	complete, err := setupDBMarker(state, "desktop.setup-database-complete", false)
	if err != nil {
		return "", err
	}
	if exists && (!reserved || complete) {
		return store.ReadMainIdentity(ctx, root)
	}
	if !exists && complete {
		return "", errors.New("desktop setup: established database is missing; it was not recreated")
	}
	if !exists {
		if _, err := setupDBMarker(state, "desktop.setup-database", true); err != nil {
			return "", err
		}
		f, err := nativehttp.OpenPrivateFile(state, "hataori.db", os.O_RDWR|os.O_CREATE|os.O_EXCL)
		if err != nil {
			return "", err
		}
		f.Close()
	} else {
		f, err := nativehttp.OpenPrivateDatabase(state, "hataori.db")
		if err != nil {
			return "", err
		}
		var header [16]byte
		n, readErr := f.ReadAt(header[:], 0)
		f.Close()
		if !(n == 0 && errors.Is(readErr, io.EOF) || n == 16 && bytes.Equal(header[:], []byte("SQLite format 3\x00"))) {
			return "", errors.New("desktop setup: unknown interrupted database was preserved")
		}
	}
	db, err := store.Open(ctx, root)
	if err != nil {
		return "", err
	}
	id, err := db.MainIdentity(ctx)
	closeErr := db.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if _, err := setupDBMarker(state, "desktop.setup-database-complete", true); err != nil {
		return "", err
	}
	return id, nil
}
