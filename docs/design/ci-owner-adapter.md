# Opted-in owner coordinator seam

Related to #1103. The existing main worker route still requires full final-head
and actual merged-main checks. An integration Done never permits release or
upgrade. No profile, schedule, routing, branch target or upgrade source is
activated by installing this code.

## Cold start

The registered manual proof workflow must exist on the default branch, and its
immutable execution closure at the signed frozen target must match the approved
control. These are read-only checks; a previous successful execution of the new
workflow is not a prerequisite for its first dispatch.

The actual `.github/ci-policy.json` must first be committed and approved; the
bundle's `PolicyTemplate` is unsigned configuration, not that source. For the
first control, `rollback_route: "main-full"` with an empty `rollback_revision`
declares the existing stricter main fallback. It avoids inventing predecessor
policies or references to future commit SHAs. Omitted/`revision` mode keeps the
existing separately approved rollback-SHA requirement; unknown/contradictory
modes fail closed. The real source bytes/digest, exact merged/head approvals,
owner, bounded validity and signed root are required in either mode.

1. The responsible operator supplies an independently signed root, pinned public
   key, reviewed binary and private database/dispatcher credential outside any
   candidate checkout. No bundle or code-review comment supplies live approval.
2. The signed Bootstrap fixes target, from, control, policy and authority digests,
   full compatibility scope, unique intent ID and attempt 1. RunID is zero for the
   first dispatch. Required actual exact-head APPROVED control reviews already
   have to exist; candidates cannot supply substitutes.
3. `ci-coordinator bootstrap-prepare AUTHORITY_DIR` durably freezes that request
   before a lane or watermark exists.
4. `ci-coordinator bootstrap-pump AUTHORITY_DIR` checks registered workflow,
   execution closure, actual dispatcher and fixed ref, then persists uncertainty
   before its single dispatch. Restart only reconciles that same request. An
   unknown/lost response never authorizes another POST or cancellation.
5. `ci-coordinator bootstrap AUTHORITY_DIR` binds the discovered exact run and
   initializes once only after all required full jobs/steps/runners succeed.
   Running, failed, skipped, incomplete, unknown or expired evidence cannot
   initialize a lane. Reusing an independently pinned successful run remains a
   compatible path when no pre-lane intent exists.

The pre-lane outbox and eventual lane share the private durable database. Root
renewal cannot replace an outstanding request; no reinitialization at another
head clears debt. For a positively identified terminal failed run,
`ci-coordinator bootstrap-recover AUTHORITY_DIR --reason DIAGNOSIS` requires an
actual authenticated responsible owner and a new independently signed same-key,
same-control/scope request with a different intent ID and RunID zero. It archives
the failed attempt and prepares only; a later explicit pump is the sole dispatch.
Running, ambiguous, successful, unknown, expired or differently scoped proof
cannot be replaced. No database clearing, old run rerun or blind retry is used.

## Actual owner caller

`ci-coordinator integrate AUTHORITY_DIR --pull N` connects admission -> durable
write intent -> fresh authorization -> both-ref guarded write -> exact provider
merge recognition -> observation. This is the delivered opt-in caller, not an
instruction to call a generic merge API after reading an allowance. `done` then
requires this confirmed receipt AND full applicable coverage; partial feedback,
uncovered writes and covered manual merges fail. ReleaseAllowed/UpgradeAllowed
remain false. Unadopted owners retain their existing main PR and Done contracts.
The runtime runner/worker instruction assembly has no existing CI hook and is not
silently modified; owner adoption invokes this explicit trusted entry point.

The signed root must independently enable both RoutingEnabled and
IntegrationWrite.Enabled, identify the actual writer, and carry an exact owner
grant. Actual GitHub owner consent, mandatory exact-head reviews and executed
feedback are fetched independently. No candidate label/input can enable this.
The integration branch is distinct from control/main; exact PR source and target
repository/ref are verified. The provider's immutable test-merge object must have
exact ordered parents base/head and a valid tree identity.

