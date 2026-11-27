# Machine placement implementation (#963)

The accepted policy is in design section 6.4 E. Admission requires a fresh,
ready, reconciled machine, scope and agent permission, logged-in agent health,
and room under every configured resource limit. Rank eligible machines by
remaining resident-slot fraction, normalized load, free memory and key.

The main must commit the fixed machine and its reservation before workspace
creation. Automatic starts without room retain their thread, link and input
durably; explicit ineligible requests refuse without a fallback. Pending work
is reconsidered oldest first with current authorization and configuration.

Validation uses isolated fixtures and serial focused local checks. Full Linux
and macOS suites run in CI. This work does not establish actual macOS worker
runtime support. Final-head CI, independent fresh Codex reviews, merge,
post-merge-main CI and issue closure are required before completion.

Focused evidence before CI:

- Policy table covers every eligibility gate, deterministic ordering and no
  fallback. Core fixtures cover concurrent reservations, durable repeated-key
  waiting/order/cancellation, current-grant revocation and attachment recovery.
- An actual isolated resident runs explicit placement, automatic waiting and
  dispatch after verified end, reopen/sleep wake while full, and conversation
  replacement without changing machine/workspace. A lost successful start reply
  recovers its original receipt and produces one turn with one start intent.
- Local unknown prior holders block replacement/release. Phase-two recovery
  recognizes the original host independently of fresh new-start resource gates.
- Worker fixtures cover every admission limit, exact registered repository
  resolution, occupied-place refusal, purged remake, restore/transfer/restart and
  repeated purge/remake. Existing removal proof and purge crash checks pass.
- `negative-guards.json` records seven test failures with individual guards
  removed. `negative_guards.py` restores every candidate in `finally`.
- Serial focused checks pass for placement, core, runner, worker, wire, chat,
  supervisor, store and workerproto. Protocol documentation/golden checks,
  `gofmt -l .`, `git diff --check` and serial `go vet` pass. Full suites and
  platform checks remain CI gates; these fixtures do not prove real Mac runtime.
