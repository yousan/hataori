# CI policy and staged release validation

Status: design for #1101. Phase 1 is the compatible implementation in this
change. Later phases below are proposals with explicit activation gates;
this document does not activate profiles, create branches or authorize a
release, deployment, provider review or operator change.

## Objective and current boundary

Minimize total delivery cost: Actions queue/runtime, reviewer time and model
tokens, plus the time and consequences of finding and repairing regressions.
Runner minutes alone are not the objective. Detect dangerous changes early;
batch redundant work where the cost of a delayed finding is bounded.

At the design baseline, `main` is the default branch, all 25 open PRs target
it, there is no development integration branch and no published release.
The existing operator upgrade contract requires CI-green `origin/main`.
These are migration constraints, not permissions to change the default
branch, PR targets or upgrade source. Existing main-targeted workers retain
their exact-head and exact-main-merge CI evidence and Done rules. Pages and
its operator checks remain separate and unchanged.

## Phase 1: adopted behavior

Keep main's applicable full Linux/macOS suites and compatibility checks.
Keep full applicable suites for ready PRs targeting main during transition.
Preserve a unique concurrency group for every non-PR run, including main;
never coalesce or cancel separate main runs. Superseded runs of one PR may
cancel, including a ready run when that PR returns to draft.

Draft code PRs run inexpensive formatting, static analysis, compilation,
cross-compilation and web checks. Their full Linux/macOS suites and tmux
matrix are skipped before runner allocation. `ready_for_review` starts the
applicable full gates on the same head without another push. A later ready
push starts them again. Job names remain stable; a draft skip is development
feedback, never evidence of full validation or permission to merge.

Documentation exemptions are an allowlist: root Markdown, `docs/`, `site/`
and `LICENSE`. Package files, captured fixtures and embedded Markdown such
as `internal/supervisor/layer.md` remain code regardless of extension.
Unknown paths, dependencies, workflow/policy files and generated assets
remain code. Evaluate the complete PR diff, or the entire pushed main range;
renames include both old and new paths. The last docs-only commit cannot
hide an earlier code change. Docs-only changes skip unrelated runtime and
build jobs through job conditions, retaining their check names.

Before merging a main PR: mark it ready, wait for the applicable full gates
on its final head and obtain its mandatory independent/provider/authority
reviews. Check the separate full push run on its actual main merge SHA.
A failed check remains failed; a skip, another revision's green result or a
cross-build is not Linux/macOS runtime evidence. Keep the existing failure's
owner; do not expand this policy change into unrelated process-cleanup or
flaky-test fixes. Never rerun a queued macOS job to improve its position.

## Target phases: development, integration, release

The target separates development feedback from release authorization.
An opt-in integration branch is proposed; `main` stays the default and the
operator's upgrade source until an explicitly approved compatible change.
No open PR is silently retargeted. Until migration is enabled, the phase 1
main workflow remains the effective policy, even if more expensive.

| Stage | Checks and evidence | Boundary |
|---|---|---|
| Development | Format/static/compile, focused tests of changed packages and importers, relevant web checks, regression tests and risk-specific review | Feedback identifies the exact head, risk tier and checks actually executed; no release claim |
| Integration | The same targeted checks, plus bounded cumulative full sweeps over immutable integration SHAs | An integration merge is recorded as integrated, not released or deployable |
| Release-candidate PR to main | Full Linux/macOS suites, applicable full tmux matrix, web reproducibility, cross-builds, regression evidence and mandatory reviews on a frozen candidate | No release PR merges on partial, skipped, stale or unrelated results |
| Main promotion | Actual merged SHA receives the existing full checks; verify candidate identity and operator checks before upgrade | Keep exact merge-SHA evidence and the CI-green main upgrade contract |

### Risk tiers

Classify the complete cumulative diff and its dependencies, not its size.
If classification is uncertain, use the higher tier.

- Documentation: only the phase 1 allowlist; no runtime or unrelated build
  claims. Static-site Pages checks retain their existing independent policy.
