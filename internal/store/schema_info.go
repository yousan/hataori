package store

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
)

// SchemaVersion is the schema this binary can migrate to and read. Desktop
// replacement must never offer a binary older than the persisted schema.
func SchemaVersion() int { return len(migrations) }

// ReadSchemaVersion observes an existing database without creating/migrating it.
// The caller checks the selected root and database filesystem ownership first.
func ReadSchemaVersion(ctx context.Context, root string) (int, error) {
	u := url.URL{Scheme: "file", Path: filepath.Join(root, File), RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var version int
	err = db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&version)
	return version, err
}
