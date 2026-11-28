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
the source-only core/adapter architecture test, cross-compilation and web checks. Their full Linux/macOS suites and tmux
matrix are skipped before runner allocation. `ready_for_review` starts the
applicable full gates on the same head without another push. A later ready
push starts them again. Job names remain stable; a draft skip is development
feedback, never evidence of full validation or permission to merge.

Documentation exemptions are an allowlist: non-policy root Markdown,
`docs/`, `site/` and `LICENSE`. Policy overrides take precedence: `AGENTS.md`,
`CLAUDE.md`, `docs/testing.md`, this policy and `.github/` remain code even
inside an otherwise exempt path. Executable documentation contracts
`docs/protocol.md`, `docs/plugins.md` and `docs/configuration.md` also remain
code: wire/plugin tests read their protocol, operations, fields and hook
policies. Ready/main changes to these paths retain the full OS suites; they
do not select the unrelated tmux matrix. The four rate-limit/statusline
captures read by adapter tests under `docs/notes/agent-usage-limits/captures/`
and the capture-metadata scrubber `docs/notes/codex-supervisor-1088/scrub_actual.py`
also remain code (the shared filter names them explicitly). Other research
captures remain documentation; runtime/test fixtures in package trees, and embedded Markdown
such as `internal/supervisor/layer.md`, remain code regardless of extension.
Unknown paths, dependencies and generated assets outside the documentation
allowlist remain code. Evaluate the complete PR diff, or the entire pushed main range;
renames include both old and new paths. The last docs-only commit cannot
hide an earlier code change. Docs-only changes skip unrelated runtime and
build jobs through job conditions, retaining their check names.

A future required-check configuration must include the classifier/static
checks or a result aggregator, not only runtime names that can be skipped
after a dependency failure. Full workflow success and actual gate execution
are the transition evidence.

Before merging a main PR: mark it ready, wait for the applicable full gates
on its final head and obtain its mandatory independent/provider/authority
reviews. Check the separate full push run on its actual main merge SHA.
A failed check remains failed; a skip, another revision's green result or a
cross-build is not Linux/macOS runtime evidence. Keep the existing failure's
owner; do not expand this policy change into unrelated process-cleanup or
flaky-test fixes. Never rerun a queued macOS job to improve its position.

## Target phases: development, integration, release

The target separates development feedback from release authorization.
An opt-in integration branch (`develop` is the proposed name) is proposed; `main` stays the default and the
operator's upgrade source until an explicitly approved compatible change.
No open PR is silently retargeted. Until migration is enabled, the phase 1
main workflow remains the effective policy, even if more expensive.

| Stage | Checks and evidence | Boundary |
|---|---|---|
| Development | Format/static/compile, tests of changed packages and reverse dependencies, relevant web checks, cross-module smoke/regression tests and risk-specific review | Feedback identifies the exact head, risk tier and checks actually executed; no release claim |
| Integration | The same targeted checks, plus bounded cumulative full sweeps over immutable integration SHAs | An integration merge is recorded as integrated, not released or deployable |
| Release-candidate PR to main | Full Linux/macOS suites, applicable full tmux matrix, web reproducibility, cross-builds, regression evidence and mandatory reviews on a frozen candidate | No release PR merges on partial, skipped, stale or unrelated results |
| Main promotion | Actual merged SHA receives the existing full checks; verify candidate identity and operator checks before upgrade | Keep exact merge-SHA evidence and the CI-green main upgrade contract |

### Risk tiers

Classify the complete cumulative diff and its dependencies, not its size.
If classification is uncertain, use the higher tier.

- Documentation: only the phase 1 allowlist; no runtime or unrelated build
  claims. Static-site Pages checks retain their existing independent policy.
- Ordinary code: focused regression tests, affected packages and importers,
  format/static/compile, relevant web checks and existing cross-module smoke
  tests on development/integration PRs. Full integration sweeps bound what these checks miss.
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

Track coverage of cumulative integration changes, not the number of runs.
A sweep records last completely covered integration SHA G and frozen target H,
and validates G..H. New commits cannot mutate H. Full OS suites cover H;
compatibility selection considers the entire range, including dependencies
and policy changes. Reuse existing same-SHA full results with matching
base/tree, workflow, dependency and check scope; an empty or fully covered
range never starts another sweep.

In stable mode, a full ready integration-PR result may discharge the same
coverage debt if its tested merge tree, base/head and workflow identity match
the integrated result. If identity cannot be proved, retain the debt. A green
older H clears only commits it covers; it never resets the count or age of
newer uncovered commits. Measure age from the oldest uncovered code merge.
Candidate/main checks remain independent where the current exact-SHA upgrade
contract requires them; do not deduplicate by pretending different SHAs passed.

Allow one active full integration sweep and at most one cumulative pending
successor. Never cancel the only validating sweep just because code continued.
Main runs and frozen release gates are never coalesced. Trigger only for
uncovered work, ahead of its hard admission bound using observed p95 queue
plus runtime. If a bound arrives before fresh complete coverage, pause new
code integration until covered. A delayed cron is not proof of freshness.

On sweep failure, pause affected integration and release promotion, retain
the last green watermark and assign its existing owner. Diagnose the concrete
check/revision and choose bounded probes against a stated hypothesis. More
probes follow evidence, not a retry loop or one new full run per worker.

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

Mode changes development cadence, never release quality or authority.
These initial budgets are provisional and adjustable alongside measurement,
not proven optima or waiting periods that prevent implementation.

