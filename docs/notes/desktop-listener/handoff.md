# Native listener component and pending integration

Issue #1071, draft PR #1115. This is an unintegrated transport component,
not usable desktop support. No Electron code, shipped configuration field,
control operation, event encoding or existing golden file changes here.

## Implemented boundary

`internal/nativehttp.Listen` accepts daemon-supplied root, installation identity
and daemon version. The embedding daemon must already hold `state/lock` and
must retain it until the listener and its HTTP server have stopped. This
package never opens, deletes or unlocks that file and never starts a daemon.
A separate permanent native lease prevents another native listener from
replacing live discovery. New sockets have random generation names; startup
does not remove existing socket names or arbitrary files.

Root, state and runtime directories require ownership by the effective OS user
and mode 0700. Socket, lease and discovery require mode 0600 and a single link.
Explicit Lstat checks and descriptor identity comparisons reject in-root
symlinks too: Go's os.Root may otherwise resolve them despite O_NOFOLLOW.
Ancestors must be owned by this user or root and have no group/other write,
except root-owned sticky temporary parents. Linux and Darwin use kernel peer
credentials on both ends. Unknown credentials and unknown Darwin xucred
versions refuse before HTTP sees a connection.

Discovery is an atomic private state file binding canonical root path, root
device/inode, installation, version, generation, expected socket path and
socket device/inode. A client checks the exact selected binding and peer UID;
it never accepts an arbitrary path from the file. Long roots use a short
per-user runtime directory and a root/installation hash below it. Copying a
record to another root or replacing the original directory does not redirect
it to an old runtime socket. A busy or incompatible instance refuses without
modifying the original discovery, socket or instance lock.

Darwin additionally uses libc filesystem-security/ACL inspection and refuses
ownership-disabled volumes, unknown results and every allow ACE, including
owner-only or inheritance-only grants. Deny ACEs are permitted. This is
deliberately conservative and its compatibility needs executed Darwin tests.
Without cgo, Darwin refuses native access with ErrACLUnavailable. Existing
Darwin artifact builds use CGO_ENABLED=0: those artifacts therefore do not
provide this native transport. A build-owner decision/handoff or a supported
non-cgo verifier is still required; green cgo runtime tests would not establish
the existing artifact's usability.

Close checks socket identity before unlinking. The OS-user boundary excludes
concurrent namespace mutation by another process of the same UID; the embedding
daemon must serialize it. Replacement present before the identity check is
preserved. Crash leftovers are not reclaimed by this component.

## Shared hunks awaiting explicit release

No listed hunk is changed by this component. Owner decisions and a fresh main /
PR-diff check must precede integration; no donor branch may be imported.

| File/function | Minimum requested integration |
| --- | --- |
| `cmd/hataori/daemon.go`, `daemon`, dashboard construction around `var dash` / `cfg.Dashboard` | Permit a native-only dashboard independently of browser listeners. Supply a daemon-bound owner dashboard API with `Via: dashboard`, the locked root, installation identity from the open store and the bundled version. Reuse effective host-input/attachment policy; keep browser sign-in callbacks separate. |
| `cmd/hataori/daemon.go`, `daemon`, dashboard serving / deferred cleanup | Serve native HTTP only after startup validation, track its lifetime with the daemon, close HTTP streams/upgrades before releasing the root lock, and report native discovery without advertising an owner TCP endpoint. |
| `internal/dashboard/server.go`, listener construction / handler / session lookup | Add an explicit native listener identity and owner-only request context. Headers, URLs, proxy forwarding, bearer sessions and TCP requests must never select it. Enforce fixed Host/Origin, route/method/upgrade allowlists and isolation. |
| `internal/dashboard/operate.go` and `attach.go`, first WebSocket message and expiry | Bind native upgrades to their already verified connection, including first-message authentication. Keep existing browser bearer and recent-auth rules intact. |
| `web/src/session.ts`, transport/bootstrap and rebuilt `internal/dashboard/page/index.html` | Specify and exercise the native bootstrap without issuing a bearer usable on another listener. Rebuild the existing dashboard only after the shared page release. |
| Existing Darwin build/release configuration | Decide how distributed binaries receive a verified ACL implementation. No workflow or artifact setting is changed here. |

The last checked daemon owners were PRs #1049, #1048, #1043, #1013, #1012,
#1010, #1005, #1002 and #1109; page owners included #1013, #1054 and #1108.
Their branches, tests and review gates remain intact. These are ownership
reservations, not new functional prerequisites on all their features.

## Evidence and remaining acceptance

`validation.json` records source hashes, focused serial/race results and
behavioral guard-removal failures. Reproduce the overlay checks with
`python3 docs/notes/desktop-listener/check_guards.py`. Compilation failures
are not counted as behavioral failures. Repository sources are never mutated.
The foreign-owner metadata variation is not a cross-user execution.

The cross-user fixture requires an already authorized privileged isolated
runner. It does not call sudo or alter accounts. It skips on this unprivileged
runner, so cross-user acceptance remains unmet. Darwin libc/ACL/mount-flag
fixtures have not executed here. Cross-builds and skipped tests are not platform
acceptance. No local tmux, real Mac, production/configuration, service or host
operation is part of this work.

Still required for #1071: released startup/authority/page/build integration;
existing dashboard page, reads, streams, writes, operate/attach and native
bootstrap evidence; hostile HTTP and cross-listener/browser sign-in regressions;
complete discovery/lock lifecycle evidence; executed supported-platform and
cross-user security acceptance; final fresh independent implementation and
authority reviews; all preserved mandatory review gates; full required Linux
and macOS CI on final head and exact merge commit on main; merge and issue
closure. No Claude invocation or gate waiver is authorized.

#1072 starts on a separate issue/branch/PR after #1071 merges; #1073 follows
#1072. Real-Mac acceptance, Windows/WSL acceptance, signing/spending and public
distribution gates remain with their later steps. No unsigned installer is
published by this component.
