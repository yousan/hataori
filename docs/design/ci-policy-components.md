# Offline integration-policy components

Implementation for [#1103](https://github.com/yousan/hataori/issues/1103), stacked
on the unmerged phase-one PR #1102 at `a4942c86`. The parent design is
[ci-policy.md](ci-policy.md). These components can be developed and reviewed
while the parent's actual macOS failure remains with its existing owners.

`internal/cipolicy` contains executable policy selection and coverage-debt
transitions with deterministic focused tests. Nothing calls it from Actions
or runtime code. There is no active `.github/ci-policy.json`, integration
branch, profile, schedule, routing change, dispatch, cancellation or release
authorization. Existing main PR targets, full gates, provider reviews and
Pages behavior are untouched. Neither phase one nor these profiles are
adopted merely because code exists on a branch.

## Approved control

`Resolve` takes a `Source`, `Trust` and clock. `Trust` is the responsible
coordinator's already-approved immutable control revision and allowlist of
policy SHA-256 digests. It must be obtained outside candidate-controlled
files, labels, workflow inputs and artifacts. This library does not create
approvals or authenticate a GitHub reviewer; those are adapter boundaries
that must be proved before activation. Calling the library with an anchor
chosen by a candidate would violate its contract, not establish trust.

`GitSource` reads `.github/ci-policy.json` from the pinned commit object,
including verifying that the object is a commit. Current HEAD and working
tree edits cannot replace those bytes. The strict version-one schema rejects
unknown/noncanonical fields (including case aliases), duplicate keys, trailing
values, unknown profiles, missing
owner/reason/validity, invalid budgets and an unapproved rollback revision.
Missing, invalid, unapproved, unavailable or expired control fails closed:
`FullOnly` is true and code integration is denied. It never silently grants
partial validation. Expiry is rechecked on every decision, including when
the selection was loaded earlier.

Reports identify profile, reason, responsible owner, start/expiry, approved
revision/digest, limits and rollback revision. Rollback selects the old
approved revision through the same loader and validation. If its policy is
missing or expired, rollback fails closed too. The Git-backed test commits
approved stable/throughput policies followed by a candidate's cheaper edit;
the approved anchor keeps its original limits, the candidate cannot elect
itself, and selecting the approved stable revision restores its limits.

## Coverage transition and fences

`NewState` starts with an externally proven integration watermark and a
control identity. `Reconcile` takes a complete trusted first-parent history,
a clock and optional exact completion, early-sweep request or diagnosed
recovery. The coordinator must verify history/classification and use merge
observation times, not candidate-controlled author dates. Rewritten,
incomplete, duplicate, future-dated and time-reversed observations fail
closed. State pins the observed tip, prefix fingerprint (including code/scope
classification and merge times) and observation clock. Later events must
append to that exact prefix; stale/truncated history and clock rewind cannot
erase previously observed debt. The bootstrap root remains fixed; history
compaction needs a separately verified checkpoint transition. History includes documentation commits, but only code merges incur
runtime coverage debt.

A dispatched sweep freezes its ID, old watermark, target and approved
control revision/digest and cumulative compatibility requirement. The caller
receives a value copy. New history
cannot move an active target, replace its ID or cancel it. At most one
pending cumulative target represents later uncovered code; it is a plan,
not a second allocated runner. After completion, debt is recomputed before
dispatching any successor. An older success clears only its covered prefix,
retaining the count and original age of newer debt. Empty, already-covered
and documentation-only ranges allocate no sweep, including on explicit
early requests. A documentation-only tail does not duplicate an active run.

Completion must match the entire frozen tuple. The future trusted Actions
adapter must first verify exact run/attempt/head/control provenance and all
applicable full scopes. Linux and macOS must each be `success`. Compatibility
is required only when the trusted cumulative classification frozen before
dispatch selects it; then it must also be `success`. Otherwise the adapter
must explicitly report `not_applicable`, never invent successful execution.
Partial, missing, skipped, cancelled or failed applicable results retain the
watermark and close admission. They cannot authorize a release. Candidate
and actual-main provenance remain separately owned in #1104.

Limits bound uncovered changes and age. The lead time is the measured
queue/runtime allowance; dispatch becomes due before the age ceiling or
one change before the count ceiling. At either ceiling, code admission
closes even if cron is late or a sweep is still executing. A failed sweep
does not automatically retry on pushes/ticks: recovery requires an explicit
responsible-owner identity and concrete diagnosis from the trusted caller.
This is an audit boundary for the existing owner, not another permission
question. The owner must establish actual identity in the future adapter.

State persistence is deliberately outside this pure reducer. Before wiring
it to Actions, implement serialized compare-and-swap storage/dispatch so two
coordinators cannot allocate the same transition twice. Control identity
changes cannot reuse the old ledger: transition through a newly proven
watermark under the selected approved identity. Do not reset to an arbitrary
head or copy old green results into a new policy scope.

## Evidence and remaining integration

Run `go test ./internal/cipolicy -count=1`. The tests use a fixed clock,
synthetic integration histories and a temporary Git repository; they start
no terminal, tmux, provider, browser or network service. Assertions cover
approved-vs-candidate control, rollback, malformed/unknown/expired fallback,
metadata, frozen/replayed/moved completions, cumulative pending targets,
older-success debt, deduplication, count/age/late-schedule fences, failure
and recovery. These are component proofs, not Actions or release validation.

Remaining #1103 work is the trusted Actions/history adapter, atomic durable
coordination, changed-package/reverse-dependency/smoke selection, and explicit
owner-compatible routing/Done integration. Activation requires compatible
phase-one adoption and reviewed adapter evidence; existing test repairs stay
in #965/#1100/#1105. #1104 and the existing operator owner retain frozen
candidate/main promotion and upgrade/Pages compatibility. No arbitrary
sample-count or repeated full-suite rehearsal is required for these pure
components. Refresh only evidence affected by later changes.
