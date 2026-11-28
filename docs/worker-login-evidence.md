# Ephemeral worker login evidence

This records acceptance evidence for #966, part of #956. An unchecked gate
blocks completion and merge. CI does not prove provider login at runtime.

## Required evidence

- [ ] Main verifies the owner and the exact outage/message before sending a code.
- [ ] Only the selected worker's registered agent configuration receives it,
  and only while its login command waits for a code.
- [ ] Clean command exit and the same worker/configuration's CLI status check
  close only the original machine-agent-configuration outage.
- [ ] A marker link/code reaches only the direct-message sink and live memory
  or transport: no database, events, receipts, replay, session/diagnostic logs,
  or process arguments contain it.
- [ ] Disconnect, timeout and main/resident restart end the attempt. Reconnect
  replays nothing; a fresh button press starts a fresh command.
- [ ] Code-back and device-auth parsing use real captured CLI output from
  disposable configurations, including the required completion/status cases.
- [ ] Every authority, isolation and no-replay guard has an observed failing
  regression with that guard removed, then passes with sources restored.
- [ ] Fresh independent Codex implementation review and separate fresh
  authority/credentials/destructive-operation review approve the final head.
- [ ] Final-head and exact post-merge-main Linux/macOS CI pass; issue closes.

## Capture blockers

The existing Codex 0.163.0 device-auth capture records the link/code display
and cancellation, not a successful provider login. Its notes explicitly
identify completion text derived from source rather than a runtime capture.
That text cannot establish a captured successful completion. Required new
runtime cases remain blockers until disposable real-CLI evidence is supplied
or safely obtained. Synthetic fixtures test transport and authority but do
not replace provider evidence. Never use an existing login or production
credentials to obtain it.

## Coordination

New login-specific files belong to #966. Shared daemon, control, runner,
store, wire, worker-protocol and common documentation changes require explicit
ownership coordination. Initial shared integration order: #966, #967, #968.
Recheck protocol versions and documentation ownership before merging.

## Candidate implementation evidence

The isolated tests cover owner and guest checks, original outage selection,
explicit and omitted default configuration directories using the runner's
actual lowercase `spec` encoding, two residents, clean exit plus a separate
status command, failed status execution despite success-shaped output,
bounded output/queues/status duration, disconnect during blocked stdin,
correlated cancellation before immediate replacement, and abrupt death of an
owned fixture process. A dedicated EOF guard ends its own live group after
the direct child exits, including a fixture descendant. It also terminates
its still-owned direct child if that child changes session. The private guard
descriptors are close-on-exec; an empty filtered environment cannot inherit
the resident's ambient environment. No saved PID lookup or process-name
cleanup is used.

The new `agent.WorkerLoginStatus` contract opts an adapter into a bounded,
guarded status command whose zero exit certifies login. Only the Codex adapter
opts in here. Adapters requiring status-output interpretation are refused;
their existing local login behavior is unchanged. Synthetic code-back tests
exercise the relay but do not enable or prove a real code-back provider flow.
No other agent's implementation or login configuration was changed.

`TestWorkerLoginCapturedDevicePrompt` replays the existing real Codex capture
through the pipe reader and the actual Codex decoder. It verifies link/code
delivery and refusal of code-back input. Its helper's exit is not a captured
provider completion and cannot certify login.

