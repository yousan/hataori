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
