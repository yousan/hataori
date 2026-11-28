# Story points for development issues

Adopted in [#1126](https://github.com/yousan/hataori/issues/1126).
These are development estimates, maintained during issue triage; they add no
runtime feature, scheduler, dashboard or recurring automation.

## Estimate the accepted scope

Use **1, 2, 3, 5, 8, 13, 20, 40**, relative to the references below. Points
are not hours, days waiting, business value, priority or individual performance.
Compare the whole accepted issue scope at the observation time, including
implementation, evidence, review and integration, rather than just lines changed
or the work remaining in an existing PR.

Consider together:

- Work volume and structure, specification ambiguity, investigation and context
  spread across components, designs, captures and related work.
- Verification difficulty, long test execution, compatibility and authority/data
  protection, integration and coordination with other owners.
- Expected AI/context/token load and material external stop risks, such as a
  maintainer decision or a required platform/environment whose behavior is unproved.
  There is no mechanical token-to-point conversion.

State dependencies and permission/evidence blockers separately: a number cannot
explain why work can stop. Use independent AI for machine reviews, rather than
assuming a human review queue. Every existing explicit reviewer, provider
(including Claude), approval and CI gate remains mandatory. An estimate grants
no authority to run a provider, access an environment or waive a gate.

Known large work can deserve large points even with a clear specification.
Small work can still be estimated incorrectly. Record high, medium or low
confidence with a concrete reason. If the accepted deliverable exceeds 40,
refine/split it explicitly; do not silently cap it at 40.

## Calibration references

These are retrospective **reference assignments**, not labels on closed issues,
advance estimates or historical velocity. Scope, merged PRs and their evidence
were read for the first calibration on November 28, 2026.

| Points | Completed reference and evidence | Relative load |
| --- | --- | --- |
| 1 | [#1030](https://github.com/yousan/hataori/issues/1030), [PR #1031](https://github.com/yousan/hataori/pull/1031) | Settled phase-order correction in two design sections; reconcile the superseded decision and check consistency. |
| 3 | [#1017](https://github.com/yousan/hataori/issues/1017), [PR #1029](https://github.com/yousan/hataori/pull/1029) | Bounded wrapping fix, committed UI build, before/after geometry across widths/themes and shared-asset integration. |
| 5 | [#1067](https://github.com/yousan/hataori/issues/1067), [PR #1068](https://github.com/yousan/hataori/pull/1068) | Two workflows, conservative classification/concurrency, actual Actions evidence and preserved gates. Later classifier refinements are outside this reference's original scope. |
| 8 | [#1105](https://github.com/yousan/hataori/issues/1105), [PR #1106](https://github.com/yousan/hataori/pull/1106) | Small race fix with deterministic ordering proof, stop/replacement authority safeguards, separate reviews and coordinated runner ownership. |
| 20 | [#1088](https://github.com/yousan/hataori/issues/1088), [PR #1089](https://github.com/yousan/hataori/pull/1089) | Current-CLI confinement research and counterexample, real captures, adapter/daemon lifecycle, two-worker management proof and non-destructive switching. |
| 40 | [#965](https://github.com/yousan/hataori/issues/965), [PR #1099](https://github.com/yousan/hataori/pull/1099) plus [#1100](https://github.com/yousan/hataori/pull/1100) | Broad first-worker SSH/restart/recovery/removal evidence, fixture-owned process cleanup, independent safety review and platform integration. Actual OS reboot/provider login/macOS worker runtime were explicitly outside the proved scope. |

Use 2 between the first two anchors and 13 between 8 and 20; they do not need
invented historical examples. The #965 reference includes its required cleanup
correction, not an inflation for elapsed CI delay. Historical failures, retries
and unknown causes remain limitations of that evidence, not additional points.

## Parents and estimates already in progress

A pure container has **`sp:tracking` alone**. Its comment names the children
where estimates live and excludes the container from additive totals. Do not
give it a numeric roll-up that would also count its children.

A phase tracker with its own actionable acceptance work is different: identify
and estimate only its independent integration/acceptance scope, explicitly
excluding child implementation and child-specific acceptance already estimated
there. State this boundary in its comment. An issue title containing “Tracking”
does not decide whether it is a pure container. Dependency scope is never added
to the dependent issue's estimate.

For this first rollout, an issue with investigation or implementation already
started, a partial/merged PR, or a released claim is a **late estimate**. Say
so even if `in progress` was removed while waiting. It estimates accepted scope,
not remaining effort, and must not be presented as an advance estimate or used
to claim past velocity.

## Triage and readback

1. Read the body, substantive comments, linked acceptance/design material and
   current PR state. Capture a paginated, counted open-issue inventory, excluding
   PR objects; record the observation time and exact issue numbers.
2. Compare scope to named anchors. Choose one numeric `sp:N` label for an
   actionable issue, or `sp:tracking` for a pure container. Preserve unrelated
   labels, ownership, assignments, priority, acceptance bodies and state.
3. Add one concise English comment with `<!-- hataori-sp:v1 -->`, points (or
   non-additive treatment), confidence, named references, concrete load/reason,
   external blockers (or none identified), observation time and late status.
   Update only a marker comment owned by this rollout; never duplicate one or
   rewrite another author's text. Preserve the initial estimate and append any
   later change with its date and reason; never inflate completed points to
   account for elapsed delay.
4. Immediately before metadata execution, recheck the exact inventory and each
   target's state. Skip closed issues without reopening them and record
   exclusions/state changes; triage newly opened issues too. Mutate only SP
   labels and rollout-owned marker comments. Read every label and comment back
   from GitHub and verify they agree and unrelated labels remain intact.
5. Reconcile the live inventory again, including the rollout issue while open.
   Obtain independent AI review of calibration and a varied sample, especially
   high-point/environment-blocked issues and parent/child boundaries. Publish
   the distribution, tracking count, exclusions, unresolved exceptions and UTC
   observation time in the rollout issue/PR. A distribution describes that
   snapshot, not priority or velocity.

New issues receive the same triage through ordinary development work. Scope or
evidence changes warrant a reasoned re-estimate; review estimates after delivery
to improve calibration while keeping the original basis visible.
