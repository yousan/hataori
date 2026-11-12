# SQLite driver for a single static binary

hataori keeps its state in one SQLite file ([design, section 6.2](../design/overview.md#62-sqlite)) and should ship as one binary for Linux and macOS.
This note compares the Go SQLite drivers in common use on the one question that decides the build: does it need cgo.

Checked on 2026-11-12 with go1.27.4. The benchmark is in [`sqlite-bench/`](sqlite-bench/), a separate Go module so that its dependencies do not enter hataori's `go.mod`.

## Drivers

| Driver | Version checked | How it gets SQLite | API |
|---|---|---|---|
| [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) | v1.62.0 | SQLite's C source translated to Go (ccgo) | `database/sql`, driver name `sqlite` |
| [github.com/ncruces/go-sqlite3](https://pkg.go.dev/github.com/ncruces/go-sqlite3) | v0.37.0 | A Wasm build of SQLite translated to Go with wasm2go ([README](https://github.com/ncruces/go-sqlite3/blob/v0.37.0/README.md)) | `database/sql` (`/driver`, name `sqlite3`) and a direct API |
| [github.com/mattn/go-sqlite3](https://pkg.go.dev/github.com/mattn/go-sqlite3) | v1.14.53 | SQLite's C source compiled by cgo | `database/sql`, driver name `sqlite3` |
| [zombiezen.com/go/sqlite](https://pkg.go.dev/zombiezen.com/go/sqlite) | v1.4.2 | Wraps modernc.org/sqlite's low-level package (see its [go.mod](https://github.com/zombiezen/go-sqlite/blob/v1.4.2/go.mod)) | Its own API, not `database/sql` |

Since v0.37.0 the ncruces driver no longer needs wazero or the `embed` package; importing `github.com/ncruces/go-sqlite3/embed` only prints a deprecation notice ([embed.go](https://github.com/ncruces/go-sqlite3/blob/v0.37.0/embed/embed.go)).

## Answers per driver

| | modernc.org/sqlite | ncruces/go-sqlite3 | mattn/go-sqlite3 | zombiezen.com/go/sqlite |
|---|---|---|---|---|
| `CGO_ENABLED=0` build, linux/amd64 | works | works | builds, **but is a stub** | works |
| `CGO_ENABLED=0` build, linux/arm64 | works | works | builds, **but is a stub** | works |
| `CGO_ENABLED=0` build, darwin/arm64 | works | works | builds, **but is a stub** | works |
| WAL mode | yes | yes | yes | yes |
| Busy timeout | yes | yes | yes | yes |
| Last release (module proxy) | v1.62.0, 2026-11-07 | v0.37.0, 2026-11-07 | v1.14.53, 2026-11-05 | v1.4.2, 2025-05-23 |
| Licence | [BSD-3-Clause](https://gitlab.com/cznic/sqlite/-/blob/v1.62.0/LICENSE) | [MIT](https://github.com/ncruces/go-sqlite3/blob/v0.37.0/LICENSE) | [MIT](https://github.com/mattn/go-sqlite3/blob/v1.14.53/LICENSE) | [ISC](https://github.com/zombiezen/go-sqlite/blob/v1.4.2/LICENSE) |

SQLite itself is in the public domain ([sqlite.org/copyright.html](https://sqlite.org/copyright.html)) in every case.

### Cross builds

Run from `docs/notes/sqlite-bench`, once per driver tag `$t` in `modernc ncruces mattn zombiezen` and per target:

```sh
CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -tags $t -o bench-$t-linux-amd64  .
CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -tags $t -o bench-$t-linux-arm64  .
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -tags $t -o bench-$t-darwin-arm64 .
```

All twelve builds exited 0. Binary sizes:

| | linux/amd64 | linux/arm64 | darwin/arm64 |
|---|---|---|---|
| modernc | 10.5 MB | 10.1 MB | 10.1 MB |
| ncruces | 14.4 MB | 14.1 MB | 13.9 MB |
| mattn (stub) | 3.5 MB | 3.3 MB | 3.3 MB |
| zombiezen | 9.8 MB | 9.6 MB | 9.5 MB |

`file bench-modernc-linux-amd64` (and the same for ncruces and zombiezen) reports `ELF 64-bit LSB executable, x86-64, ... statically linked`.
The linux/amd64 binaries were run on the build host; the arm64 and darwin binaries were built but not run.

**mattn/go-sqlite3 without cgo is a trap.** The build succeeds because the package has a `//go:build !cgo` file ([static_mock.go](https://github.com/mattn/go-sqlite3/blob/v1.14.53/static_mock.go)), and the binary fails on the first open:

```text
$ ./bench-mattn-linux-amd64 -n 10
error: open: Binary was compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to work. This is a stub
```

Its README says so: "you are required to set the environment variable `CGO_ENABLED=1` and have a `gcc` compiler present" ([README](https://github.com/mattn/go-sqlite3/blob/v1.14.53/README.md#installation)).
With cgo on, a cross build needs a C cross toolchain for each target. On a linux/amd64 host with only the native gcc:

```text
$ CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build -tags mattn -o /dev/null .
# runtime/cgo
gcc_arm64.S: Assembler messages:
gcc_arm64.S:30: Error: no such instruction: `stp x29,x30,[sp,'
$ CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -tags mattn -o /dev/null .
# runtime/cgo
cgo: C compiler "clang" not found: exec: "clang": executable file not found in $PATH
```

So each release target would need its own build machine or cross toolchain (and a macOS SDK for darwin).

### WAL and busy timeout

The settings each driver was given ([`driver_*.go`](sqlite-bench/)):

| Driver | How |
|---|---|
| modernc | DSN `file:<path>?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)` ([docs](https://pkg.go.dev/modernc.org/sqlite#Driver.Open)) |
| ncruces | DSN `file:<path>?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)` ([docs](https://pkg.go.dev/github.com/ncruces/go-sqlite3/driver)) |
| mattn | DSN `file:<path>?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL` ([docs](https://pkg.go.dev/github.com/mattn/go-sqlite3#SQLiteDriver.Open)) |
| zombiezen | `sqlite.OpenConn(path, OpenReadWrite\|OpenCreate\|OpenWAL)`, then `conn.SetBusyTimeout(5s)` and `PRAGMA synchronous=NORMAL` ([docs](https://pkg.go.dev/zombiezen.com/go/sqlite#OpenConn)) |

The bench reads `PRAGMA journal_mode`, `PRAGMA busy_timeout` and `PRAGMA synchronous` back after opening and stops if any differs from what was asked. Every driver printed:

```text
journal_mode=wal busy_timeout=5000 synchronous=1 (NORMAL)
```

`synchronous` is set explicitly because the defaults differ: mattn/go-sqlite3 sets `NORMAL` when the journal mode is WAL ([sqlite3.go](https://github.com/mattn/go-sqlite3/blob/v1.14.53/sqlite3.go), `synchronousMode`), the others leave SQLite's default `FULL`. `NORMAL` is the setting SQLite documents as safe from corruption in WAL mode ([pragma synchronous](https://sqlite.org/pragma.html#pragma_synchronous)).

### Release dates

From the Go module proxy:

```sh
curl -s https://proxy.golang.org/modernc.org/sqlite/@latest
curl -s https://proxy.golang.org/github.com/ncruces/go-sqlite3/@latest
curl -s https://proxy.golang.org/github.com/mattn/go-sqlite3/@latest
curl -s https://proxy.golang.org/zombiezen.com/go/sqlite/@latest
```

```text
{"Version":"v1.62.0","Time":"2026-11-07T09:13:04Z",...}
{"Version":"v0.37.0","Time":"2026-11-07T01:19:24Z",...}
{"Version":"v1.14.53","Time":"2026-11-05T15:04:08Z",...}
{"Version":"v1.4.2","Time":"2025-05-23T22:11:01Z",...}
```

zombiezen v1.4.2 requires modernc.org/sqlite v1.37.1; in the bench module, minimal version selection lifts it to v1.62.0 (`go version -m bench-zombiezen-linux-amd64` shows `modernc.org/sqlite v1.62.0`). That combination worked in the bench but is not one zombiezen released.

## Benchmark

10,000 single-row inserts, each in its own transaction, into a fresh WAL database in a temporary directory, then one `SELECT` reading all rows back. One connection, busy timeout 5000 ms. Source: [`sqlite-bench/main.go`](sqlite-bench/main.go).

Run from `docs/notes/sqlite-bench`:

```sh
CGO_ENABLED=0 go run -tags modernc .
CGO_ENABLED=0 go run -tags ncruces .
CGO_ENABLED=1 go run -tags mattn .
CGO_ENABLED=0 go run -tags zombiezen .
```

Flags: `-n` rows (default 10000), `-busy` ms (default 5000), `-sync NORMAL|FULL` (default `NORMAL`).

Machine: go1.27.4, linux/amd64 (Linux 6.6 under WSL2, ext4 on a virtual disk), an 8-core / 16-thread desktop x86-64 CPU (AMD Zen 3).
The four drivers were run in turn, three rounds.

### `synchronous=NORMAL` (default)

| Driver | Insert, 10,000 tx (runs 1 / 2 / 3) | Per row (median) | Read back 10,000 rows (runs 1 / 2 / 3) |
|---|---|---|---|
| modernc | 760 ms / 599 ms / 1065 ms | 76 µs | 4.6 / 3.9 / 8.8 ms |
| ncruces | 629 ms / 658 ms / 628 ms | 63 µs | 3.0 / 4.1 / 3.1 ms |
| mattn (cgo) | 699 ms / 768 ms / 710 ms | 71 µs | 10.1 / 4.9 / 5.1 ms |
| zombiezen | 560 ms / 596 ms / 597 ms | 60 µs | 1.5 / 1.6 / 2.6 ms |

All four are within about 25 % of each other on inserts, and the spread between runs of one driver is as large as the spread between drivers. Reading back is a few milliseconds for all of them. zombiezen is slightly faster than modernc on the same SQLite code; its bench path reuses prepared statements (`sqlitex.Execute`) and does not go through `database/sql`, and this benchmark does not separate the two causes.

### `synchronous=FULL`, one round

Command: the same, with `-sync FULL` (the bench then checks `synchronous=2`).

| Driver | Insert, 10,000 tx | Per row | Read back |
|---|---|---|---|
| modernc | 9.5 s | 948 µs | 4.1 ms |
| ncruces | 10.9 s | 1090 µs | 2.9 ms |
| mattn (cgo) | 22.8 s | 2283 µs | 5.1 ms |
| zombiezen | 8.0 s | 800 µs | 1.4 ms |

With `FULL` every commit waits for an fsync, so the number measures the disk, not the driver; on this virtual disk it also varied several-fold between runs (an earlier round of modernc alone took 17 s, 44 s and 18 s). Do not read driver differences into it.

## Recommendation

Use **modernc.org/sqlite** through `database/sql`.
It builds with `CGO_ENABLED=0` for linux/amd64, linux/arm64 and darwin/arm64, supports WAL and a busy timeout through the DSN, performs like the cgo driver for hataori's write pattern, and is a stable v1 module that zombiezen also builds on, whereas ncruces is still v0 and mattn would need a C toolchain per release target and silently produces a broken binary without one.
