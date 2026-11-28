# Native listener integration and pending acceptance

Issue #1071, draft PR #1115. The transport, daemon and HTTP integration are
implemented for review. This is not complete desktop support. No Electron app,
installer, shipped configuration field, control operation, event encoding or
existing golden file changes are included.

## Startup and authority

`hataori run --native --root <root>` explicitly enables a separate native HTTP
server after startup validation. Ordinary browser-only daemon startup keeps its
existing configuration. The instance already holds `state/lock`; installation
identity comes from the open store and the daemon supplies its version. Native
startup failure ends startup rather than opening an owner TCP endpoint.

The native API uses a separate owner dashboard Caller, with `Via: dashboard`
and dashboard scope/action permissions. It never reuses the control-socket
Caller, permits sign-in-link creation, or grants unlisted-scope starts. The
pending `StartUnlisted` field is absent at this main baseline; its zero value
must stay false when the owning change lands. Host input retains the effective
configured policy. Browser sign-in callbacks and listeners remain separate.

The daemon stops native admission, cancels and joins active handlers,
WebSockets, their request workers and immediate stream producer channels, and
joins its HTTP serving goroutine before the store/root lock close. Binding is
validated before background workers start; HTTP acceptance starts later with
dashboard serving. Native stream cleanup drains the canceled producer channel.
The existing browser server still uses its previous Close lifecycle; this
change does not certify a join of all browser work.

The nested Terminal lifecycle finding is now repaired in the specifically
authorized `core.(*api).Terminal` hunks: cancel/drain the current Runner child
before closing the API channel, and cancel/drain the old child before opening
a replacement. Cancellation during the join prevents a replacement start.
Replacement open failure retains the closed old channel for safe deferred
cleanup; replacement still starts with empty run/zero position, with no replay.
Held-child fixtures cover these exit, success, open-error and cancellation
paths. No Runner production code was edited.

This is a limited supervisor handoff, not a release of other owners' hunks.
#1003 reports no ownership/reservation/unpublished producer delta. Immediately
before editing, #1118 head `5b8dcf01bf61198940a4ac32eb685f6ddb9aacb5` changed
only Core's adapter-capability field and selected-agent research preflight;
its published reservation does not include Terminal/Runner producer work.
Those hunks are disjoint and preserved. Other owner tests, branches and review
gates are retained without imports or overwrites.

## Private transport

`internal/nativehttp.Listen` accepts daemon-supplied root, installation identity
and version. It never opens, deletes or unlocks `state/lock` and never starts a
daemon. A separate permanent native lease prevents another native listener from
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
socket device/inode. A client checks the selected binding and peer UID; it
never accepts an arbitrary pathname. Long roots use a short per-user runtime
directory and a root/installation hash. A copied record or replaced root cannot
redirect to the old runtime socket. A busy or incompatible instance refuses
without modifying discovery, socket or instance lock.

Darwin additionally uses libc ACL inspection and refuses ownership-disabled
volumes, unknown results and every allow ACE, including owner-only or
inheritance-only grants. Deny ACEs are permitted. Without cgo, Darwin refuses
native access with ErrACLUnavailable. Existing Darwin artifacts use
CGO_ENABLED=0 and therefore cannot provide this transport. The supervisor
explicitly accepted keeping that fail-closed behavior: no build-setting change
or additional maintainer decision is requested. Executed Darwin ACL/security
acceptance remains missing; a cross-build is not evidence of usable native
access.

Close checks socket identity before unlinking. The OS-user boundary excludes
concurrent namespace mutation by another process of the same UID; the embedding
daemon must serialize it. A replacement present before the identity check is
preserved. Crash leftovers are not reclaimed.

## Native HTTP/bootstrap contract

Only a kernel-checked Unix connection selects native authority. No header,
request identity, bearer or TCP request can select it. The fixed Host is
`instance`; the fixed Origin is `hataori-desktop://instance`. Reads may omit
Origin; writes and WebSocket upgrades require exactly that Origin. Empty,
duplicate or foreign Origin fields, Authorization and proxy fields, absolute
proxy targets, noncanonical/encoded paths and routes/methods outside the
native allowlist are refused. Arbitrary subsequently registered server routes,
sign-in/session/passkey routes and control operations are excluded.

The explicitly specified setup read is `GET /api/native`, returning
`{"instance":"<installation>","version":"<daemon version>","native":true}`.
It is private HTTP metadata, not a control-socket message. It returns 404 on
signed-in TCP listeners. `GET /api/me` identifies the bound owner and way
`native`. Neither response issues a bearer. Existing read/write/stream payloads
and control protocol versions/golden examples remain unchanged.

Native HTTP requests carry no Authorization header. The first native operate
message is `{"session":"","pane":"%1"}`; the first attachment message is
`{"session":"","thread":1}`. Their authority is already bound to the
connection; any nonempty session is refused. Browser WebSockets continue to
require their signed bearer and their existing web recent-auth/passkey checks.
No native bearer exists to copy to another listener.

The existing page and its entry asset are delivered over native HTTP in tests.
Fresh-origin bootstrap currently has no stored browser bearer and already
emits the empty WebSocket session. However, `web/src/session.ts` still adopts
stored credentials: the proposed minimum native bootstrap branch must ignore
browser link/storage adoption on the fixed native origin. This source hunk and
its necessary generated entry asset/index reference remain held pending
explicit decisions from #1054/#1108. #1013 explicitly released only the native
link/storage bypass, any needed entry initialization, and corresponding JS
bundle/index script reference; its later source-preserving rebuild need not
keep the same bundle hash. Its control StartUnlisted:true, native permissions
and unlisted source/generated behavior stay protected. That release does not
release #1054's AllThreads/Details/api/catalog entry rebuild or #1108's worker
UI entry/CSS rebuild; neither owner's source/build hunk has been changed.
Fresh narrow requests are recorded on each of those PRs. No page source,
index, generated assets, stylesheet or workflow was edited. Real Electron
custom-scheme fetch/SSE/duplex integration belongs to step 2 and is unverified.