- Ordinary code: focused regression tests, affected packages and importers,
  format/static/compile and relevant web checks on development/integration
  PRs. Full integration sweeps bound what these checks miss.
- High risk: working-directory removal/reset, protocol/state migration,
  authentication/authority, process ownership/cleanup, host terminals and
  transport, dependencies, generated/embedded assets, CI/policy changes or
  unclassified paths. Require the affected full OS/compatibility tests and
  independent domain review before integration, not only at release.

Any explicit existing Claude or other provider/authority gate is additional
to this table. A profile, Codex review or cost target cannot replace it.
Unavailable mandatory evidence blocks that change. This work neither
contacts Claude nor grants permission for real-provider, Mac or operator
checks. Their existing authorized owners supply those checks separately.

### Bounded integration sweeps

Each sweep records last successful integration SHA G and frozen target H,
and validates the cumulative G..H changes. New commits cannot mutate H.
Full OS suites cover the target; compatibility selection considers the
whole cumulative range, including dependencies and policy changes.

Allow one active full sweep and at most one pending successor for integration
only; the successor may absorb newer integration heads. Never cancel the
active sweep merely because development continued. Main runs and frozen
release gates are never coalesced. A release candidate always needs its own
complete immutable gate, regardless of the latest integration result.

A scheduler must enforce both a merge-count bound and an elapsed-time bound.
If a bound is reached without a fresh successful sweep, pause further code
integration until it passes. A delayed cron event is not proof of freshness.
On sweep failure, pause affected integration and release promotion, keep the
last green watermark, and assign the failure's existing owner. Bound initial
triage to two diagnostic probes; further attempts need an explicit incident
plan, rather than a retry loop or one fresh full run for every worker.

### Immutable release candidate

Record a candidate tuple: target base B, candidate head H, tested merge/result
commit C and tree T, policy/workflow revision, dependency locks, selected
checks and artifact digests. Create a dedicated frozen candidate PR after
integration selection; do not use a moving integration head as a release
approval. New code, a changed base, changed policy or changed dependency locks
invalidate affected checks and reviews and require a new tuple.

The release authority checks the tuple and obtains all applicable reviews
before merging. Promotion must serialize competing main merges and reject a
changed base/head. An API option that pins only H does not protect B. A
follow-up must prove this race prevention before enabling the new release
route; a convention to "check just before merge" is insufficient.

The actual main merge M may have a different SHA from C, even with tree T.
Verify its identity/tree and run full checks on M under the existing upgrade
contract. Do not copy C's green status onto M or call a candidate artifact a
build of M. Until a separately reviewed promotion mechanism proves exact
commit/artifact provenance and operator compatibility, keep this second
main validation. No deployment source is switched by this document.

## Changeable profiles (planned, not enabled)

These are initial trial limits, not measured performance promises. Both
profiles retain all high-risk and immutable release gates.

| Setting | Throughput trial | Stable/default fallback |
|---|---|---|
| Ordinary development/integration | Targeted checks; batch ready code pushes | Targeted checks plus full ready integration-PR suites |
| Full sweep trigger | First of 8 code merges or 2 hours since last successful sweep | First of 2 code merges or 30 minutes |
| Active/pending full sweeps | One active, one cumulative pending; pause on freshness bound/failure | Same bounds; prioritize repair and release evidence |
| Release candidate and main | Full gates, no coalescing | Same full gates, no coalescing |
| Trial lifetime | At most 7 days, explicit authority renewal | Persistent fallback; still reviewed weekly |

A profile activation must be a reviewed, versioned repository policy change
with owner, start, expiry, thresholds and rollback revision. Do not introduce
a temporary variable, mutable secret toggle or paid/self-hosted runner to
select a profile. Expiry without renewal falls back to stable; it cannot
silently extend a throughput exception. The follow-up implementation must
prove expiry behavior even if the scheduled sweep is delayed.

Start with at least 48 hours and 20 ordinary changes of baseline measurements,
plus three successful release-candidate rehearsals. The release authority
and CI owner must agree that existing failures have owners, mandatory gates
are available, sweeps meet their bounds and no severe escape remains open
before opting into throughput. Lack of samples is lack of evidence, not a
reason to treat an unmeasured profile as safe.

