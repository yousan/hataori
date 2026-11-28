# OpenSSH failure captures

`openssh-9.6p1/*.stderr` are real `OpenSSH_9.6p1` client stderr captured by
`TestSSHFailuresStayUnavailable` against its own loopback sshd, generated keys,
explicit client/server configuration and fake-agent-free resident. No operator
SSH destination, key, authentication agent or configuration was used.

The capture hook runs after the owned client has exited and stderr is drained.
Only the generated fixture directory, local fixture account name, ephemeral
loopback port and temporary transport directory are redacted. Line endings are
preserved. Capture with `go test ./internal/workerssh -run
TestSSHFailuresStayUnavailable -capture-ssh testdata/<new-version>` from the
repository root. Existing files refuse overwrite. Add changed output alongside
these captures, never replace an old capture to make a parser test pass.

All matching rules in errors.go are exercised against these files. Runtime
status/errors expose fixed cause categories only, never captured/raw stderr.
The real-SSH acceptance tests run independently on Linux and macOS; a missing
OpenSSH client/server or key generator is a test failure, never a skipped pass.
