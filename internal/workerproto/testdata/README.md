# Worker protocol examples

These files are generated from the worker protocol's own typed encoder, not
captures of an agent or chat service. Login examples contain synthetic material
which has never been a usable link, code or credential. No deployment data is
used. The frozen worker versions are independent of `internal/wire/testdata`.

For a new version, change `Version`, add its examples, and run:

```
go test ./internal/workerproto -run TestGolden -update
```

This refuses to update any version already present in `protocol/SUMS`. Keep
previous directories and checksums. Run the package tests without `-update` to
check both the current encoder and all frozen directories. The aggregate hash
uses sorted filenames, each followed by NUL, decimal byte count, NUL, and the
exact file bytes. Each JSON frame includes its final newline.