| Setting | Throughput | Stable |
|---|---|---|
| Ordinary development/integration | Targeted + reverse-dependency + cross-module smoke checks; batch ready code pushes | Broader early per-change tests, including full ready integration-PR suites where useful |
| Uncovered integration debt | Initial ceiling: first of 10 code merges or 6 hours from oldest uncovered merge | Initial ceiling: first of 2 uncovered code merges or 2 hours; same-SHA full per-change evidence discharges debt |
| Full sweep admission | One active, one pending, only for uncovered work; start before the ceiling using measured queue + runtime | Same deduplication; no extra timer sweep for an empty/already-validated range |
| Release candidate and main | Full immutable gate and existing exact-main proof | Identical release boundaries |
| Mode expiry/review | Initial seven-day expiry, responsible owner renews or stable fallback | Default fallback; review when volume/risk changes |

The [observed #1067 ready run](https://github.com/yousan/hataori/actions/runs/45926062077)
spent about 2h33 waiting for macOS and 26m
executing; Linux took about 22m. Another recent PR started both OS jobs in
under a minute (#1101). A 30-minute sweep ceiling would often stop integration
before coverage could complete. The provisional longer debt budgets allow
room for observed work without asserting a universal queue. Start measuring
concurrently; revise count/time ceilings from actual uncovered debt, latency
and escapes. Stable prioritizes earlier coverage at lower volume, not more
identical runs. If actual p95 execution exceeds a budget, the fence stops
admission; the owner changes the budget or repairs capacity rather than
silently extending coverage freshness.

Use a reviewed, versioned repository policy with visible active mode/reason,
owner, start, expiry and rollback revision. Classification, profile selection
and admission use a trusted already-approved control revision, never a PR's
own label or edited policy. Candidate policy changes need validation under
the existing trusted policy; they cannot self-activate a cheaper gate.
Unknown, invalid or expired configuration falls back to stable or fails closed
if stable cannot be established. No temporary variable, secret toggle or
paid/self-hosted infrastructure is needed.

The responsible CI/operator owners may perform routine reversible rollout
steps within the maintainer's authorization; do not invent a new question or
approval stage for each branch/workflow/profile edit. Existing independent
reviews and actual publication, money, destructive-operation and provider
constraints remain distinct and unchanged. Prove expiry, late scheduling,
coverage accounting, candidate/base integrity, missing-review rejection and
rollback with focused invariant/event tests and the minimum end-to-end route
evidence needed. Do not require 48 hours, 20 changes or three repeated full
rehearsals merely to reach a count. Capture baseline alongside rollout; label
small samples and unavailable data honestly.

Return to stable and pause promotion for any severe data-loss, authority or
protocol escape, an unresolved candidate integrity failure, repeated coverage
budget breaches or a materially worse escape/repair trend. Initial incident
thresholds and the seven-day review interval are tunable, not measured facts.
The owner records the cause and evidence for repair before resuming throughput.
Renew only when the ledger shows useful cost/latency or coverage improvements
against baseline without an unacceptable reliability regression; absence of
samples is uncertainty, not claimed savings.

## Measurements and review cadence

Start with one small PR/issue evidence summary per accepted change plus
existing Actions metadata, keyed by policy/profile and immutable SHA. Do not
build a large telemetry subsystem before learning what changes the decision:

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

## Actionable rollout and owners

These are work packages, not six serial approval barriers. Each needs an
issue, claim, branch/PR and testable acceptance. Owners continue runnable
work under existing authorization and coordinate real cross-repository
boundaries. Operator-specific changes stay in the operator tracker. No step
silently retargets an open PR, changes the default/upgrade source or rewrites
history.

| Work | Accountable owner and next action | Acceptance and relationship |
|---|---|---|
| Compatible delivery (#1101) | Current CI maintainer: merge the reviewed design and phase 1 | Actual draft -> ready same-head proof; applicable ready/main full gates green; docs exemptions conservative; Pages unchanged |
| Adjustable development/integration lane ([#1103](https://github.com/yousan/hataori/issues/1103)) | CI maintainer, with existing worker-instruction owner: implement trusted risk/profile selection, targeted + smoke checks and debt-aware bounded sweeps; start lightweight measurement concurrently | Test self-weakening/unknown/expired policy, stale/late sweep admission, cumulative debt and no duplicate coverage; opt-in integration routing/Done wording explicit per owner; open main PRs retain targets/gates unless their owner agrees |
| Candidate and operator compatibility ([#1104](https://github.com/yousan/hataori/issues/1104)) | Existing release/operator owner, with CI maintainer: implement frozen candidate proof and race-safe promotion, coordinate worker Done and upgrade checks in the operator tracker | Reject moved base/head/policy and missing mandatory reviews; prove actual candidate/main identity and rollback; current main full checks/upgrade source and Pages operator checks remain until equivalent verification is implemented and tested |

The last two work packages can progress together; promotion cannot use
unproved candidate/operator invariants, but there is no arbitrary baseline
waiting period. Start the reversible implementation and record its evidence;
production rollout, publication and provider execution still follow their
actual existing authorization boundaries.

Phase 1 saves draft full suites and unrelated docs builds. Ready-main PRs
and applicable main pushes still pay their current full-suite cost; do not
claim the proposed integration/release savings are implemented. Revert phase 1
with a normal reviewed PR if it suppresses an applicable ready/main gate.
Never erase main history, rewrite statuses or retry unrelated failures to
hide a policy defect. Later profile/branch rollout must update this document
and the worker/operator contracts with the settings actually in use.
