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
for raw and base64 marker material. Its owner sink is an in-memory stand-in;
full direct-message integration remains blocked below. The negative controls
in `notes/worker-login/negative-guards.json` record behavioral failures for
guard removals/reversions. A separately identified persistence instrumentation
control writes an encoded input frame into an isolated file and proves the
marker scan detects it. Compilation failures do not count as regressions.
The script uses serial Go overlays without editing the original sources and
records the source hashes. These controls are not real provider evidence.
The inventory does not by itself establish that every acceptance guard has
been reviewed and covered; that gate stays unchecked until independent review
accepts the complete integrated evidence.

## Remaining blockers

1. Shared `cmd/hataori/daemon.go`: populate the relay's worker endpoints from
   the already configured, pinned transports immediately after constructing
   the relay. This draft's worker relay cannot be reached in the daemon yet.
2. Shared `internal/chat/login.go`: after resolving an explicit reply, refuse
   unknown/unrelated references before the sole-waiting-login fallback. Its
   existing wrong-message path fails #966's required authority boundary.
   Add the actual fake-surface regression after ownership release.
3. Shared `docs/configuration.md` and `docs/design/overview.md`: document
   the shipped limits, opt-in status contract, machine-keyed behavior and
   Appendix D. No new operator configuration key is proposed.
4. Coordinated worker protocol: document and version the new typed ephemeral
   `LoginData`, choose the next compatible version/capability at integration,
   and add new golden examples without rewriting released versions. Existing
   protocol version/golden files are deliberately untouched in this draft.
   No control operation/event encoding change is proposed.
5. Disposable real-CLI successful completion/status evidence for device auth
   and code-back flows. No real code-back provider adapter is enabled here;
   the missing implementation/capture cannot be replaced by a synthetic flow.
6. Process containment evidence for the supported CLI: the guard terminates
   the direct child and descendants still in its group. Descendants that
   independently detach into another session/group are not contained by that
   mechanism. The fixture does not establish that the real CLI never creates
   them. This remains a runtime/containment blocker, not a general claim that
   every possible descendant is terminated.
7. Fresh independent implementation and authority/credentials/destructive
   reviews after fixes. Initial reviews rejected the draft; their findings
   are not approvals for the revised source.
8. Final-head and exact post-merge-main Linux/macOS CI, accepted evidence,
   merge and issue closure. A passing intermediate CI run meets none of the
   missing runtime or ownership gates.

The adopted CI phase-1 policy skips full OS/tmux suites for drafts before
runner allocation. Intermediate draft static/build checks are development
feedback. Full final-head Linux/macOS execution is required after the complete
candidate is ready; draft skips never satisfy that gate and are not rerun.

The daemon, chat login file, common configuration/design documents and
released protocol/golden files remain unedited pending explicit coordination.
