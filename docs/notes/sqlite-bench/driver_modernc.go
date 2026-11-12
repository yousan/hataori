//go:build modernc

package main

import (
	"fmt"

	_ "modernc.org/sqlite"
)

func init() {
	driverName = "modernc.org/sqlite"
	open = func(path string, busyMS int, sync string) (store, error) {
		dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=synchronous(%s)", path, busyMS, sync)
		return openSQL("sqlite", dsn)
	}
}
