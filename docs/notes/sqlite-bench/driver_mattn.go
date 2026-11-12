//go:build mattn

package main

import (
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

func init() {
	driverName = "github.com/mattn/go-sqlite3"
	open = func(path string, busyMS int, sync string) (store, error) {
		dsn := fmt.Sprintf("file:%s?_busy_timeout=%d&_journal_mode=WAL&_synchronous=%s", path, busyMS, sync)
		return openSQL("sqlite3", dsn)
	}
}
