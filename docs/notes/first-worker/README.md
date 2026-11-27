# First usable worker gate (#965)

This gate starts after #964's final-head and post-merge-main Linux/macOS CI
passed. It covers one main, an isolated SSH server and resident worker, a fake
chat surface and a fake agent. Each thread keeps one machine and one directory.

The acceptance suite must prove automatic and explicit placement, follow-up and
question/answer, durable capacity waiting, scope and binding refusals, restart
and uncertain-write recovery, held actions and stale-target refusal, confirmed
worker loss, and worker-local removal/restore/purge crash guards. Removing
effect deduplication, catch-up-before-dispatch or a deletion guard must make
the corresponding regression fail.

The product guide must describe only shipped configuration and compatible
manual Linux installation with an existing agent login, owner CLI attachment
and recovery. Another worker adds resources, never provider allowance.

Evidence must distinguish isolated fake-worker integration, Linux/macOS CI,
and actual platform/runtime checks. No actual macOS worker installation,
login, Keychain, LaunchAgent, sleep/wake or real-agent runtime is established
by this suite. No production, services, local tmux or local full-suite access
is part of this issue. Remote login, upgrades, dashboard and full macOS
conformance remain in #966–#969; this thread stops after #965.

Completion still requires acceptance proof, two fresh independent Codex reviews,
final-head Linux/macOS CI, merge, exact post-merge-main Linux/macOS CI and issue
closure. This document records scope, not completed evidence.
