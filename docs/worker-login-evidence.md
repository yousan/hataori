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
