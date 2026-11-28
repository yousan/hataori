# Native listener acceptance and integration map

Issue #1071, PR #1115. The transport, daemon, HTTP and native page bootstrap
are implemented. Completion requires the review and CI evidence described below.
This is the first listener step, not complete desktop support. No Electron app,
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
before the Terminal edit, #1118 head `5b8dcf01bf61198940a4ac32eb685f6ddb9aacb5` changed
only Core's adapter-capability field and selected-agent research preflight;
its published reservation does not include Terminal/Runner producer work.
Those hunks are disjoint and are now present in main `4e54aab0`; they remain
preserved in the clean prospective merge. Other owner tests, branches and review
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
or additional maintainer decision is requested. The isolated hosted Darwin
ACL/peer/cross-user and non-cgo refusal tests passed, as recorded below. This
does not make a non-cgo distribution usable for native access.

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
At the fixed native origin, `adoptSession()` clears its cached session and
returns before adopting browser link/storage credentials. Browser bootstrap
is unchanged; the existing native HTTP and WebSocket paths send no bearer and
an empty first WebSocket session. The isolated bootstrap regression fails
without this branch (three native failures, ten browser passes) and passes
all thirteen cases with it. The existing main entry already waits for adoption
before rendering, so `main.tsx` needs no change.

#1013, #1054 and #1108 explicitly released this source branch, isolated tests,
the corresponding JS entry rebuild and index.html line 8 script reference.
Both page owners permit #1115 first and a later source-preserving rebuild and
re-hash; no hash is pinned. #1054 AllThreads/Details/api/catalog source,
#1108 UI/worker source and CSS, index.html line 9 stylesheet reference, and
#1013 unlisted source/generated behavior remain protected. A disposable
baseline build matched the previous bundle byte-for-byte; unminified old/new
entries differ only by the four executable guard lines. Every other generated
file is identical. Electron custom-scheme fetch/SSE/duplex integration belongs
to step 2; it is not a prerequisite for this listener issue.

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

Pending owner hunks are not imported into this PR; already merged main hunks
are preserved. Each owning PR must recheck the latest main and keep its own
tests/review/CI gates when integrating.
The current integration order is #1115 → #1109 → #967 → #968.

## Acceptance evidence

| Issue acceptance criterion | Implementation and scoped evidence |
| --- | --- |
| Private socket, ownership/mode/link/long-path and cross-user refusal | Transport guard tests cover private paths and discovery; hosted Linux/macOS cross-user and Darwin ACL/kernel-peer/non-cgo refusal tests pass at exact `655b4b08`. |
| Owner Caller only by listener identity | Unix HTTP/daemon fixtures and separate authority review cover this boundary; TCP/header/bearer promotion is refused. |
| Existing page, reads/streams/writes/operate and native bootstrap | HTTP/asset, API/stream/resume/operate/attach and shutdown fixtures pass; held-child tests cover nested producer joins; native bootstrap and browser regression tests pass with the narrowly released entry rebuild. |
| Host/Origin/method/route/isolation and browser sign-in | Hostile requests fail; complete dashboard package and focused parallel-browser daemon tests cover browser regressions. |
| Busy incompatible root/version discovery without lock deletion or duplicate daemon | Transport binding tests and disposable daemon duplicate/version/restart fixtures cover this. |
| Public formats and tests that fail without guards | No control/event wire change; private HTTP setup is specified above; transport, HTTP and Terminal overlay records prove 13, 14 and 3 behavioral guard failures respectively. |

These scoped results support all six implementation conditions. Full supported-
platform daemon/HTTP/page execution remains subject to the normal ready CI
contract; draft skips do not count. Independent implementation and authority
audits of `655b4b08` against main `4e54aab0` found no code correction needed.
The prospective merge tree `412042d5` preserves the released daemon/Terminal
hunks and current main's owner changes. Final immutable-head confirmation and
the actual merge-main checks are separate completion evidence.

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
All 28 recorded source hashes still match at `655b4b08`. Historical staged
limitations in these records are superseded by this map and the linked proof,
not rewritten or rerun as new evidence.

The daemon fixtures run no agents, chat services or tmux. They exercise explicit
startup, separate owner Caller, parallel browser sign-in, busy/incompatible
lock/discovery preservation, shutdown and restart. HTTP fixtures exercise page
and asset delivery, reads, writes, resume positions, streams, operate/attach,
rejection/isolation and held API/producer joins (including HEAD/disconnect).

The cross-user fixture requires an authorized privileged isolated runner. The
fixture itself never invokes sudo or alters accounts and skips without that
environment. The maintainer authorized `native-listener-proof.yml`:
only PR #1115, standard disposable Linux/macOS runners, exact PR head checkout,
ordinary-user Darwin ACL/kernel-peer/non-cgo checks and elevation of only the
compiled cross-user fixture with an empty environment. It uses the PR event,
not an unregistered dispatch. [Run 46155069781](https://github.com/yousan/hataori/actions/runs/46155069781)
passed on attempt 1 at exact `655b4b08a05f1c50dd0151cfc59c7e6879c41310`.
The PR event, head checkout and logs prove execution of the unmerged workflow;
existing draft/full CI and policy are unchanged.

| Standard hosted runner | Named tests executed without skips |
| --- | --- |
| Linux, ubuntu-latest | `TestNativeCrossUserBoundary` (0.05 s) |
| macOS, macos-latest | `TestDarwinACLGrantRefusesPrivatePaths` (0.10 s; root/state/discovery/socket/ancestor/runtime), `TestKernelPeerUID` (0.00 s), `TestNativeCrossUserBoundary` (0.03 s), `TestDarwinWithoutACLVerifierRefuses` (0.00 s) |

Both job logs print the exact SHA and runner OS. No command/permission error
occurred. CGO-enabled compilation and ordinary-user Darwin checks ran before
the single elevated compiled cross-user test; that fixture uses only scratch
paths and UID/GID 65534 verifier children. The non-cgo Darwin refusal compiled
and ran separately without elevation. [Detailed run/job evidence](https://github.com/yousan/hataori/pull/1115#issuecomment-7240758421)
is retained rather than rerunning unchanged tests. The privileged fixture
proves its isolated owner/foreign-UID combination, not every user combination.

Completion of #1071 requires final independent Codex implementation and
authority reviews, full required Linux/macOS CI on the final head and exact
main merge SHA, merge and issue closure. Issue #1071 specifies Codex review and
forbids running Claude.
Earlier references here to an additional mandatory Claude/provider gate had no
PR-specific primary source and do not establish one. Other PRs retain their own
explicit review requirements. Draft static/web/cross-build successes are not
runtime gates and do not establish complete daemon/HTTP/page acceptance.

#1072 starts in a separate issue/branch/PR only after #1071 merges; #1073 follows
#1072. Real-Mac/device acceptance, Windows/WSL, signing/spending and public
packaging gates remain with their later steps. No unsigned installer is
published here.
