# Deferred supervisor management sends

Issue #1116 / PR #1120 adds explicit opt-in durable management instructions for
an existing busy worker. Immediate sends retain their existing behavior.

The narrow [design](design.md) received independent authority and feasibility
approval at design commit `0a3a134cdaa6d5d317fd751f6d25c3b6cb5d420f` before
implementation. That approval is not implementation acceptance. Implementation
and final independent reviews/CI are still in progress.

The operation is `DeferredSend` (protocol 3.48), exposed through explicit
`defer_to_thread`, `inspect_deferred_send` and `cancel_deferred_send` tools.
Queue/proposal acceptance reserves one send budget; dispatch requires no model
wake and does not spend another send. Receipts distinguish queued, ordinary
input acceptance, frozen host handoff and exact agent acknowledgement. Keep
notes and the stable retry identity until the receipt is checked. Cancellation
before handoff records no physical input and does not refund budget; after the
cutoff it reports too late. No operation interrupts, replaces a worker, changes
provider or weakens a readiness/confinement gate.

Support requires a surviving capable owned pipe host and a declared adapter
(Codex, or fake pipes for fixtures). Claude Code, terminal submission, old/relay
hosts, remote and held/taken-in workers refuse before queue/proposal artifacts.
This change does not restart a worker to gain support. The receiver retains at
most 128 non-evicting dedupe keys per run, rejecting new keys when full; a
complete batch is bounded to 128 KiB/128 chunks. Original-run loss or a partial
write is explicit unconfirmed failure, never permission for blind replay.
Exactly once arbitrary pipe effects across receiver death are not claimed.

Proof uses retained actual native Codex captures, pure actual-host/Client
controlled-writer tests, record-only core/budget/proposal fixtures, and actual
runner codec/read/commit/warmIO/client reconciliation with an in-process pipe
peer. These fixtures start no CLI, terminal, worker or provider. They do not
establish new live provider behavior. Authored instruction goldens change only
by the exact layer insertion; existing external provider captures remain
immutable. Full runtime suites run in CI, not locally.

Completion requires meaningful restart/recovery and authority regressions,
independent implementation and authority reviews of the final head, full
applicable head and actual merge-main CI, a merged PR and a closed issue.
