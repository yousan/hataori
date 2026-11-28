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
head clears debt. A terminal failed bootstrap currently remains retained and
closed: ordinary lane recovery does not yet replace a pre-lane attempt. Do not
clear the database or modify the signed tuple to hide it. This explicit recovery
limitation is separate from the now-callable first successful dispatch path.

## Evidence boundary

Local deterministic tests exercise coordinator entry points with captured and
explicitly mutated GitHub payloads. They are not live dispatch, authority approval,
provider compatibility, full validation or adoption. Existing merged-main evidence
can be reused for compatibility of that exact immutable revision; it cannot
stand in for the new manual proof contract.
