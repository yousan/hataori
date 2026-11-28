# Frozen candidate evidence, without live promotion

Repository implementation for #1104. The release/operator owner retains live
promotion and operator responsibilities. This code does not activate a profile,
schedule, route, branch, publication or upgrade. Existing main PRs keep full
ready-head and separate actual-main checks; Pages/provider gates remain separate.

## Callable evidence path

The independently signed root may supply a `Candidate` request. It freezes PR
number, dedicated source ref, main base B, source head H, tested result C, tree T,
source integration checkpoint G, expiry, approved control/policy/scope, dependency
digests, full proof intent, mandatory reviewers, domain-check app identities and
main workflow/actor identities. Candidate labels, files, workflow inputs and
success strings cannot provide this authority. No candidate is configured by
default; the existing root remains compatible when this optional field is absent.

```
ci-coordinator candidate-freeze AUTHORITY_DIR --pull N
ci-coordinator candidate-prepare AUTHORITY_DIR --pull N
ci-coordinator candidate-pump AUTHORITY_DIR --pull N
ci-coordinator candidate-dispatch-reconcile AUTHORITY_DIR --pull N
ci-coordinator candidate-verify AUTHORITY_DIR --pull N
ci-coordinator candidate-main AUTHORITY_DIR --pull N
ci-coordinator candidate-status AUTHORITY_DIR --pull N
ci-coordinator candidate-invalidate AUTHORITY_DIR --reason DIAGNOSIS
```

These commands use the real GitHub adapter and private durable store. Only
`candidate-pump` may create the fixed proof ref and send one workflow dispatch
for a prepared intent. They never send a merge, rerun or cancellation. All results
have PromotionAllowed/ReleaseAllowed/UpgradeAllowed false. The CLI deliberately
reports Closed=true and nonzero exit even after successful evidence collection:
an unproved promotion route is still closed. Status is historical evidence, not
a cached allowance. Internal success must not be translated into worker Done.

Freeze validates actual ready/open PR, its GitHub test-merge result C and refs, exact ordered C parents B/H and
tree T, approved policy bytes at C, pinned dependencies/main workflow sources,
and the complete B..C classifier result. Unknown or truncated diff closes; no
candidate can choose its own compatibility exemption. G must already be an
ancestor of B (or equal B). Every first-parent merge G..H needs actual integration
PR provenance and its mandatory exact-head reviews before integration, including
reviewers in an applicable signed owner grant and high-risk requirements. Manual
writes, ambiguous association, missing provider reviews and incomplete ancestry
fail. The initial source contract retains integration merge commits; a squash or
rewritten source needs a separately reviewed provenance contract, not an assumed
equivalence. Empty source ranges do not justify another candidate full run.

One frozen/verified candidate is active in the serialized SQLite path. Its tuple
is immutable; a new head/control cannot overwrite it. Restart reuses its ID and
scope. Invalidation authenticates the responsible owner, retains tuple/evidence
and diagnosis, and prevents a late verifier from committing over it. A different
tuple may then be frozen without deleting the old record. Repository/key identity
remains pinned across this history. Concurrent observers use revision CAS; these
database guarantees do not serialize external GitHub writers. Every mutation
rechecks current root/policy validity inside that serialized path; historical
on-time candidate evidence may be observed after candidate expiry, while expired
current authority cannot commit a new main observation.

## Durable candidate dispatch

After freeze, `candidate-prepare` creates a local pre-lane outbox row. It does not
initialize a coverage lane or send anything. A unique intent ID cannot be reused
from candidate history, bootstrap or a coverage sweep; only one candidate intent
can remain prepared or uncertain. Prepared, uncertain, bound and retired receipts
survive restart and are never deleted by invalidation or a new candidate.

`candidate-pump` first discovers an existing matching run. If none exists and the
intent is still prepared, it uses the shared real Actions sender: validate the
approved execution closure, workflow registration and dispatcher identity; create
or reconcile the deterministic `ci-sweeps` ref at C; recheck the ready candidate,
refs and exact mandatory approvals; then atomically claim the intent as uncertain
in the private SQLite transaction before the POST. The request uses the frozen
ref and exact integration-proof inputs/run title. Expired authority or candidate,
observed movement and missing approvals refuse sending. These reads do not make
GitHub refs/reviews atomic and do not confer promotion authority.

A successful dispatch POST returns no run ID. A lost response or crash, including
a crash after the durable claim but before sending, therefore remains uncertain.
No pump/restart retries that POST. Read-only `candidate-dispatch-reconcile` uses
complete bounded run discovery and the current run endpoint to identify the
original attempt 1, frozen target/ref, workflow, actor, scope and timestamps. It
never creates refs or sends. Duplicate, unknown, mismatched or rerun metadata
cannot bind. This is at-most-one local send claim, not an external exactly-once
promise: a zero-run uncertain receipt needs diagnosis, not blind resend.