`TestWorkerLoginOwnerOutageAndPersistence` checks the isolated main database
and worker roots, including receipts/session state, and the diagnostic sink
for raw and base64 marker material. `TestWorkerLoginFakeSurfacePersistence`
also runs the actual fake chat driver, front end and relay with the resident
for code-back and device-auth fixtures.
The guest's button opens no transport, the owner's button starts the attempt,
the link reaches the direct-message sink, and the owner's exact reply ends
only the selected outage. The device fixture accepts no stdin code; its owned
loopback completion barrier models the external page without a provider login.
The guard, login and status helpers record their
real process arguments; the four observations are included in the marker scan.
This is disposable fixture evidence, not a real provider login. The negative controls
in `notes/worker-login/negative-guards.json` record behavioral failures for
guard removals/reversions. Separately identified persistence instrumentation
controls write an encoded input frame or an actual device output frame into
isolated files, or put a fixture marker into observed process arguments.
The current inventory contains 39 behavioral guard removals/reversions and
three persistence instrumentations. Seven removals cover the new protocol's
minor/capability negotiation and refusal, durable-key rejection, typed payload
validation, input limit and contract check before a configuration probe.
The scanner checks raw markers and bounded recursive decoding of base64 tokens,
including base64 of the entire `LoginData` JSON inside a transport frame. The
three controls prove those forbidden writes/arguments are detected; they are
instrumentation, not guard removals. Compilation
failures do not count as regressions.
The script uses serial Go overlays without editing the original sources and
records the source hashes. These controls are not real provider evidence.
The inventory does not by itself establish that every acceptance guard has
been reviewed and covered; that gate stays unchecked until independent review
accepts the complete integrated evidence.

## Scoped shared integration

The owners released only login hunks in `cmd/hataori/daemon.go`,
`internal/chat/login.go`, `docs/configuration.md` and `docs/design/overview.md`.
No donor branch was imported. Supervisor conversations, worker-home routing,
stored scopes and outside-text policy hunks remain with their existing owners.

The daemon passes its already configured, pinned runner transports to the relay
and waits for relay shutdown before closing its dependencies.
`TestWorkerLoginDaemonUsesPinnedTransport` runs an isolated daemon and fake
surface, observing the original transport without SSH or agent commands.
The front end refuses an explicit unknown or unrelated reply before the sole
waiting-login fallback. Both stored alert references and in-memory follow-up
references include channel, thread and message. The fake-surface regression
reproduced the old unknown/channel/thread defects before the fix; guard-removal
controls now check refusal, thread identity, outage selection and the owner.
Configuration and design changes document the limits and opt-in status
interface, including Appendix D. No new operator configuration key is proposed.

## Remaining blockers

The narrowly released protocol contract is now worker protocol 1.4, capability
`ephemeral_login` and operations `ephemeral_login_start`, `ephemeral_login_input`
and `ephemeral_login_cancel`. Legacy opaque operations are preserved. The
preflight record in `notes/worker-login/protocol-preflight.json` checks current
main, matching local/live PR head, sole published protocol ownership and the
unused version/name before edits. New examples and one appended checksum row
leave all 1.0–1.3 bytes and SUMS lines unchanged. These are structural wire
examples, not new provider captures or runtime proofs. The control protocol
document and encodings remain unchanged.

1. Disposable real-CLI successful completion/status evidence for device auth
   and code-back flows. No real code-back provider adapter is enabled here;
   the missing implementation/capture cannot be replaced by a synthetic flow.
2. Process containment evidence for the supported CLI: the guard terminates
   the direct child and descendants still in its group. Descendants that
   independently detach into another session/group are not contained by that
   mechanism. The fixture does not establish that the real CLI never creates
   them. This remains a runtime/containment blocker, not a general claim that
   every possible descendant is terminated.
3. Fresh independent implementation and authority/credentials/destructive
   reviews of the integrated candidate. Earlier component re-reviews approved
   their documented boundary, not subsequent integration or whole-issue gates.
   Complete integrated authority/nonpersistence evidence must also be accepted;
   the fixture/control inventory alone is not an exhaustive-coverage claim.
4. Final-head and exact post-merge-main Linux/macOS CI, accepted evidence,
   merge and issue closure. A passing intermediate CI run meets none of the
   missing runtime or ownership gates.

The adopted CI phase-1 policy skips full OS/tmux suites for drafts before
runner allocation. Intermediate draft static/build checks are development
feedback. Full final-head Linux/macOS execution is required after the complete
candidate is ready; draft skips never satisfy that gate and are not rerun.

The released shared files contain only scoped login changes. Only the new 1.4
golden directory and appended SUMS row are added. Unreleased shared hunks and
`docs/protocol.md` remain frozen. Shared integration stays #966 then #967 then
#968; successors integrate only after the prior issue merges and their exact
handoff review. No other PR's review gate is waived.
