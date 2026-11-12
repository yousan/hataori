// Command sqlite-bench compares SQLite drivers for hataori.
//
// It opens a WAL database with a busy timeout in a temporary directory,
// inserts N rows with one transaction per row, then reads them all back.
// Exactly one driver is compiled in, chosen by a build tag:
//
//	go run -tags modernc .
//	go run -tags ncruces .
//	CGO_ENABLED=1 go run -tags mattn .
//	go run -tags zombiezen .
//
// Every driver gets the same PRAGMA synchronous (-sync, default NORMAL),
// because the drivers' own defaults differ.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// store is what each driver file provides.
type store interface {
	// pragma reads a setting back from the open connection, as text.
	pragma(name string) (string, error)
	createTable() error
	insert(i int, body string) error // one row, in its own transaction
	readAll() (int, error)           // returns the number of rows read
	close() error
}

// open is set by exactly one driver file.
var (
	driverName string
	open       func(path string, busyMS int, sync string) (store, error)
)

func main() {
	n := flag.Int("n", 10000, "rows to insert")
	busy := flag.Int("busy", 5000, "busy timeout in milliseconds")
	sync := flag.String("sync", "NORMAL", "PRAGMA synchronous: NORMAL or FULL")
	flag.Parse()
	if open == nil {
		fmt.Fprintln(os.Stderr, "no driver compiled in: build with -tags modernc|ncruces|mattn|zombiezen")
		os.Exit(2)
	}
	if err := run(*n, *busy, *sync); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(n, busy int, sync string) error {
	dir, err := os.MkdirTemp("", "sqlite-bench-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	s, err := open(filepath.Join(dir, "bench.db"), busy, sync)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer s.close()

	// Check that the settings asked for in the DSN are the ones in force.
	want := map[string]string{
		"journal_mode": "wal",
		"busy_timeout": fmt.Sprint(busy),
		"synchronous":  map[string]string{"NORMAL": "1", "FULL": "2"}[sync],
	}
	got := map[string]string{}
	for _, name := range []string{"journal_mode", "busy_timeout", "synchronous"} {
		v, err := s.pragma(name)
		if err != nil {
			return fmt.Errorf("PRAGMA %s: %w", name, err)
		}
		if v != want[name] {
			return fmt.Errorf("PRAGMA %s is %q, want %q", name, v, want[name])
		}
		got[name] = v
	}
	if err := s.createTable(); err != nil {
		return fmt.Errorf("create: %w", err)
	}

	start := time.Now()
	for i := 0; i < n; i++ {
		if err := s.insert(i, fmt.Sprintf("event body %d", i)); err != nil {
			return fmt.Errorf("insert %d: %w", i, err)
		}
	}
	insertDur := time.Since(start)

	start = time.Now()
	rows, err := s.readAll()
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	readDur := time.Since(start)
	if rows != n {
		return fmt.Errorf("read %d rows, want %d", rows, n)
	}

	fmt.Printf("driver=%s go=%s os/arch=%s/%s\n", driverName, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Printf("journal_mode=%s busy_timeout=%s synchronous=%s (%s)\n", got["journal_mode"], got["busy_timeout"], got["synchronous"], sync)
	fmt.Printf("insert %d rows (1 tx each): %v (%.1f µs/row)\n", n, insertDur.Round(time.Millisecond), float64(insertDur.Microseconds())/float64(n))
	fmt.Printf("read back %d rows: %v\n", rows, readDur.Round(time.Microsecond))
	return nil
}
