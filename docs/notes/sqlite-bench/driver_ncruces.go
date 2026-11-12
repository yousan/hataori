//go:build ncruces

package main

import (
	"fmt"

	_ "github.com/ncruces/go-sqlite3/driver"
)

func init() {
	driverName = "github.com/ncruces/go-sqlite3"
	open = func(path string, busyMS int, sync string) (store, error) {
		dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=synchronous(%s)", path, busyMS, sync)
		return openSQL("sqlite3", dsn)
	}
}
