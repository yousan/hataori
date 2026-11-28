# First usable worker gate (#965)

The executable gate is `python3 docs/notes/first-worker/gate.py`. It runs only
listed focused tests, serially with `GOMAXPROCS=1`, `-p 1` and `-parallel 1`.
The normal Linux/macOS CI suite also runs these tests. It starts no tmux or
service manager. All databases, repositories, SSH keys/configuration, Unix
sockets and foreground/detached processes belong to temporary fixtures.

## Acceptance mapping

| Requirement | Isolated evidence |
| --- | --- |
| Automatic and explicit start, follow-up, question/answer | `cmd/hataori.TestFirstWorkerPlacementChatAndDirectoryOwnership`: actual main child, fake chat bridge, generated-key SSH server, resident and detached fake terminal agent; actual distinct worker directories keep their inodes across main restart; worker has no database/main chat config and main launches no local host. |
| One main owns chat/database | The placement test checks database locations; `TestFirstWorkerCapacityScopeBindingAndOwnerAttach` checks one fake chat client run across restart. |
| Main restart mid-turn | `TestFirstWorkerMainRestartMidTurn`: tool-start observation, actual owned main PID killed before committed tool end, worker survives, one input/end and one visible answer. |
| Lost applied-write reply and dedup | `TestFirstWorkerLostWriteReplyDedupAndCatchup`: relay duplicates the identical worker operation, discards its completed receipt, hides original receipt across main restart; new placement and held input cannot bypass uncertainty; restoring original receipt yields one effect and one visible reply. API queue acceptance is distinct from transport acknowledgment. |
| Cut, held input/stop, autonomous progress | `TestFirstWorkerCutHeldInputAndStaleNamedStop`: actual SSH-forward connections cut, two repeated keys yield two held actions/one absence notice across main restart; own terminal starts another turn; retained catch-up refuses the original stop and the autonomous turn completes; held input runs once. |
| Current authority and original question | `TestFirstWorkerHeldChatRechecksPersonAndScope`: real non-owner fake-chat origin, person grant revoked or scope disabled across main restart, no worker write. `TestFirstWorkerExpiredHeldAnswer`: actual worker-hook wall expiry while disconnected, retained withdrawal and visible original-answer refusal without an answer operation. Core/runner target tests also run in the composite gate. |
| Confirmed worker loss | `TestFirstWorkerConfirmedBootLoss`: controlled boot-ID change plus termination of only fixture-owned worker hosts; one recovered end, next input on original worker/directory. This is not an actual OS reboot test. |
| Scope, explicit refusal, durable waiting, binding, attach | `TestFirstWorkerCapacityScopeBindingAndOwnerAttach`: scope refusal while room exists, explicit full-worker refusal, automatic wait and key across main restart, one launch after slot release, wrong installation refused, actual owner CLI SSH attachment. |
| Existing local data stays local | `store.TestMachineMigrationFromPreviousVersionCopy`: shipped previous-schema fixture migrates/reopens with unchanged old columns and file/directory inodes/bytes, including untracked/stash/nested/submodule fixture content. No production data is used. |
| Worker removal proof | `TestFirstWorkerRemovalPreservesSoleCopy`: main API through SSH/worker refuses deletion of sole untracked bytes and preserves directory identity. Worker component tests rerun all proof classes and declared physical-machine/independent-destination checks. |
| Rename/restore/purge crashes | Worker `TestWorkerWorkspaceCrashIdentityRecovery`, `TestWorkerCrashNeverAdoptsWorkspaceReplacements`, `TestWorkerRemovalAckRetentionAndRestore`, `TestWorkerPurgeAndAcknowledgmentCrashRecovery`: private worker-local crash checkpoints, replacement refusal, exact rename recovery, acknowledgment and retention. These remain component evidence rather than injected full-process crashes through SSH. |

`python3 docs/notes/first-worker/gate.py --mutations` removes one product guard
at a time in a disposable development checkout and restores every source in
`finally`. Its three regressions use the complete main/SSH/worker fixture:
operation deduplication, catch-up-before-dispatch and current worker removal
proof. `negative-guards.json` records the observed failing assertions.
Compilation failures, whole-test timeouts and skipped tests do not count as
proof. The observed input-dedup mutation reached the faulted write/recovery
phase and failed the original-turn count assertion; the catch-up mutation
failed the unknown-receipt readiness assertion; the deletion mutation accepted
the sole-copy removal and failed immediately. All three original sources were
restored. The serial normal gate, gofmt and vet passed; race and exact CI/review
results are recorded separately in the PR.

## Product guide and limits

[First Linux worker](../../first-worker.md) documents compatible manual
installation, an existing worker-local agent login, registered repositories,
shipped machine/scope fields, owner CLI attachment and recovery. Another worker
adds resources, never provider allowance. Existing single-machine configuration
needs no worker or directory move. Configuration and earlier worker foundation
documents point to the combined path and no longer describe implemented work
as future integration.

Public protocol 3.47 and independently versioned worker protocol 1.3 remain
unchanged: this issue adds tests and documentation, with no public interface or
Appendix D signature change. Exact reviewed head, merged tree and Linux/macOS
head/post-merge-main CI are recorded in the PR and #956 after those gates pass.

The SSH/resident/fake-agent tests are isolated integration evidence. Linux and
macOS CI are platform test evidence, not a real provider login or real-agent
worker installation. Actual macOS login-session/Keychain/LaunchAgent,
case-insensitive filesystem, native telemetry, sleep/wake/reboot and teardown
remain required by #969. No actual macOS worker support is claimed. No
production data/configuration, deployment, restart or service operation is
part of this issue. Remote login, upgrade automation and dashboard work remain
#966–#968. This reserved machine-core chain stops after #965.