Immediately return to stable and pause promotion for one data-loss,
authority or protocol escape; two other escaped regressions in a rolling
seven days; two missed sweep windows; or two failed candidate attempts for
the same unresolved cause. Investigate rather than repeatedly re-reviewing
unchanged code. Resume throughput only after the cause is repaired, at least
48 hours of healthy stable sweeps and a fresh explicit authority decision.

## Measurements and review cadence

Store a sanitized evidence ledger keyed by policy/profile and immutable SHA:

- Queue wait, elapsed and billable runner minutes by OS/check; p50/p95
  ready-to-integrated and candidate-to-green time. Separate cancellation,
  deliberate skips, runner acquisition failure and tests that actually ran.
- Review wall time, provider/model/role, input/output/cache tokens when
  available, cost and findings that caused a repair. Mark unavailable usage
  unknown; do not read production data or other workers' private transcripts
  to fill gaps. Aggregate from authorized summaries and public CI evidence.
- Manual retries and their reason, failure owner, diagnostic/review rounds,
  time-to-detection/repair, and regressions first found after integration or
  release, with severity and affected candidate. Count a known flaky failure
  as a failure category, not as successful validation.

Compare total cost per accepted change and per release, including repair and
rollback effort; a faster pipeline with more escaped regressions can lose.
The CI owner checks bounds/failures daily; the release authority reviews the
cost/quality ledger weekly and at every profile renewal or severe incident.
Each candidate has its own promotion review. Retune thresholds through a
reviewed policy revision using both latency/cost and escape evidence.

Reuse a review only for the same role/provider and unchanged reviewed tuple
and relevant diff, with an explicit link to its scope. Review the repair's
delta and impacted boundaries instead of commissioning an identical full
review repeatedly. A changed base that affects the boundary invalidates the
relevant review. Release authority still checks the final candidate; reuse
never waives a mandatory provider review or another authority boundary.

## Sequenced implementation and owners

Owners below are accountable roles, not permission to modify operator state.
Each follow-up needs its own issue, claim, branch, PR and exact acceptance
evidence. Operator-specific work belongs in the operator tracker.

| Order | Owner | Work and binary acceptance |
|---|---|---|
| 1 (#1101) | CI maintainer | Design reviewed for cost/quality and release-authority compatibility; phase 1 draft/ready/docs behavior proven with Actions; exact ready head and main full gates green; Pages unchanged |
| 2 | CI maintainer with release authority | Add an authorized metrics ledger, risk classifier and bounded-sweep scheduler in shadow mode; prove cumulative ranges, count/time admission fence, delayed schedule handling and one-active/one-pending behavior without reducing current main checks |
| 3 | Release authority and worker-instruction owners | Approve an opt-in integration branch and worker Done contract; inventory every open main PR and its mandatory review gates; retain existing targets unless each owner agrees; no default branch or upgrade-source change; prove unreleased integration cannot be installed as a release |
| 4 | CI maintainer with release authority | Implement frozen candidate PRs, tuple/artifact ledger and race-safe promotion admission; three rehearsals reject moved base/head/policy and missing provider reviews, and verify actual main merge identity/full CI before operator use |
| 5 | Operator owner with release authority | In the operator tracker, prove compatibility of upgrade and Pages/operator checks against the release route; explicit source-change approval if ever needed; retain known-good main and rollback evidence; no automatic deployment from a partial integration result |
| 6 | CI maintainer with release authority | Activate one time-limited profile only after steps 2–5 and baseline criteria pass; prove stable fallback/expiry, incident pause and rollback, then measure the seven-day trial before renewal |

Until each prerequisite is accepted, it stays planned. Phase 1 does not claim
the integration/release architecture is deployed. Revert phase 1 with a
normal reviewed PR if it suppresses an applicable ready/main gate; preserve
main run history and never use retries or a status rewrite to conceal that
failure. Ongoing provider-specific approvals and operator/release authority
remain effective throughout the migration.
