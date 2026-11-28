# Trusted coordinator and inactive activation bundle

Implementation: #1103, merged #1107, and follow-up #1114. This is a callable repository
increment, not an installation or adoption record. Parent #1102 merged as
`86ee315d` after exact ready-head full Linux/macOS and compatibility success;
its separate actual merged-main evidence is recorded on that PR. Parent evidence
is not validation of this control or its dispatch. Profiles, schedules and routing
remain off. Existing main-targeted PRs, full ready-head and actual merged-main
proof, provider reviews, Pages and upgrade source remain unchanged.

## Concrete entry point

Build `cmd/ci-coordinator` from the reviewed immutable control revision into a
trusted location outside candidate checkouts. The binary accepts only explicit
commands; it has no installer, timer, signing, key-generation, rerun, cancellation, deployment or credential-issuance command.
The separately disabled `integrate` command is restricted to the signed owner
contract; it is not a generic PR merger:

```
ci-coordinator bundle TRUSTED_REPOSITORY IMMUTABLE_REVISION
ci-coordinator bootstrap-prepare AUTHORITY_DIR
ci-coordinator bootstrap-pump AUTHORITY_DIR
ci-coordinator bootstrap AUTHORITY_DIR
ci-coordinator observe AUTHORITY_DIR
ci-coordinator admission AUTHORITY_DIR --pull N
ci-coordinator integrate AUTHORITY_DIR --pull N
ci-coordinator pump AUTHORITY_DIR
ci-coordinator status AUTHORITY_DIR
ci-coordinator control-transition AUTHORITY_DIR
ci-coordinator recover AUTHORITY_DIR --reason DIAGNOSIS
ci-coordinator bootstrap-recover AUTHORITY_DIR --reason DIAGNOSIS
ci-coordinator done AUTHORITY_DIR --pull N
ci-coordinator summary AUTHORITY_DIR --run N
```

`bundle` prints a nonsecret, unsigned, inactive JSON bundle with the exact source
revision, running binary SHA256, immutable execution-file digests, expected
workflow jobs/steps/runner labels, missing real identities/evidence and commands.
It neither invents an approved control nor signs a root. Invalid/missing source
objects close bundle generation. Working-tree edits do not change its contents.
The remaining commands use the real HTTPS GitHub adapter and durable store;
they are not watchers or fixtures. JSON errors expose closed categories without
raw errors, paths, response bodies, credentials or authority payloads.

`recover --reason DIAGNOSIS` explicitly handles a diagnosed failed sweep. It
refreshes authenticated control/history and verifies the credential's actual
`GET /user` numeric identity and login against the approved responsible owner.
A dispatcher credential for a different account cannot recover. Empty/oversized
diagnoses, expired authority, unknown control and a still-active or ambiguous
sole proof fail closed. The diagnosis is a nonsecret audit entry in the same
serialized CAS as recovery; root, watermark, cumulative debt and old intents
remain. A new fixed-ID proof is only prepared, not sent: recovery has no rerun,
cancellation or dispatch side effect. Normal admission/provider/Done/full-proof
requirements still apply. Diagnosis does not clear the failure/admission fence,
including below-budget early failures: only a successful complete replacement
advances the watermark and clears failure. Pending recovery stays `Closed=true`
and returns a nonzero CLI gate status; do not interpret that as permission to
repeat recovery or dispatch. Supply the responsible credential through the existing
external trusted boundary; this command does not issue or replace credentials.

`AUTHORITY_DIR` is a dedicated private directory outside Git checkouts. Its
files are `authority.pub` (base64 Ed25519 public key), `root.json` (exact canonical
signed bytes), `root.sig` (base64 detached signature), `dispatcher.token` (private
external credential), and `coverage.sqlite` (created once). The root pins the
actual executable SHA256. The loader reads/fstats the same non-symlink authority file, rejects hardlink
aliases, writable public-key/root/signature files, public credential files,
untrusted ownership and replaceable writable ancestors; a root-owned sticky
temporary parent does not let other users replace the private child directory.

The binary, independent key and directory need a trusted OS principal/process
boundary. **0700 does not isolate candidate processes running as the same OS
identity.** Candidate Actions run separately on ephemeral GitHub runners, with
read-only repository permission, no persisted checkout credential, no authority
directory, no dispatcher secret and no signing key. Do not run this coordinator
from candidate code or expose its credential to candidate tests. No principal,
key, signature, token, directory or service is provisioned by this PR.

`bootstrap-prepare` and `bootstrap-pump` provide the durable pre-lane first-proof
route; `bootstrap` verifies its exact bound full baseline and initializes once.
The [owner adapter contract](ci-owner-adapter.md) defines cold start, diagnosed
failed-baseline recovery, the actual integration caller and provider proof limits.
`observe` obtains fresh actual history/completion and advances debt/CAS/outbox.
`pump` performs safe registration/credential/closure/ref preflight before marking
uncertainty; ambiguous POSTs reconcile the unique fixed ref without blind resend.
`status` checks fresh authority/policy identity and exposes intent/coverage; it
is not an admission permit. `control-transition` revalidates full retained history
for a changed scope, refuses uncertain/bound proof replacement, and preserves
debt while retiring only definitely unsent intents. Expired/unknown policy cannot
admit work. Already-bound full proofs can still finish after policy expiry under
the same authenticated scope; root expiry needs a signed expiry renewal.

