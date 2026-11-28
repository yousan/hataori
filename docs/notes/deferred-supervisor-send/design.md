# Deferred management send design

Status: proposed; no implementation approval yet. Issue #1116 / PR #1120.
The authority review must resolve the transport question below before code is
implemented. Immediate `Send`, its busy result, answers and interrupts keep
their existing behavior.

## Explicit operation and durable identity

Add one additive control operation, `DeferredSend`, with queue, inspect, find
by key and cancel actions. Queue takes an existing target number, nonempty
text and a required caller-chosen retry key. It returns a durable number,
state, target and, when applicable, proposal number. Inspect/cancel take the
durable number; find supports recovery after a lost queue reply. MCP exposes
separate defer, inspect and cancel tools, rather than changing immediate send.
The model must retain the retry key before requesting the queue and reuse it
after uncertainty. Reusing a key with different target/text is refused.

The daemon grants enqueue capability only to its authenticated supervisor
caller. A serialized caller is provenance, never a grant. Core rechecks its
send permission, current person, scope and source conversation. Reads obey
scope and text visibility; a supervisor sees only its own accepted entries.
The owner may inspect a visible scope's entries. Cancellation reduces an
existing grant and remains available to an issuer in watch mode.

An append-only receipt table retains identity, source thread/session/person,
scope, original target thread/session/conversation/workspace, repository and
machine, text, retry key, request watermark, approval provenance, acceptance
time, handoff identity, state and reason. No credentials are stored. There is
at most one outstanding entry for a target; different instructions are not
silently combined. Terminal rows and keys are retained across restarts.

Queue states distinguish `proposed`, `queued`, `accepted` by the ordinary
input machinery, confirmed `delivered`, `cancelled`, `refused` and `failed`.
Neither queued nor accepted claims that the agent read the instruction.
Confirmed delivery requires the matching agent input/turn acknowledgement.
The result exposes these states and durable refusal/failure reasons. Notes
must retain the receipt while proposed/queued, and can replace the retry
order with that receipt after terminal acceptance; they cannot say delivered
without its acknowledgement. A failure needs a new explicit decision, never
an automatic fresh key or replacement worker.

## Same target and eligibility

Queue binds the actual current ordinary session, its recorded agent
conversation and workspace identity. Unknown identity, confined targets,
closed/removed/failed targets and any open request are refused before queue
acceptance. Creating sessions are not eligible for this narrow feature.
The source must be an existing live supervisor conversation in the same scope.

A small deterministic daemon dispatcher scans durable rows on startup and
when state changes, with a bounded retry tick for transient store/connection
errors. It calls no model and is independent of the waker's gates. It never
starts a new thread, replaces a session, changes provider or calls interrupt.
Idle or asleep are eligible only with the same recorded lineage, no open
turn, no pending input/placement/held action and no outstanding question.
Background activity does not authorize an interrupt or extra turn.

The dispatcher rechecks the enabled scope, current person mapping and send
rights, current supervisor source lifetime, allowed repository, target kind,
session/conversation/workspace/machine identity and the question watermark.
Any question opened after acceptance invalidates the instruction even when
already answered or withdrawn. It is never repurposed as an answer. This
includes a nonblocking question if that separate feature is later integrated.
Target/source closure, replacement or authority/repository revocation has a
durable terminal refusal. A temporary busy/unavailable/catching-up target
stays queued and never bypasses existing runner readiness checks.

The final idle/identity/question check and the unique ordinary input receipt
commit together. Ordinary prompt hooks and the supervisor mark are retained;
no raw unmarked injection is added. The guarded send cannot join a new running
turn if one starts during hooks. The same transaction changes queued to
accepted and records the pending input or resume-first-input identity.
Recovery checks that receipt before repeating dispatch. It never emits a
second accepted input for the same durable number.

Unwritten input is checked again in the existing runner initial/pending
revalidation paths, for local as well as remote sessions. Current authority
comes from trusted daemon configuration, not the saved caller. Question and
lineage checks remain attached to that input through reconnect. Written or
uncertain input cannot be silently retargeted or reauthorized.

## Autonomy, approval and cancellation

Under watch, queue is refused. Under act, current authority can accept it.
Under propose, acceptance atomically stores the exact target/text/identity and
an ordinary human proposal, explicitly labelled as deferred; it delivers
nothing until an eligible different person approves. Existing proposal
decider checks remain. An optional deferred identity on proposal records and
events distinguishes approval of queueing from immediate delivery. Approval
cannot change the frozen payload, target, question binding or retry key.