Reconcile may retain a historical receipt after candidate expiry, ref movement or
review withdrawal, using the unchanged signed request and still-valid current
control/root. It cannot extend expiry or grant full validation. Full verification
still checks fresh candidate/ref/review state and all required executed proof
steps; a retained outbox must bind that exact run before evidence is committed.
Restart thus connects the original receipt to evidence without trusting a
success string. Binding queued/running/failed metadata is only identity capture.

Owner invalidation may retire a definitely unsent prepared intent. It cannot
abandon an uncertain send or a bound run that is still running/unknown. A known
terminal original run may be invalidated while its receipt remains retained; a
new tuple must use a new intent ID. No path cancels a proof or releases coverage
debt. Root/key/credential/process/DB provisioning and the first real candidate
execution remain external, unacquired inputs; merging this code activates none.

## Full proof and artifacts

The frozen `Sweep` has a unique `candidate-` intent ID, From=B and Target=C. Its
title and frozen `ci-sweeps` ref use the existing integration-proof contract.
Verification discovers exactly one matching actual run: no successful previous
run is required before constructing/freezing the request. The registered manual
workflow can execute that first proof after its trusted dispatcher/ref route is
established. Unknown/duplicate discovery never selects a newer success silently.

Full Linux/macOS, reproducible web, cross-build and applicable ten-version tmux
steps must actually execute under the approved workflow/closure/runner contract.
Attempts other than 1, skip/partial/failure/cancellation, wrong repository/event/
head/ref/workflow/scope and late/unknown timestamps cannot validate C. The run
must follow the actual control approval/merge and policy start, and finish before
the candidate expiry. Expiry closes validation; a delayed schedule grants no
extension. Mandatory candidate and source reviews retain their exact scoped
identities. Additional domain checks bind GitHub app ID/name, head and success,
not a candidate-created check with the same name. Actions-backed checks must
also match check-run IDs from executed jobs of the already verified workflow;
the shared Actions app identity alone is insufficient.

Only candidate-prefixed manual intents upload the two cross-built binaries, using
a commit-pinned upload action. Ordinary integration sweeps do not acquire this
extra upload. The verifier records actual artifact ID/name/run/size/SHA256 from
complete GitHub metadata, requires unexpired provenance for C and its original
attempt, and freezes that identity with run/job/check/review IDs. A changed proof
identity requires explicit invalidation; an older success does not replace it.
API artifact metadata is provenance, not a claim that these binaries were built
from M or that publication/upgrade is authorized.

## Actual main and the unproved promotion boundary

`candidate-main` requires C's verified evidence to predate the actual PR merge M,
with M's ordered parents B/H and tree T. It independently discovers original
successful main push CI and applicable tmux runs at M, verifies required full
steps, frozen main workflow/filter sources and domain checks, and records new
run/job IDs. C's run or artifact is never copied as M evidence. Current main must
still be M at observation; historical status later grants no right to upgrade a
newer main. The existing CI-green origin/main operator check remains mandatory.

Pre/post HTTP ref reads reject observed base/head movement, but cannot prevent a
write immediately after the last read, mutable review revocation or ABA changes.
There is no promotion sender or assertion that GitHub's head-pin merge API guards
B. Before any future activation, the operator route must prove a compatible
server-enforced both-ref/serialized promotion contract, confined writer authority,
PR/rules recognition and handling of uncertain outcomes without duplicate writes.
The existing integration Git receiver tests do not establish that contract for
GitHub/main. Candidate evidence alone cannot bypass existing PR targets, required
provider/Claude reviews, main checks, Pages or operator release/upgrade consent.

Rollback invalidates an eligible candidate under the authenticated owner,
retains its evidence and returns work to existing full main routes separately.
It does not reset coverage debt, retarget another owner's PR, cancel proof,
rewrite history or roll back an external main write. No routing/profile adoption
or live authority is supplied by merging this implementation.

## Local and live evidence

Focused tests use captured real GitHub payload shapes with explicitly mutated
positive/negative variants, disposable databases and concurrency boundaries.
They are not actual approval, candidate dispatch, provider promotion or adoption.
Existing final-head and merged-main CI are compatibility evidence for those exact
revisions only. Live proof must record the actual frozen tuple, run/attempt/jobs/
artifacts/reviews and separately verified M through the existing owner route.

Dependency identity here means pinned repository locks and approved execution
sources. Hosted runner images and the existing main workflows' floating action
tags are not claimed hermetic or immutable resolved runtime dependencies. The
release proof workflow pins its action commits; any additional provider/operator
requirements remain explicit before adoption.