## Repository execution closure

`.github/workflows/integration-proof.yml` has only `workflow_dispatch`. It has no
PR/main/schedule/workflow-run trigger or cancellation group. Existing CI and tmux
classifiers now use complete Git ranges rather than the capped PR-files API;
their heavy suites, names, concurrency and other triggers are unchanged. Pages is
byte-identical to the parent. Merely adding the proof workflow on a branch does
not register or activate a live coordinator route.

The dispatcher sends nine exact inputs: `intent`, `from`, `target`, `control`,
`digest`, `authority`, `scope`, `title`, `purpose`. Run name is the frozen sweep
title. `.github/ci/proof.py` checks attempt 1, workflow_dispatch, immutable tuple,
canonical title and deterministic `ci-sweeps/<intent-hash>` ref. It accepts no
input that waives a gate. The adapter always dispatches `purpose=sweep` for the
integration outbox. A separate explicitly approved development run is partial
feedback and cannot satisfy the full sweep jobs.

| Scope | Required executed steps |
|---|---|
| contract | Validate frozen dispatch |
| development | Development static checks; Targeted and cross-module smoke |
| linux | Full Linux suite; Cross-build macOS |
| macos | Full macOS suite |
| web | Reproducible web build |
| each applicable compatibility version | Full applicable tmux compatibility |

The CI implementation lives in `cmd/internal/cipolicy`, private to these commands;
it does not add a forge-specific adapter to runtime core. Development performs
format/vet/build/test compilation, the source-only core/adapter guard, cross-build
and verified complete-diff targeted and reverse-dependency/smoke tests through `cmd/ci-feedback`.
Unknown/high-risk impact refuses this lane and retains the main full-check route.
Sweep runs full OS suites, web reproducibility and cross-build. Its compatibility
matrix preserves the existing ten versions and applicable holder/runner/hostterm,
split mutant and repeated split tests; those execute only on GitHub runners.
The copied shell restores separate-step working-directory/environment boundaries.

The bundle pins workflow/scripts, all coordinator/feedback library sources,
module locks, web package scripts/locks and toolchain selectors. New action uses
are immutable upstream commit SHAs. tmux release archives are SHA256-checked
against `.github/ci/tmux-sha256.txt` before extraction; hashes were captured from
public upstream release bytes, not a local installed tmux. Existing workflows
are not changed. Go/npm downloads are lock-verified; GitHub runner images,
setup-action distribution services, Python and OS package repositories remain
platform dependencies, not falsely immutable project artifacts. Scope review
must assess these boundaries and refresh approved control when execution files
or dependency locks change. Candidate application/test code executes as untrusted
CI input without coordinator credentials.

Actual registration metadata (workflow ID/path/active state), fixed-ref workflow
source, display title, matrix/skip job names, runner labels, steps and attempt
must be captured and matched before adoption. The bundle's expected manifest is
reviewable configuration, **not observed successful execution**. Optional skipped
matrix aggregate names may be accepted only as skips; they cannot substitute for
any required compatibility version or OS job.

## Owner opt-in, admission and Done call boundary

A signed root defaults to `RoutingEnabled=false` and no `Routes`. Unadopted owners
keep `main`; callers get no integration permission. The command does not retarget
PRs. `IntegrationWrite.Enabled` also defaults false; only the separately reviewed
provider-compatible contract permits `integrate` to write the exact integration ref. An opted-in grant freezes PR number, head/base/ref, responsible
GitHub owner identity, consent comment ID, expiry, mandatory reviewers and actual
feedback run/tuple. The adapter verifies the actual open non-draft PR, repositories,
base tip and an exact owner-authored consent body:

```
CI integration opt-in
repository=NUMERIC_REPOSITORY_ID
pull=N
head=IMMUTABLE_HEAD
base=IMMUTABLE_BASE
ref=INTEGRATION_REF
expires=RFC3339_UTC
```

This is a verified owner adoption requirement, not a request for another global
permission stage. The independently signed grant is additional to the actual
comment: another candidate label/comment cannot appoint its own owner. The
union of explicit per-PR provider requirements and grant reviewers must approve
the exact head. Done requires reviews and the currently verified consent body before actual
integration. Both comment creation and last-edit timestamps must predate the
merge; post-merge edits or approvals cannot retroactively authorize it. A grant
valid when integration occurred remains historical adoption evidence if full
coverage finishes after its expiry; current policy/root still must be valid.

