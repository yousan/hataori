package store

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
)

func (s *Store) MainIdentity(ctx context.Context) (string, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT identity FROM machine_main_identity WHERE id=1`).Scan(&id)
	return id, err
}

// ReadMainIdentity reads only an existing database. The owner can obtain the
// registration ID while the daemon runs; this neither creates nor migrates it.
func ReadMainIdentity(ctx context.Context, root string) (string, error) {
	path, err := filepath.Abs(filepath.Join(root, File))
	if err != nil {
		return "", err
	}
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{}
	q.Set("mode", "ro")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return "", err
	}
	defer db.Close()
	var id string
	err = db.QueryRowContext(ctx, `SELECT identity FROM machine_main_identity WHERE id=1`).Scan(&id)
	return id, err
}
