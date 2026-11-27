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
