# Deferred supervisor management sends

Issue #1116 adds an explicit opt-in durable management instruction for an existing
busy worker. Immediate sends retain their existing behavior.

Implementation is blocked on independent authority review of the narrow design.
That design must fix persistent identity, same-target lineage, idle eligibility,
question refusal, cancellation/revocation, autonomy and approval, and send-budget
timing. No interrupt, replacement worker, model wake or budget bypass is part of
this change.

The proposed design and its unresolved transport boundary are in
[design.md](design.md). A design review is not implementation acceptance.

Completion requires meaningful restart/recovery and authority regressions,
independent implementation and authority reviews of the final head, full
applicable head and actual merge-main CI, a merged PR and a closed issue.
