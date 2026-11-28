# Durable supervisor work records

Design for [#1123](https://github.com/yousan/hataori/issues/1123).
Baseline: `dfa83589ed67c1258746e8f0224cccbb8d78e3cf` on main.
This document proposes a small durable binding between accepted work and the
existing supervisor mechanisms. It implements nothing and supplies no runtime,
provider, deployment or unattended-operation proof.

## Core progression and user policy

Core retains accepted objectives and pending actions, correlates outcomes,
recovers after restart, and offers work again at accepted triggers under current
authority and limits. The model decides priorities, plans, evidence sufficiency,
consultation and completion using the user's policy. Configuration constrains
tools, repositories, autonomy, approvals, budgets and reporting destinations.
Neither prose nor a saved record grants permission.

The existing instruction layers remain: built-in mechanics, loaded configuration,
then scope user files. Completion examples belong to user policy: reviewed merge
plus exact-main CI is appropriate for some development work, whereas a research
answer may finish with attributed sources and uncertainty. Core must support both
without recognizing GitHub, a PR or a CI job as universal success.

This is not an executable prose interpreter, workflow DSL, recurring task engine
or scheduler replacement. No new autonomous goal is inferred from a worker's
message. A successor needs an accepted bounded action or a fresh model decision
under the current grant.

## Existing mechanisms and missing bindings

| Foundation | Current source and behavior | Missing work-level binding |
| --- | --- | --- |
| #645 persistent scope directory | `internal/supervisor/sessions.go`, store `supervisor_dirs`; notes survive conversations | Notes are model-owned text, not authoritative accepted-work state |
| #656 / #653 instruction layers | `layers.go`, `layer.md`; optional regular bounded user files, content hashes and changed-file notices | No objective-bound policy snapshot/version or evaluation revision |
| #649 reminders | `reminders.go`; durable one-shot scope reminders, 5 minutes to 7 days, bounded count and send budget | No dependency/owner/action binding or continuing work obligation |
| #648 / #678 scoped evidence | `forge.go`, `mcp_forge.go`; authorized read-only forge and running-build comparisons | No attributed evidence/evaluation record; a worker answer remains a claim |
| #676 fresh wake conversation | `wake.go`, `converse.go`, `answered.go`; persisted wake position, frozen send key/text and notes | A delivered wake is not a completed objective |
| #1060 informational notices | Open existing issue; next-turn notice delivery proposed | Reuse its channel-input path; do not implement a second notice queue |
| #1116 / PR #1120 deferred busy send | Open, owned by worker162; bounded accepted send, target lineage, cancellation, current checks and receipts | Reference its action/receipt; never implement another busy-send queue |
| #1117 research capability | Merged; `research.go`, `sessions.go`, `mcp.go` refuse unsupported settings-free launches before artifacts | Preserve named evidence-route blockers; confined Codex support proves no ordinary research capability |
| #275 / PR #1038 acceptance | Existing fake-surface/fake-agent supervisor tests; open PR retains its own review/CI gates | Extend later with work-record scenarios; do not absorb its implementation |
| Chat delivery | `internal/chat/outbox.go`; stable event/link/part rows and retry keys, durable outcomes | Correlate a logical work report to existing rows, without another transport |

The store migrations contain wakes, directories, files, asked threads, reminders,
given layers and proposals, but no authoritative objective/policy/owner/evidence/
wait relation. The proven gaps are (A) that relation and guarded updates, (B)
durable reactivation/catch-up for it, and (C) terminal report/successor correlation.
Detailed owner audit and child links will be added before review completion.

## Minimal authoritative state

Use the existing store and scope identity, with an additive versioned record.
Notes remain useful narrative; they cannot overwrite this state by file edits.

| Field group | Minimum retained information |
| --- | --- |
| Identity | Scope-bound work ID, schema version, revision, stable acceptance key, creation/update times |
| Objective/source | Bounded objective text, authenticated original caller, source input/event and originating conversation, report destination reference |
| Policy | User-policy source references, exact bounded snapshot/content hashes, policy revision, who requested adoption and its source; instruction/config digest for audit only |
| Owner | Existing worker thread/session lineage, assignment revision and acceptance evidence; empty only before assignment or during explicit reassignment |
| Evidence | Append-only attributed references: actor/tool, event/receipt or external immutable revision, observation time, claim versus observation, completeness/unknown reason |
| Progress | Active/waiting/terminal; current wait reason, dependency/owner identity, event cursor or existing reminder ID, next eligible time if known |
| Next action | One bounded accepted intent: kind, immutable target/payload digest, authorizing source, policy/owner revisions, preconditions, stable key, existing proposal/action/receipt reference and cutoff/outcome |
| Evaluation/outcome | Model decision, policy revision and evidence IDs it used; pending/satisfied/unsatisfied/unknown; terminal completed/cancelled/failed reason and actor |
| Report/successor | Logical report ID, frozen content/destination revision, existing outbox row/part outcomes; at most one accepted successor relation keyed to terminal revision |

Objective text and policy are data for the model, not executable predicates.
Evidence sufficiency is attributed to the evaluating model/caller; the daemon
checks revision/provenance/authority, not semantic truth. The label
`verified-complete` means a current-policy satisfied evaluation with cited
observations, never an independent core proof of arbitrary user prose.
Recorded, queued, delivered and verified-complete are distinct. A worker input
ACK, successful tool call, completed turn or sent report proves no objective met.

Store updates use compare-and-swap on work revision. Acceptance-key retries
return the same work; stale revisions return conflict with the current revision.
Retain decisions, evidence, cancellations and action receipts; do not replace
history with the latest model summary. Bound text/reference counts and return
explicit capacity refusals before acceptance; never silently evict active work.

## Operations and transitions

These are semantic contracts for later small supervisor tools, not new wire
operations in this stage. Prefer supervisor-local operations using existing
store/API seams; any later public operation requires the protocol's independent
version/golden procedure.

1. **Accept/read/list:** authenticated in-scope input records its objective,
   policy and destination before acknowledging acceptance. Reads are scope-bound;
   foreign IDs and caller-supplied authority fields are refused.
2. **Update/evaluate:** the supervisor supplies expected revision and attributed
   evidence. Commit owner/wait/evaluation changes atomically. An evaluation must
   cite the current policy and owner revisions; unknown, truncated, withheld or
   stale evidence cannot be relabeled an observation of success.
3. **Prepare action:** record only a bounded action allowed by current APIs.
   Reuse existing proposals in `propose` mode; `watch` does not dispatch. Reserve
   the intent/key durably before any side effect, and correlate by key after a
   crash. A duplicate retry must not start a second worker or send.
4. **Reconcile:** update only from matching receipts/events for that work,
   owner lineage and action key. Worker claims may trigger evaluation but cannot
   themselves complete work. A busy owner is retained; immediate Send remains
   refusal-compatible and an explicit deferred action uses #1116.
5. **Wait/reoffer:** retain one current condition plus cursor. Relevant result,
   question, decision, dependency event, reminder or budget reopening offers work
   once to the existing waker. Persist association and catch-up position before
   advancing it. A wake accepted by a conversation consumes notification, not
   the work obligation. No eligible condition means no model polling.
6. **Finish/report/succeed:** a current-policy satisfied model evaluation commits
   a terminal decision and one logical report intent. Existing posting rules,
   mention/quiet/digest rules and authorized destination bounds still apply.
   Successor preparation is separately authorized and keyed to that terminal
   revision; terminal status cannot manufacture permission or a plan.
7. **Cancel:** an authenticated authorized requester cancels future actions and
   triggers, retaining all evidence/history. Cancellation is not interruption,
   deletion, workspace reset or a guarantee to recall bytes already delivered.

Report rendering is a model decision and consumes the current model budget.
Sending an already frozen accepted report through the existing outbox needs no
new model decision. A report is one logical identity with stable parts, not
necessarily one chat message. Pending, sent, refused, suppressed by configured
posting rules and uncertain must remain distinguishable. No sent assertion is
made while a surface outcome is unknown. A successor requiring report delivery
waits for a confirmed matching receipt; one requiring only recording follows
its explicitly accepted precondition. Unknown delivery never causes a new report
identity or blind duplicate action; use existing transport reconciliation.

## Authority, policy change and recovery

Every effect rechecks the current scope/token, repository/tool capability,
autonomy/proposal approval, target lineage, budget and current intent revision at
the existing delivery boundary. Stored caller/config digests are provenance,
not renewable grants. Revocation blocks unsent work, records the reason and
retains it; it cannot silently select another repo, worker, provider or destination.
Questions requiring a person wait on the existing question/approval identity;
neither a notice nor an ordinary model answer impersonates that decision.

Only a person's authorized instruction adopts a policy revision, using existing
user-file tuning semantics. Detect changed, missing or unreadable policy bytes
with the existing bounded file reader/hash rules: mark affected evaluations
stale, hold uncommitted effects and ask the model to reconcile under budget.
The model cannot self-edit completion requirements to pass work. Re-adoption
records who requested it and why; file text alone grants no new action. Explicit
snapshot policy stays pinned unless its adoption contract requested following
the referenced user-file revision. A changed report destination also requires
current authorization; do not leak a saved report to a replacement conversation.

Before an action's existing irreversible cutoff, cancellation, changed policy,
owner reassignment or revocation invalidates its preparation. After the cutoff,
retain delivered/too-late/uncertain as the receipt says; never pretend to undo or
resend it. Late evidence for a prior owner remains history and cannot satisfy a
new assignment. A missing/stale owner waits for a model decision or person;
there is no automatic replacement worker.

Exhausted model budget retains the record and eligible trigger until existing
budget gates allow the next decision. Preserve their current exceptions and
loop guards; do not reset counters or turn each reminder into a bypass. Only
already accepted bounded deterministic delivery/reconciliation can continue,
under the same current checks and the delivery mechanism's reserved budget.
It cannot evaluate completion, choose a plan, render new prose or start an
unapproved successor. Record loop-guard holds visibly; new work IDs do not reset
the worker's loop count.

On restart, reconcile pending keys/receipts before considering another action,
then catch up bound events from durable cursors. Missing provider information
means unknown/waiting, not success or replay. A daemon restart is not proof of
host-loss recovery; #1116 defines supported hosts and uncertainty separately.
External dependencies without an event route use an explicitly accepted bounded
existing reminder when allowed. No reminder capacity, zero reminders or an
unavailable route is an explicit unsatisfied prerequisite, never a fabricated
automatic wake or infinite hidden polling loop.

Migration adds empty structured state without parsing notes into accepted work,
deleting sessions/notes, moving directories or rewriting old receipts. Adoption
of an existing objective requires explicit source/policy/owner confirmation.
Unknown record versions hold affected work and fail closed; never discard or
downgrade it. Rollback leaves new data intact and disables new actions when an
older binary cannot safely honor it. Schema/rollback compatibility requires its
own focused tests and review before implementation is activated.

## Before/after traces (proposed acceptance)

| Trigger | Current gap | Intended trace |
| --- | --- | --- |
| Busy owner | Immediate Send refuses; notes alone must remember retry. #1116 independently fixes explicit bounded delivery | Accept work W, bind owner O, prepare bounded A, reference #1116 receipt; busy -> queued -> matching delivery; retain W pending evidence/evaluation |
| Daemon restart | Wakes/reminders survive, but objective-to-policy/action relation is absent | Restart after W/A commit or delivery before receipt association; recover same key and owner, reconcile once, catch up evidence; no second dispatch |
| Model limit | Existing waker holds events, but no accepted-work ledger ties all obligations | W remains budget-waiting with trigger; no plan/report rendering; budget window opens -> one admitted fresh wake -> evaluate; frozen delivery obeys current checks |
| Blocked dependency | Reminder note and worker text do not identify a durable obligation | W waits on attributed dependency D/review/credential condition; accepted event/reminder reoffers W once, model checks evidence, then reports or prepares allowed successor |

## Acceptance plan: one core, two user policies

These are future tests and runtime acceptance, not results obtained by this
design. Both fixtures use the same record/reconciliation core and different
policy bytes and model decisions:

| Policy | Completion/evidence | Report/successor |
| --- | --- | --- |
| A: development change | Independently reviewed merge and exact merged-main applicable CI, at immutable revisions | Concise source-thread report with links; one specifically authorized dependent task after confirmed report delivery |
| B: research answer | Two attributed primary sources, answer to the original question and explicit uncertainty; no merge requirement | Answer first in the allowed requester conversation, sources below; one specifically authorized comparison task after confirmed report delivery |

For each, a fake surface/agent harness sends exactly one initial request, seeds
an already-owned busy worker, and then supplies only worker/tool/system events
and simulated clock ticks. It records all operator input and fails on any later
operator nudge. Observe acceptance, plan/intent, matching dispatch receipt,
worker question/result, evidence, policy evaluation, one logical report and one
authorized successor. Inject restart before/after record, action acceptance,
delivery association, evaluation and outbox association. Assert stable work,
action/report/successor keys and counts at every boundary. No-succession policy
and absent authority produce zero successors.

The test matrix must also cover: repeated results/restarts; stale CAS writers;
cross-scope read/update attempts; busy target/question changed before cutoff;
owner reassignment and late evidence; exhausted sends/turns/tokens and loop guard;
no accepted trigger/no unsolicited model turn; cancellation/revocation on each
side of cutoff; uncertain delivery without replay; changed/pinned/missing policy;
blocked dependency/review/credential; unsupported research before artifacts;
unknown/withheld/incomplete evidence; digest/quiet/needed report suppression;
multi-part reports; destination revocation; migration preserving notes, nested
content and sessions; unknown schema refusal. Each regression must fail with
its relevant guard removed, not merely exercise a successful implementation.

Fake-agent tests prove deterministic mechanics, never model judgement, provider
capability or real unattended operation. A later approved disposable runtime
acceptance uses the same two policies, initial-request-only input ledger,
existing worker ownership, actual allowed evidence route, restart boundaries
and report/successor receipts. Save exact code/policy revisions, actor/source
provenance, timestamps, waits and attributable outcomes. Existing required
provider/authority reviews stay with their owners. Missing forge credentials,
unsupported research capability and unavailable mandatory review are separate
named waits; no provider switch, paid fallback or weakened isolation substitutes.
No production experiment is authorized by this document.

## Implementation breakdown and completion boundary

Proposed concerns are A: record/policy/evidence operations; B: work-bound wake
recovery and existing action correlation; C: terminal report/successor association
and the two-policy unattended acceptance. Child issues will be filed only after
review confirms these gaps and boundaries; each needs binary criteria, exact
source/hunk coordination, first runnable stage, dependencies and bounded effort.
Existing #1060, #1116 and #275 remain their owners' work with unchanged gates.

#1123 completes only after independent Codex design and separate authority review
have no unresolved findings, the design and linked breakdown merge, applicable
final-head and actual-main documentation CI succeed, and the issue closes.
The operator verifies and dispatches the next approved stage. Design completion
does not complete the feature or activate any runtime behavior.
