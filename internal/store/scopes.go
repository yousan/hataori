package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// StoredScope is the identity of a channel scope. Configuration may supply
// its current policy; this identity and its key are never removed.
type StoredScope struct {
	Key     string
	Client  string
	Channel string
	Created time.Time
}

func (s *Store) StoredScopes(ctx context.Context) ([]StoredScope, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT key,client,channel,created_at FROM stored_scopes ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredScope
	for rows.Next() {
		var sc StoredScope
		var ms int64
		if err := rows.Scan(&sc.Key, &sc.Client, &sc.Channel, &ms); err != nil {
			return nil, err
		}
		sc.Created = time.UnixMilli(ms).UTC()
		out = append(out, sc)
	}
	return out, rows.Err()
}

// ReserveScopeKeys remembers every configured key, even one with no
// threads. All stored-client conflicts are checked before anything changes.
func (s *Store) ReserveScopeKeys(ctx context.Context, configured map[string]string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, client := range configured {
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT client FROM stored_scopes WHERE key=?`, key).Scan(&owner)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && owner != client {
			return fmt.Errorf("scope %q is stored for client %q but configured for client %q", key, owner, client)
		}
	}
	for key := range configured {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO scope_keys(key) VALUES (?)`, key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// KeepScope reserves the channel's exact id as key and records its scope.
// available checks the directory without changing it, inside the claim's
// transaction. Unknown availability is an error. A taken key returns no
// scope; an existing stored (client,channel) returns its original identity.
// It activates nothing: the daemon's ingress policy is separate (#893).
func (s *Store) KeepScope(ctx context.Context, client, channel string, available func() (bool, error)) (StoredScope, bool, error) {
	if client == "" || channel == "" || available == nil {
		return StoredScope{}, false, errors.New("store: a scope needs a client, channel and directory check")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return StoredScope{}, false, err
	}
	defer tx.Rollback()
	var sc StoredScope
	var ms int64
	err = tx.QueryRowContext(ctx, `SELECT key,client,channel,created_at FROM stored_scopes WHERE client=? AND channel=?`, client, channel).Scan(&sc.Key, &sc.Client, &sc.Channel, &ms)
	if err == nil {
		sc.Created = time.UnixMilli(ms).UTC()
		return sc, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return sc, false, err
	}
	var taken int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM scope_keys WHERE key=? UNION ALL SELECT 1 FROM threads WHERE scope=? UNION ALL SELECT 1 FROM supervisor_dirs WHERE scope=?)`, channel, channel, channel).Scan(&taken); err != nil {
		return sc, false, err
	}
	if taken != 0 {
		return sc, false, nil
	}
	free, err := available()
	if err != nil {
		return sc, false, err
	}
	// Remember a directory-only collision even if that directory later goes.
	if _, err := tx.ExecContext(ctx, `INSERT INTO scope_keys(key) VALUES (?)`, channel); err != nil {
		return sc, false, err
	}
	if !free {
		return sc, false, tx.Commit()
	}
	sc = StoredScope{Key: channel, Client: client, Channel: channel, Created: time.Now().UTC().Truncate(time.Millisecond)}
	if _, err := tx.ExecContext(ctx, `INSERT INTO stored_scopes(key,client,channel,created_at) VALUES (?,?,?,?)`, sc.Key, sc.Client, sc.Channel, sc.Created.UnixMilli()); err != nil {
		return StoredScope{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return StoredScope{}, false, err
	}
	return sc, true, nil
}