The smart-HTTP transaction sends `base -> merge` and `head -> head` on the real
source branch. Both old identities are validated in one atomic Git ref
transaction. Ref movements or rejection of either command reject the transaction;
malformed/partial/unknown output is not success. A missing atomic capability
closes before sending. The source assertion is a no-op, not permission to change
another owner's branch. Failed or lost writes retain uncertainty; restart only
reconciles, never repeats or rolls back. Prepared unsent intents can be retired
on authenticated closed-unmerged observation, exact-grant replacement or control
transition, with history retained. Claim rechecks the exact live reservation in
the same transaction, including renewals that do not change the lane revision.

This atomicity covers ref identities, not arbitrary PR metadata, review/consent
revocation, time or ABA writes. The operator must establish an isolated/confined
writer and compatible provider branch/rules behavior, including PR merge
recognition, before enabling it. Local actual receive-pack tests do not establish
GitHub support or authorize direct pushes past provider/main gates. Unknown
recognition holds the reservation and closes admission/Done without duplicate
writes. The historical prefix remains authenticated and is reused instead of
refetching all covered merges for each receipt.

## Nonsecret operator handoff

Read-only inputs available now: public numeric repository/user identities and
roles as selected by the responsible owner; registered workflow ID/path/state;
immutable control/target files and execution digests; actual review states and
merge metadata; run/attempt/jobs/steps/labels; exact existing head/main evidence.
Workflow registration is present; it is not successful execution. Existing
COMMENTED implementation reviews are not live APPROVED provenance. Reuse the
accepted exact-main evidence for that revision's compatibility, without claiming
it proves another target or this manual workflow.

The minimum external inputs are responsible role selection, actual mandatory
APPROVED control provenance, independently held signing key/public-key custody
and signed root/binary identity, a private durable database under a trusted OS
principal, dispatcher/writer permissions and the provider/rules compatibility
contract, plus the chosen exact owner consent/grant for adoption. Separate the
signer, trusted coordinator process and candidate process boundaries; 0700 under
the same untrusted UID is insufficient. A responsible-owner credential for
diagnosis must authenticate as that owner; a differently identified dispatcher
cannot stand in for it. The code does not issue credentials or install services.

The first lawful sequence is: approved control and registered immutable workflow
-> independently signed frozen bootstrap request -> durable prepare -> registered
workflow/closure/dispatcher/ref preflight -> one dispatch -> exact attempt-1 full
proof -> one-time initialization -> separately compatible owner opt-in. There is
no requirement for that workflow to succeed before its first execution. Policy,
schedule and routing activation, branch default/targets and deployment source
remain off/unchanged until their concrete compatible proof route is satisfied.

`main-full` fallback is the callable existing `RoutingEnabled=false` boundary:
keep the same current control, key, database, watermark and unresolved intents;
stop new integration routing. The returned `main` route is a referral with
Allowed/DoneAllowed/ReleaseAllowed/UpgradeAllowed false. It neither retargets an
existing integration PR nor validates or merges it. Owners separately use the
existing full final-head and actual-main workflow. Stop the old caller first:
a changed signed root cannot atomically revoke a process already using old
verified bytes. This is an explicit fallback contract, not a new rollback writer.

Rollback: stop the owner caller; disable new routing in an independently approved
root; keep the same database, watermark, grants, archived attempts and uncertain
proof/write intents. Retain the old scope for reconciliation. Never erase debt,
cancel sole proof, resend unknown writes, reset a baseline, or change existing
main/upgrade/release authority to make progress look green.

## Evidence boundary

Local deterministic tests exercise coordinator entry points with captured and
explicitly mutated GitHub payloads. They are not live dispatch, authority approval,
provider compatibility, full validation or adoption. Existing merged-main evidence
can be reused for compatibility of that exact immutable revision; it cannot
stand in for the new manual proof contract.