`admission` refreshes coverage, verifies owner/reviews/feedback, and reserves one
PR/head/base in the same serialized durable admission path. Policy/root/consent
expiry, count/age and ambiguous dispatch are rechecked at transaction time.
Another outstanding reservation closes admission, including across restart.
Only an authenticated fresh same-PR/head/base grant renews that reservation;
previous grant digests remain in its audit table. Expiry never silently drops a
possibly used permit. Actual merged-and-observed history or an actual closed,
unmerged PR resolves it; closing a changed unmerged PR retires no coverage.

The actual owner hook is `ci-coordinator integrate AUTHORITY_DIR --pull N`.
It calls fresh admission, persists an exact write intent, refreshes consent,
reviews, source ref and provider merge object, then claims uncertainty in the
same serialized path as reservation, count/age and expiry. A raw smart-HTTP Git
atomic transaction guards BOTH base and actual candidate branch head. Ordinary
`git push` can omit the candidate no-op assertion, and the REST merge API's `sha`
parameter guards only the head; neither is substituted here. The receiver must
advertise atomic capability and return complete success for both commands.

Ref CAS does not atomically lock review revocation, consent edits, PR metadata,
expiry or an ABA change by another writer. The compatible isolated integration
branch and confined writer/rules contract must be proved by the responsible
operator before enabling it. Direct pushes must not bypass an existing main or
provider gate. Actual GitHub transaction/rules support and automatic PR merge
recognition are live proof dependencies, not established by local Git tests.
Unknown outcomes survive restart without resend or rollback. Only actual exact
PR merge recognition plus immutable history confirms a receipt, after which the
caller invokes `observe`. A closed unmerged PR retires a definitely unsent intent;
uncertain writes cannot be retired this way. Fresh same-PR grant replacement or
control transition can retire only definitely unsent intents while retaining audit
and debt. No existing owner or main PR is migrated here.

`done` checks actual merged PR/history, the matching admitted and confirmed write
receipt, and the durable full watermark. A covered manual merge without that
receipt cannot acquire integration Done. An uncovered
merge, draft/partial feedback, expired policy or missing mandatory review cannot
be Done. Successful integration Done is explicitly integration-only; `ReleaseAllowed`
and `UpgradeAllowed` remain false. Current main workers still require exact full
ready-head plus separate actual merged-main evidence. Release/candidate/operator
promotion stays with #1104 and existing operator owners.

## Actual identities, permissions and proof still needed

Public read-only API queries resolve the repository ID, workflow ID/path/active
state, immutable source/digests, actual reviews, run/attempt/jobs and observed
main evidence. Owners must identify which numeric accounts hold dispatcher,
control-owner and mandatory provider/authority roles, and supply the real
APPROVED provenance, independent signature/key custody, confined credentials,
valid policy/rollback and exact opt-in. An already successful new-workflow run is
not required before the first legal dispatch: its run ID is discovered by the
pre-lane outbox, and baseline success is required only before initialization. A supplied name or successful fixture
is not that evidence. No root is signed or accepted as live by these docs.

The dispatcher needs GitHub metadata/PR-review/issue-comment read access, Actions
write for dispatch, and Contents write for frozen ref creation. Opted-in
integration writing additionally needs the explicitly confined integration/source
assertion permissions and compatible branch rules; this does not grant main write. Any GitHub
workflow-write scope required by registration/ref semantics is an explicit
prerequisite to verify, not permission granted here. Runtime proof jobs themselves
have only Contents read. Signing key custody is separate from dispatcher tokens
and candidate runners.

Actual dispatch cannot yet be proved for a new workflow absent from the default
branch's registered workflows and an independently approved compatible control,
root and target execution closure. Registration is now observed on main; real
authority and first-dispatch/baseline proof remain external dependencies. Parent #1102's
ready/main gates cannot substitute for this new closure or owner compatibility.
These are precise live proof prerequisites; they do not block code, deterministic
tests or review.
No queued macOS job is retried, no sole proof cancelled, and no skipped result
is called full validation. The minimum actual contract/promotion evidence is
needed; there is no arbitrary sample count or repeated full-suite quota.

## Cost/quality summary and rollback

`summary --run N` uses existing attempt-specific Actions run/jobs metadata:
actual head/event/status/conclusions, attempt/additional attempts, job labels,
start delay and elapsed time. Skips remain skips with no invented runtime.
Start delay includes dependencies and is not advertised as runner queue wait.
Runner billing, token/money/review usage, escaped regressions and repair cost
remain `unknown` when unavailable. Authorized owner summaries can supplement
these later; no private transcripts or production data are read.

Rollback is fail-closed and preserves evidence: stop the external caller, disable
new routing in the independently approved root, retain the same private database,
fixed root/watermark, grants and outbox. Keep the frozen old scope available for
an uncertain/bound proof to reconcile/finish. Do not cancel, blindly resend,
delete the database/refs, invent a new baseline or reopen admission on a timeout.
A separately approved rollback control follows `control-transition` with all
known debt/reviews retained and old coverage invalidated conservatively. Existing
main PR targets/gates/default/upgrade source remain the fallback. No systemd,
production configuration, deployment or destructive migration is part of rollback
implementation here.
