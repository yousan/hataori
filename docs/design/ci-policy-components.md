# Integration-policy implementation

[#1107](https://github.com/yousan/hataori/pull/1107) implements components of
[#1103](https://github.com/yousan/hataori/issues/1103), explicitly stacked on
unmerged phase-one #1102 at `a4942c86`. See [the policy design](ci-policy.md).
The parent's actual macOS failure and #965/#1100/#1105 repairs retain their
existing owners. This PR does not close #1103.

`internal/cipolicy` now includes GitHub provenance verification, a durable
SQLite coordinator/outbox, and complete Git-object change selection. No
runtime or Actions caller activates it. No live policy, authority root/key,
integration branch, dispatcher, schedule, profile or routing is installed.
Existing main PR targets, full ready/main gates, mandatory provider/authority
reviews, Pages and deployment sources remain unchanged. Draft successes and
skips are partial development evidence, never full validation or adoption.

## Independent authority and actual provenance

`NewAdapter` verifies canonical Ed25519-signed `AuthorityRoot` bytes against an
independently provisioned public key. The coordinator's key/root/database must
be outside candidate checkouts and candidate-writable inputs/artifacts. This
is an explicit trust bootstrap, not approval inferred from a `Trust`, owner
string, label, fixture or success conclusion. No actual root is provisioned by
this implementation. Test keys and mutated API variants are not real approvals.

The root fixes numeric repository identity, control/default and integration
refs, approved immutable control revisions/policy digests, responsible owner,
all required review identities, dispatcher, workflow identity, execution-file
digests, job scopes, required steps and runner labels. The HTTPS adapter checks
actual repository identity and default ref; actual merged control PR/head/base;
latest exact-head `APPROVED` reviews from every required identity before merge;
actual owner identity; and immutable policy/execution content digests. Comments
are not approvals. Integration high-risk changes and explicit per-PR provider
requirements also require their exact-head approvals. No provider requirement
is automatically waived or converted into a cheaper repeated review.

The signed scope digest includes the control, execution, classification/review
requirements and dispatcher. State and every sweep persist it. A renewed root
cannot silently change a running proof's requirements. Expiry refresh alone
preserves scope; changed requirements use `SwitchControl`, which refuses an
uncertain/bound proof, atomically retires only a definitely unsent prepared
intent, retains all known history/classification/failure, and invalidates
coverage back to the fixed bootstrap root. It cannot initialize at an arbitrary
new head or discard debt. Keep the frozen root available to finish a sent proof
before switching; a root expiry requires a signed expiry renewal.

`Bootstrap` requires the signed fixed target and an actual applicable full
Actions proof plus immutable ancestry. `Initialize` accepts only that sealed
baseline, once. Actual installation still needs a compatible approved workflow,
complete reviewed execution closure and valid live baseline. A declaration that
a workflow or helper is trusted does not establish that closure's correctness.

Actions proof matches repository/head repository, run ID, attempt 1, event,
workflow ID/path, frozen target/control/policy/scope, unique intent title and
actual dispatcher/triggering actor. All target execution files must match the
approved closure. Complete attempt-specific job enumeration must have unique
IDs/names, exact run/attempt/head and only approved jobs. Every applicable scope
requires successful completed jobs, assigned runners with approved labels,
and successful required steps. Skips, missing jobs/steps, unknown scopes,
incomplete enumeration and reruns are not full proof. Compatibility is explicitly
not applicable only when the frozen cumulative diff establishes that fact.
Release/main candidate evidence remains #1104's responsibility.

Integration history follows actual immutable first-parent commit objects and
associated merged PR records. Debt age uses GitHub's `merged_at`, never candidate
commit timestamps or workflow inputs. Complete compare enumeration is required;
its 300-file cap fails closed. Previously stored authenticated prefixes are
retained and only new suffixes fetched. Each operation has a signed bounded API
request budget; exhaustion cannot grant coverage. The separate local Git-object
selector below handles uncapped complete tree diffs.

## Serialized persistence and recoverable dispatch

`OpenStore` creates a dedicated private SQLite file with an application ID,
WAL and synchronous FULL. It refuses unrelated/permissive files and symlinks;
it never replaces an existing database. An interrupted initial database creation
that cannot prove identity fails closed and needs owner diagnosis. Candidate
write access to this file or coordinator credentials is outside the trust model.

`BEGIN IMMEDIATE` and version CAS serialize admission, current transaction clock,
policy/root expiry, observation freshness, coverage, failure and outbox creation
across independent coordinator handles. Observations come only from sealed
stored snapshots and verified API data. Count/age/lead fences apply even when
scheduling is late or another proof is running. High risk requests early coverage.
One unique active intent and one cumulative pending range are retained; empty,
docs-only and already-covered ranges create no sweep. An older success clears
only its prefix; newer debt keeps its count and original merge age. Failures
retain debt; responsible-account authentication and diagnosis are required for
recovery. They do not waive provider, merge or publication authority.

The same transaction commits a frozen intent with a random ID before dispatch:

1. `prepared`: safe preflight verifies the dispatcher and target execution
   closure, creates/reconciles a deterministic `ci-sweeps/<intent-hash>` ref at
   the frozen target, and constructs/budgets the HTTP request. Ref creation is
   idempotently reconciled by its unique name; refs are never moved or deleted.
2. `uncertain`: a serialized claim is committed before the single workflow POST.
   Only its winning coordinator sends. Crash/lost response/restart does not
   blindly repeat the POST. Reconciliation filters the fixed ref and target,
   checks full provenance and binds exactly one observed run. Multiple matches
   quarantine; missing visibility remains uncertain and closes admission.
3. `bound`: completion must match the persisted run/attempt and frozen tuple.
   A completed proof becomes `done` in the same transaction as debt advancement.
   A prepared intent superseded by an explicit control switch is recorded as
   `retired`; it is never represented as an executed or successful proof.

This is not an external Actions exactly-once guarantee. A crash after the
uncertain commit but before sending cannot be distinguished safely from a lost
response. Without a uniquely matching run, admission stays closed; there is no
timeout retry, cancellation of the sole proof, or claimed automatic recovery.
Expired policy blocks new admission/dispatch but still allows an authenticated
already-bound proof to finish, then an explicit approved control transition.

## Conservative complete-diff selection

`CompleteDiff` requires immutable base/head IDs, verifies Git object type and
SHA-1/SHA-256 content hashes, and walks both complete trees. It does not checkout
candidate files, execute their tools/hooks, or use their diff drivers. Renames
are conservative deletion plus addition, retaining both paths. Invalid objects,
unknown/status inputs, incomplete enumeration, missing dependencies or invalid
Go source escalate to full/high risk.

Only explicitly ordinary version/chat utility files receive targeted Go
selection. New/unknown Go paths, authority/plugin/runner/process changes,
policy/dependency inputs, executable documentation contracts and captures stay
full/high risk. Docs exemptions are narrow prose/site paths; protocol, plugin,
configuration and policy/testing documents are executable or gate contracts.

`Select` derives the complete diff itself and parses imports from both verified
base and head trees, including tests and all platform files. It selects changed
packages and transitive reverse dependencies across both graphs, plus compiled
cross-module smoke packages. Deletion/rename cannot hide old importers. Missing
smoke packages or uncertain graphs select full coverage. Candidate labels and
policy edits never choose this classifier. This does not authorize routing
selected tests into a live partial gate; that integration remains explicit work.

## Validation and compatible continuation

Focused tests cover signed-root and actual-review rejection, provenance and
executed-step/runner completeness, authoritative history, request bounds,
concurrent coordinators, CAS conflicts, commit/send crash boundaries, lost
responses/restarts, fixed IDs/ref dispatch, stale completions, newer debt,
clock/expiry/age fences, scope refresh, immutable complete diffs, reverse
imports, cross-module smoke and conservative escalation. HTTP tests derive
records from real public API captures; explicitly mutated positives are labelled.
A separate read-only live probe checks actual repository/ancestry/merge-time
parsing and rejects the known partial draft run. It establishes no approval,
dispatch, release/main proof or active configuration. No host tmux tests run.

The next compatible increment must wire a trusted coordinator entry point and
owner-compatible admission/Done route, prove execution-closure and dispatch
contract against actual Actions, and preserve current main PR targets, upgrade
requirements, gates and provider reviews. This is runnable work under #1103,
not a new permission stage. Parent compatibility must be proved before adoption;
activation stays off meanwhile. #1104 and existing operator owners retain frozen
release-candidate/main provenance, publication and operator checks. Exact final
ready-head and actual merged-main full CI remain required before claiming a
merged/complete rollout. No unchanged queued macOS run is retried.
