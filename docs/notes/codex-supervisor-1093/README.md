# Pending instruction recovery (#1093)

This follow-up addresses the commit/act crash gap in confined startup.
The runner can retain a thread/start or thread/resume response before its
generated instruction injection reaches the live agent. Reconnecting replays
that response through warmIO but intentionally discards ordinary Write actions.

Recovery must reconcile only the trusted instruction snapshot, retain the
correlated acknowledgement gate and withhold all queued turns until success.
No worker management or arbitrary input may be replayed as a shortcut.

Acceptance requires captured-output regression coverage through actual runner
reconstruction, fresh independent code and confinement reviews, and green
final-head and post-merge main CI. This is separate from #1091 capture cleanup.

## Recovery behavior

An unopened confined codec rebuilds the pending injection, including its
original request ID and complete trusted composition, from retained output.
Its first live Tick retries only that request. Open disables this recovery
path for a normally launched codec. The runner reads and commits available
output before Tick: an already waiting or committed acknowledgement clears
the gate without retrying. Missing trusted instruction state, error replies
and malformed acknowledgements still refuse startup.

The retry can append another identical developer snapshot if the first copy
was applied but its acknowledgement is delayed. It does not deduplicate
native history. The snapshot explicitly replaces earlier instruction
snapshots, and no queued turn is allowed before the correlated empty-result
acknowledgement. A second reply cannot repeat readiness or dispatch an input.
Ordinary writes and worker calls remain excluded from replay. A subsequent
reconnect can retry the same still-unconfirmed snapshot once again.

## Evidence

`TestConfinedCodexReconnectRecoversInstructionGate` uses the actual runner
pipe host, commit, follow, warmIO, poll and act paths with the actual Codex
codec. A disposable Python peer emits only existing real captured CLI
responses. Start and resume each cover an unsent injection, an unread
acknowledgement, a committed acknowledgement, repeated reconnects, malformed
acknowledgement and refusal. Inputs remain held before acknowledgement and
are dispatched once afterward; the native conversation remains the same.
The start/resume unsent cases fail with the codec fix reverted.

`TestConfinedInstructionAckReplayOrdering` also checks both original and
reconstructed codecs with reordered rate-limit responses/notifications.
`TestConfinedRepeatedInjectionAcknowledgements` replays a fresh real CLI
capture of both replies, checking one readiness event and one queued turn.

The opt-in `TestRealCodexInstructionInjectionRetry` used the actual confined
adapter launch on Linux with codex-cli 0.163.0 and gpt-6-luna, disposable
directories and existing CLI authentication mounted read-only. Two identical
injection requests were sent before reading either reply: both returned an
empty acknowledgement, exactly one readiness event occurred, and no turn
started through two further idle seconds. It passed in 3.25 seconds, with
no inference requested. Only the two empty replies and counts are stored in
`supervisor_injection_retry/`; no startup identity metadata was exported.

The pinned CLI's [injection implementation](https://github.com/openai/codex/blob/rust-v0.163.0/codex-rs/core/src/session/inject.rs)
records trusted items into idle history rather than starting a turn. Its
[app-server regression](https://github.com/openai/codex/blob/rust-v0.163.0/codex-rs/app-server/tests/suite/v2/thread_inject_items.rs)
also checks that injection alone sends no model request. This narrow retry
proof supplements, and does not replace, #1088's launch/management evidence.

No production input, data, configuration, service action or switch is used.

Local validation passed: formatting, vet, the Codex package, the 12 runner
recovery cases plus retained-range/refusal regressions, and focused command
importers including daemon restart during a Codex turn. Full suites remain
CI-only; no local tmux test runs. The opt-in CLI check is separate from the
default test suite and never requests an inference turn.
