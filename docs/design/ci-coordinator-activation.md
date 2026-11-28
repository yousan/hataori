# Trusted coordinator and inactive activation bundle

Implementation: #1103 / stacked draft #1107. This is a callable repository
increment, not an installation or adoption record. Parent #1102 merged as
`86ee315d` after exact ready-head full Linux/macOS and compatibility success;
its separate actual merged-main evidence is recorded on that PR. Parent evidence
is not validation of this control or its dispatch. Profiles, schedules and routing
remain off. Existing main-targeted PRs, full ready-head and actual merged-main
proof, provider reviews, Pages and upgrade source remain unchanged.

## Concrete entry point

Build `cmd/ci-coordinator` from the reviewed immutable control revision into a
trusted location outside candidate checkouts. The binary accepts only explicit
commands; it has no installer, timer, signing, key-generation, rerun, cancellation,
merge, deployment or credential-issuance command:

```
ci-coordinator bundle TRUSTED_REPOSITORY IMMUTABLE_REVISION
ci-coordinator bootstrap AUTHORITY_DIR
ci-coordinator observe AUTHORITY_DIR
ci-coordinator admission AUTHORITY_DIR --pull N
ci-coordinator pump AUTHORITY_DIR
ci-coordinator status AUTHORITY_DIR
ci-coordinator control-transition AUTHORITY_DIR
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

`bootstrap` verifies the signed fixed full baseline and initializes once.
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

Development performs format/vet/build/test compilation/cross-build plus verified
complete-diff targeted and reverse-dependency/smoke tests through `cmd/ci-feedback`.
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
or merge PRs. An opted-in grant freezes PR number, head/base/ref, responsible
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

The owner hook calls `admission` immediately before using its exact reserved
base/head route, then calls `observe` after actual integration. Integration
branch writes must be confined to the compatible owner route; a JSON allowance
is not an atomic GitHub merge operation. Competing/manual unmanaged branch writes
are not made safe by a check-then-merge convention. Prove that caller/branch-rule
compatibility with the existing owner before enabling routing. No existing owner
or main PR is migrated here.

`done` checks actual merged PR/history and the durable full watermark. An uncovered
merge, draft/partial feedback, expired policy or missing mandatory review cannot
be Done. Successful integration Done is explicitly integration-only; `ReleaseAllowed`
and `UpgradeAllowed` remain false. Current main workers still require exact full
ready-head plus separate actual merged-main evidence. Release/candidate/operator
promotion stays with #1104 and existing operator owners.

## Actual identities, permissions and proof still needed

The responsible owners supply the real numeric repository, dispatcher, control
owner and mandatory provider/authority reviewer identities; approved merged PR,
head/control SHA, policy digest/expiry/rollback; workflow ID/path; full baseline
run/attempt/tuple; and exact owner grants. A supplied name or successful fixture
is not that evidence. No root is signed or accepted as live by these docs.

The dispatcher needs GitHub metadata/PR-review/issue-comment read access, Actions
write for dispatch, and Contents write only for frozen ref creation. Any GitHub
workflow-write scope required by registration/ref semantics is an explicit
prerequisite to verify, not permission granted here. Runtime proof jobs themselves
have only Contents read. Signing key custody is separate from dispatcher tokens
and candidate runners.

Actual dispatch cannot yet be proved for a new workflow absent from the default
branch's registered workflows and an independently approved compatible control,
root, target execution closure and real baseline. Parent #1102's
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