## Shared hunk preservation

Two independent daemon hunks bind/register cleanup before the control listener
and start native HTTP immediately before existing dashboard serving. The
supervisor explicitly released independent native hunks.
No donor branch was imported, merged, overwritten or rearranged. Preserve:

- #1002 frontend CheckCall/Retry/Converse and Conversations;
- #1005 frontend/repository TurnContext;
- #1013 the control Caller's StartUnlisted:true (native stays false);
- #1043 MaxResidentAgents assignment;
- #1010 attachments import/Manager and #1033 launch/lookup;
- #1012 Outbox People;
- #1048 revokeUnservedInputs immediately after LinkThreads and its error
  return: failed revocation must precede all request acceptance;
- #1049 environment assignments;
- #1109 Workers registration and loginCtx/loginCancel/loginDone plus shutdown
  join, in their existing positions before their dependencies close.
- #1118 settings-free research preflight assignment.

These pending owner hunks are not imported into this PR. Each owning PR must
recheck the latest main and keep its own tests/review/CI gates when integrating.
The current integration order is #1115 → #1109 → #967 → #968.

## Evidence and remaining acceptance

| Issue acceptance criterion | Current evidence / remaining gate |
| --- | --- |
| Private socket, ownership/mode/link/long-path and cross-user refusal | Transport guard tests pass locally; executed Darwin ACL and privileged cross-user evidence are missing. |
| Owner Caller only by listener identity | Unix HTTP/daemon fixtures and separate authority review cover this boundary; TCP/header/bearer promotion is refused. |
| Existing page, reads/streams/writes/operate and native bootstrap | HTTP/asset and API/stream/WS fixtures pass; nested core child joins are repaired/tested; page bootstrap shared build remains held by #1054/#1108. |
| Host/Origin/method/route/isolation and browser sign-in | Hostile requests fail; complete dashboard package and focused parallel-browser daemon tests cover browser regressions. |
| Busy incompatible root/version discovery without lock deletion or duplicate daemon | Transport binding tests and disposable daemon duplicate/version/restart fixtures cover this. |
| Public formats and tests that fail without guards | No control/event wire change; private HTTP setup is specified above; transport and HTTP overlay records prove behavioral guard failures. |

These are scoped evidence statements, not an approval of full acceptance.

`validation.json` records the transport component's source hashes, focused
serial/race results and 13 behavioral guard-removal failures at its component
review boundary. Reproduce with `python3 docs/notes/desktop-listener/check_guards.py`.
`http-validation.json` separately records current daemon/dashboard hashes,
focused serial/race results and HTTP/lifetime behavioral guard failures;
reproduce with `python3 docs/notes/desktop-listener/check_http_guards.py`.
Go overlays use disposable files and never mutate repository sources.
Compilation failures are not counted as behavioral evidence.

`terminal-validation.json` records the separately authorized core Terminal
delta, with source hashes, three behavioral guard removals and serial/race
held-child tests plus focused native dashboard/daemon regression runs.
Reproduce with `python3 docs/notes/desktop-listener/check_terminal_guards.py`.
Its fixtures use only disposable in-process metadata/fake producers and start
no agents or host processes. Older records retain their original component
boundaries; the new record supersedes their previously unmet nested-child gate.

The daemon fixtures run no agents, chat services or tmux. They exercise explicit
startup, separate owner Caller, parallel browser sign-in, busy/incompatible
lock/discovery preservation, shutdown and restart. HTTP fixtures exercise page
and asset delivery, reads, writes, resume positions, streams, operate/attach,
rejection/isolation and held API/producer joins (including HEAD/disconnect).

The cross-user fixture requires an already authorized privileged isolated
runner. It never invokes sudo or alters accounts and skips locally, so
cross-user acceptance remains unmet. Darwin libc/ACL/mount fixtures have not
executed here. No real Mac, local tmux/full suite, production/configuration,
service, credential or host operation is part of this work.

The minimum page/build releases and native bootstrap are implemented. The
maintainer now authorizes the separate `native-listener-proof.yml` workflow:
only PR #1115, standard disposable Linux/macOS runners, exact PR head checkout,
ordinary-user Darwin ACL/kernel-peer/non-cgo checks and elevation of only the
compiled cross-user fixture with an empty environment. It uses the PR event,
not an unregistered dispatch. Named PASS results without skips must be recorded;
adding the workflow or compiling a test binary is not executed acceptance.
Existing draft/full CI and policy are unchanged.

Still required for #1071: executed supported-platform ACL/cross-user security
acceptance; fresh final independent Codex implementation and authority reviews;
full required Linux/macOS CI on the final head and exact main merge SHA; merge
and issue closure. Issue #1071 specifies Codex review and forbids running Claude.
Earlier references here to an additional mandatory Claude/provider gate had no
PR-specific primary source and do not establish one. Other PRs retain their own
explicit review requirements. Draft static/web/cross-build successes are not
runtime gates and do not establish complete daemon/HTTP/page acceptance.

#1072 starts in a separate issue/branch/PR only after #1071 merges; #1073 follows
#1072. Real-Mac/device acceptance, Windows/WSL, signing/spending and public
packaging gates remain with their later steps. No unsigned installer is
published here.
