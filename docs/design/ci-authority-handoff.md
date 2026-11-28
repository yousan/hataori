# Public authority observations and initial control

Read-only observation on November 28, 2026 for #1103/#1104; not live authority,
role assignment, service installation or permission to dispatch/promote.

| Public input | Observed value |
| --- | --- |
| Repository/default | `yousan/hataori`, `main` |
| Accepted implementation/main | `cc7a192d69571defcd125f3cf02beeee075d8092`, PR #1114 head `9983354d25dcdec3dbf2265a48e350890b1c066d` |
| Exact-main full CI / tmux | Actions `46126592224` / `46126592215`, push attempt 1 SUCCESS, actual full steps |
| Registered proof workflow | `.github/workflows/integration-proof.yml`, ID `433472301`, active, zero runs |
| Main CI / tmux workflow IDs | `415474141` / `432147149` |
| Actual policy source digest at that main | `cef6b6c714cefdf505d50abcee74bee9ddfdedd05b3ac7564cffd68dcf5aeabd` |
| Actual #1114 reviews | Five COMMENTED; none APPROVED. The native Codex review targets older c856; the final-head no-findings receipt is an issue comment. Neither is native APPROVED. |
| Proposed integration ref | `develop` absent at observation; no branch created |

Public API reads can resolve repository ID, public numeric account identities,
workflow IDs/paths, exact immutable contents/digests, PR author/head/merge times,
review IDs/states/commits, run attempts/jobs/steps/runners and artifact metadata.
The operator handoff keeps numeric user/role observations separate from source
configuration; repository ownership or an authenticated CLI account is not an
assigned signer, confined dispatcher or mandatory reviewer role. Nonpublic IDs,
hosts, filesystem paths, credentials and ops details do not belong here.

## Unacquired versus impossible for the old revision

`reviewRequirements` requires every pinned provider/authority identity's actual
latest APPROVED review on the exact head, by a nonauthor, submitted by merge time.
All five #1114 reviews are COMMENTED. Therefore post-merge approval, a new signature
or reusing final code-review comments cannot make #1114 eligible under this
unchanged contract. That specific historical provenance is unavailable, not just
waiting for a retry. `a.control` validates every declared Controls entry, so an
initial live root must not retain an ineligible #1114 entry as approved.

The existing PR author cannot approve their own PR ([GitHub review rule](https://docs.github.com/en/pull-requests/how-tos/review-pull-requests/reviewing-proposed-changes-in-a-pull-request#submitting-your-review)).
The same principal could provide authority approval on a genuinely independently
authored PR. A same-author future control needs another eligible, assigned
reviewer. GitHub's [review API](https://docs.github.com/en/rest/pulls/reviews#create-a-review-for-a-pull-request)
supports APPROVE with eligible GitHub App user/installation credentials and Pull
requests write permission. This is an available mechanism, not evidence that the
current Codex integration has that permission, behavior or authority assignment.
No provider is contacted and no credential is issued by this audit.

A complete bounded scan of reviews on twenty recently updated PRs found no
APPROVED state. This proves no currently observed approval mechanism in that
sample, not that no suitable account/app exists. Eligibility/role assignment and
actual approvals remain unacquired external inputs. A COMMENTED review must not
be relabeled APPROVED or stand in for a separately mandatory provider gate.

## Minimal lawful first-control sequence

Use one actual control/policy PR and obtain all required eligible nonauthor
exact-head approvals before merge. Keep the existing full ready-head and separate
actual-main proof. Its real `main-full` policy needs no predecessor policy or
future SHA. Pin that eligible merged control, workflow/closure, actual policy
digest and responsible identities in the independently managed root.

Then the existing pre-lane outbox prepares a signed frozen request with RunID 0,
preflights registration/closure/ref/dispatcher, dispatches once through the trusted
route, discovers the exact attempt and initializes only after full success. It
does not require a successful run of this workflow before its first execution.
The zero current run count is missing live proof, not a circular bootstrap gate.

Actual role selection, independent signing-key custody, a trusted process/DB
principal isolated from candidates, confined dispatcher/writer permissions,
provider/rules compatibility and exact owner adoption remain with the existing
operator owner. These are concrete external inputs, not another general approval
stage for routine reversible repository changes. No root/key/token/service is
created, and current main/Pages/provider/upgrade contracts are unchanged.
