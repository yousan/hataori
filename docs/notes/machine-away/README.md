# Machine absence and held actions (#964)

The accepted design overlays machine availability on the last known session
state. Transport failure blocks dispatch immediately; a durable away state
follows the configured grace (30 seconds by default). Binding/version refusals
are immediate. Unknown inventory retains reservations and never means reboot.

Accepted input, named stops and request-specific answers wait durably on the
main in order, with idempotent keys and one notice per absence. A pending stop
claims no stop occurred. Destruction, close, reopen and attach refuse while the
machine is unchecked. Reconnection verifies identity/version/inventory, drains
retained events and reconciles receipts before current-authority dispatch.
Stale stops and withdrawn/expired answers are visibly refused. Main-side codec,
start, delivery, idle, background and cleanup deadlines pause during absence
and catch-up; agent-side hook expiry remains authoritative.

Completion requires isolated connection-cut/restart/catch-up/receipt fixtures,
negative guard removals, independent fresh implementation and authority Codex
reviews, final-head Linux/macOS CI, merge, exact post-merge-main Linux/macOS CI
and issue closure. This issue does not establish actual macOS worker runtime
support. No production data/configuration, services or local tmux/full-suite
runs are part of this work. #965 remains unclaimed until all gates pass.

## Acceptance evidence

- `store.TestAwayGraceCatchupAndPersistentActiveAge`: immediate checking,
  configurable grace, persistent first failure/reason, immediate identity
  refusal, generation-specific catch-up, restart and retained reservations.
- `core.TestAwayHeldInputStopRestartOrderAndUncheckedRefusals`: durable ordered
  input/stop, repeated-key and notice deduplication, pending stop, and unchecked
  removal/close/reopen/attach refusal. The core fixture owns no processes.
- `runner.TestAwayHeldAnswerSurvivesClosedMainDatabase`: actual isolated
  resident/fake terminal hook, closed/reopened main database and runner,
  one accepted answer, one worker reply and one absence notice.
- `runner.TestAwayResidentRestartDrainsBeforeHeldInputAndStaleStop`: retained
  output/end precedes held input, stale stop is refused, and no duplicate turn.
- `runner.TestAwayNativeWrittenReportCorrelatesBeforeCatchup`: real captured
  Codex output crosses warm/read/commit with the native codec; both first and
  numeric written input retain caller correlation and retire before deadlines.
- `runner.TestAwayExpiredWorkerHookIsWithdrawnBeforeHeldAnswer` and
  `TestAwayNewNativeHookUsesWallClockAfterPause`: actual isolated hook expiry
  during absence plus a real Claude capture after historical absence. Main
  delivery deadlines pause; worker-hook wall deadlines continue.
- `runner.TestAwayUnknownInventoryAndConfirmedGoneDuringWarmReplay`: unknown
  inventory retains its slot; confirmed gone with lost retained output produces
  one interrupted recovery on the original machine. This is controlled inventory
  evidence, not an actual operating-system reboot test.
- The actual isolated resident idle-budget test spans a cut longer than the
  idle period, preserves the remaining time after recovery, and then sleeps
  only after active time expires. Active-time tests cover delivery reconstruction,
  start/background budgets and
  closed-workspace cleanup. Durable pause intervals preserve real history times.
- Authority and target tests cover revoked people/scopes, original request/run
  changes during hooks, withdrawn answers, and archived initial input that cannot
  revive after regrant. Rejection ends only its own recorded caller turn.
- Receipt tests cover actual applied input with lost reply and hidden receipt:
  missing/unknown outcomes block dispatch until the original receipt returns.
  Trigger-abort fixtures prove immediate and recovered refused outcomes cannot
  partially commit their journal and written markers. The full immediate-answer
  refusal path proves the next follower event is still committed.
- A proven transient no-write answer refusal survives restart and retries only
  after reconstruction of the original hook and current-authority checks.
  Missing receipts retain the written marker and block recovery. Receipt keys
  survive schema upgrade/reopen; reusing a deleted SQLite pending row cannot
  make a previous answer receipt retire or refuse a new answer.

`negative_guards.py` applies one guard mutation at a time to these isolated
fixtures, serially with `GOMAXPROCS=1`, and restores the original source in
`finally`. `negative-guards.json` records the observed failing assertions.
Compilation errors or a passing test do not count as evidence.

All of this is isolated fake-worker/native-capture evidence. It does not prove
installation, login, sleep/wake, teardown, SSH setup or agent runtime on a Mac.
Linux/macOS CI results and exact reviewed/merged heads are recorded separately
in the PR and tracking issue. The next first-worker integration gate is #965.
