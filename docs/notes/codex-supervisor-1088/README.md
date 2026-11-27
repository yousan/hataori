# Codex supervisor confinement gate (#1088)

Status: current-CLI verification in progress. No supervisor support is enabled.

Before implementing selection or launch/continuation, prove that the installed
Codex can meet every existing `agent.Spec.Confine` and `agent.Launch` guarantee
in disposable fixtures. Reuse #268's findings without treating its configuration
or captures as current proof. Keep the runner's unsupported-adapter refusal.

The proof must cover only supplied scope-bound MCP tools, no native write or
execution tools, no network/browser/subagents/background wakes, literal input,
settings isolation, MCP credentials and isolated mutable CLI configuration.
Successful launch/management/continuation requires additional start, human and
wake conversation, resume, daemon restart and two disposable worker evidence.

If an essential boundary cannot be enforced, record the precise mechanism and
bounded current evidence here instead of enabling unsupported launches.

Required before merge: fresh independent Codex code review, separate independent
confinement review, and green CI on the final head. Check main CI after merge.
Provider-error explanation and outage handling remain in #1086/#1087.