Current watch always revokes undelivered queue authority. A former act grant
changed to propose without human approval fails closed; no synthetic approval
or later model turn is invented. A human-approved entry can remain eligible
under propose, with current sender/decider authority checked. Changing propose
to act does not auto-approve a waiting proposal.

Cancellation and input acceptance serialize on the durable receipt. Cancel
before acceptance is terminal, records no input and invalidates a pending
proposal. Repeated cancellation returns the same receipt. After acceptance,
the operation reports that it is too late rather than claiming that bytes
already handed over were cancelled. Inspection continues to expose the
accepted input's actual outcome. No cancellation interrupts an agent.

## Budget decision

Reserve exactly one send in the current hour at initial queue/proposal
acceptance, as current proposals are charged when made. Queue refusal costs
none. Repeating an accepted key only reads its receipt, even at the limit;
it cannot consume another credit. Proposed, cancelled and later-refused
entries retain their original charge. A new key is a new attempt and must
pass the current send limit. One pending target and a bounded payload prevent
an unlimited queue of distinct instructions.

The same scope budget lock serializes immediate sends, starts, reminders and
queue acceptance. Durable counting includes each queue once, including a
proposal-backed row, and excludes that row's proposal and eventual input
from double counting. Restart reconstructs the count from durable records.
Dispatch and human approval consume the reserved credit, not a second credit
at delivery. Rolling hours and a lowered limit do not mint/refund credits or
expand an accepted payload. Immediate send's accounting is unchanged.

Daily token and hourly model-turn limits still hold model wakes. They do not
drop or hold an already-accepted fixed instruction: dispatch uses no model
turn or tokens. A full hourly send budget refuses new queue acceptance.
Inspection/cancellation are free. No operation changes configured budgets.

## Unresolved exact-delivery boundary

Source inspection identifies a material existing limitation. Runner marks
input written before host I/O (`runner.act` / `markWritten`); reconnect passes
written input back to the codec without writing it again. The local host's
`input` command carries bytes but no durable idempotency key. A daemon crash
after the mark and before the host write can therefore leave no agent input;
blindly replaying after a lost write acknowledgement can duplicate one.
The existing safe outcome for uncertainty is an explicit not-delivered result,
not an unconditional exactly-once physical write guarantee.

The queue can atomically guarantee one logical ordinary-input acceptance and
never duplicate dispatch, but that alone does not establish #1116's stated
delivery across every restart boundary. The independent review must decide
whether the acceptance requires a narrowly scoped idempotent handoff to the
already-resident host. A candidate repair would bind the rendered input and
its retry identity to that host run, let the surviving host deduplicate a
repeated command after a daemon restart, and refuse if the host/run or target
changes. It must retain all readiness and input-evidence gates, including the
remote command journal, and must not replay ambiguous writes to a new agent.
This is not approved by this document. No claim of exactly-once delivery or
implementation approval is made while this boundary is unresolved.

## Integration and proof plan

Use new focused helpers/files for queue state and policy. Bounded existing
insertions are: core guarded-send acceptance, proposal approval branch and
transaction helper; session input acknowledgement recording; existing runner
initial/pending validation entry points; supervisor caller/API/budget/tool
bindings; daemon dispatcher lifecycle; control/wire operation registration and
additive proposal metadata; chat proposal wording. Preserve independent
attachment, caller-mark, asynchronous-question, shared-file and remote-readiness
hunks. Coordinate actual overlaps with their owners, not entire paths.

The additive operation/optional event field needs a new protocol minor after
current 3.47, full docs and new golden directory/checksum; earlier versions are
immutable. A forward-only table migration appends after current main and any
concurrently landed owner. Appendix D and supervisor instructions document the
new contract. No live routing, production configuration or provider is needed.

Focused fixture regressions must cover immediate busy unchanged, durable
queue/idempotent retry, busy to idle/asleep to one matching input, restart
before/after queue commit, input acceptance, host write and acknowledgement,
and any explicit uncertainty failure. Test open/new/resolved questions,
source/target replacement/closure, scope/person/repository/machine revocation,
watch/propose/act changes, real proposal approval/refusal/cancel races, hourly
and daily-budget exhaustion and restart accounting. Negative guard removals
must fail their intended assertions. Use record-only core fixtures and pure
runner/host fixtures locally; terminal E2E and full suites stay in CI.

Final code requires separate independent implementation and authority reviews,
applicable full ready-head checks and the actual merge-main push checks, merge
and issue closure. Every existing explicit Claude gate remains with its owner.
