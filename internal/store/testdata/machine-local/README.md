# Previous-version local migration fixture

`previous.db.gz` was created by `generate.go` with the unmodified store at
commit `357cd3db161ae600c785a4c49b8ee2650aa7ad43` (schema 53), before the
machine migration existed. All records are synthetic; no running instance,
agent, service, credentials or configuration was read.

To regenerate, check out that commit in a disposable worktree, copy
`generate.go` into its `internal/store/testdata/machine-local/`, and run:

```sh
go run internal/store/testdata/machine-local/generate.go previous.db.gz
```

The migration test decompresses and copies this database into an isolated root,
adds sentinel workspace/session files and a nested repository, then opens it
with the new store. It compares every old column of every old table, all
recorded path/spec bytes, and the files and directory identities before/after.
The source fixture is never opened as a writable database.
