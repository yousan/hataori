# Deferred management send design

Status: independently approved for narrow implementation at design commit
`0a3a134cdaa6d5d317fd751f6d25c3b6cb5d420f`. Final code reviews and CI remain required.
Issue #1116 / PR #1120. The surviving-host handoff below is part of the narrow
implementation, not permission to weaken the existing uncertainty guards. Immediate `Send`, its busy result, answers and interrupts keep
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
time, frozen handoff identity/payload/result, state and reason. No credentials are stored. There is
at most one outstanding entry for a target; different instructions are not
silently combined. Terminal rows and keys are retained across restarts.

Queue states distinguish `proposed`, `queued`, `accepted` by the ordinary
input machinery, `handoff` for its frozen transport grant, confirmed
`delivered`, `cancelled`, `refused` and `failed`.
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
acceptance. Creating sessions are not eligible for this narrow feature. Initial enqueue
requires a running or idle local worker with a verified live owned host. It
refuses an already-asleep worker (use immediate send) and checks the transport
capability before any queue, proposal or budget artifact.

The first implementation supports only adapters that explicitly declare one
complete submitted input in a single emitted action, with stable input-ID
acknowledgement: the existing native Codex app-server path and the fake adapter.
Unknown adapters and Claude Code do not declare it. Claude Code deliberately
separates paste and Enter across actions; a batch for its paste alone would not
close the submission gap, and this change does not relax its editor guards.
Its immediate sends remain available. No CLI/provider execution or switch is
needed to establish this source/fixture contract.

Deferred inputs additionally carry a generic `agent.Input.IdleOnly` property,
retained for pending/first input, codec queues and Written reconstruction.
Supported codecs must hold or refuse it while another native turn is running,
never steer it into that turn. Codex excludes IdleOnly from both flushSteers
and text-based Written steer/userMessage acknowledgement. Normal immediate
steering and legacy Written matching stay unchanged.

For this opt-in path Codex must validate that the frozen emitted request is
exactly `turn/start`, with its actual request identity and original native
thread. The frozen write is supplied to the reconstructed codec as opaque
written-command evidence; it restores the original request association and
reserves later request numbers above it. Only that exact request's successful
turn/start response and its identified native turn can confirm the input.
Neither arbitrary turn/started, same-text userMessage, an unrelated request's
response nor a host write receipt may confirm it. Retained-response ordering
before/after turn notifications and two codec reconstructions need fixture
proof. A missing/invalid association fails closed; no text-match fallback.
The generic runner/core do not parse provider-specific RPC messages; that
validation/correlation lives only in the adapter.


Remote, tmux-held, taken-in, relay and old/non-advertising hosts are explicitly
unsupported for deferred acceptance. Their existing input/readiness/foreground
checks and remote journal remain untouched. Refusal is specific and creates
no artifact; there is no raw-input fallback or forced worker restart.
The source must be an existing live supervisor conversation in the same scope.

A small deterministic daemon dispatcher scans durable rows on startup and
when state changes, with a bounded retry tick for transient store/connection
errors. It calls no model and is independent of the waker's gates. It never
starts a new thread, replaces a session, changes provider or calls interrupt.
Idle or asleep are eligible only with the same recorded lineage, no open
turn, no pending input/placement/held action and no outstanding question.
Background activity does not authorize an interrupt or extra turn.

A queued worker that falls asleep may resume only its same session and
recorded agent conversation. It is not a new worker/session. That resume must
verify the eventual live owned host capability and random run before freezing
input. A cannot-resume failure is terminal for this instruction and must not
trigger the usual replacement-session path. Before any frozen handoff, this
same-conversation resume may bind its new physical run; after a handoff is
frozen its run can never change.

The dispatcher rechecks the enabled scope, current person mapping and send
rights, current supervisor source lifetime, allowed repository, target kind,
session/conversation/workspace/machine identity and the question watermark.
Any question opened after acceptance but before the frozen handoff invalidates
the instruction even when already answered or withdrawn. After that cutoff,
reconciliation is of the original already-granted attempt, not a new send. It is never repurposed as an answer. This
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

Before the frozen handoff, current watch revokes undelivered queue authority. A former act grant
changed to propose without human approval fails closed; no synthetic approval
or later model turn is invented. A human-approved entry can remain eligible
under propose, with current sender/decider authority checked. Changing propose
to act does not auto-approve a waiting proposal.

Cancellation and the frozen host handoff serialize on the durable receipt.
Cancel while proposed, queued or accepted-but-not-frozen is terminal, records
no physical input and invalidates a pending proposal. Existing accepted input
may still need its normal not-delivered cleanup; revalidation and the final
freeze guard forbid any bytes after cancellation. Repeated cancellation
returns the same receipt. At/after the atomic handoff checkpoint the operation
reports too late, never success: transport may already have begun. Inspection
continues to expose the actual input outcome. Cancellation interrupts nothing.

The authority/lineage/question check at that checkpoint is the send's
linearization point. A later revocation cannot retract bytes already granted
or written. Reconnect first reads retained evidence. If the same host proves
that the frozen key is absent (no attempt began), current authority and the
question watermark must pass again before beginning it; a revoked grant is
then refused. A pending/completed receiver receipt is reconciled as the
original attempt, never treated as permission for a new send.

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

## Surviving-host handoff and its failure boundary

The existing raw path cannot prove this guarantee: runner marks written
before host I/O, and its `input` command has no key. Preserve that ordinary
path and its explicit not-delivered behavior. The opt-in path adds a bounded
idempotent batch to the already-running owned host, rather than replaying raw
keys or redefining a written marker.

A capability handshake on the actual input connection returns support for
`input-batch-v1` and the live random host run for both pipes and PTY. PID,
pathname and caller-reserved Spec.Identity are not run evidence. The batch
operation repeats the exact run check before writing. Old/relay hosts refuse.

At the actual emitted-actions boundary, require exactly the deferred input's
stable ID, a complete keys-or-pipe-write action, and no mixed inputs/replies.
The adapter's readiness, instruction injection, retained-read integrity and
catch-up gates must already permit that emission. Convert keys with the
existing encoding and preserve write chunk boundaries/order. Atomically
freeze those exact bytes, input ID, queue ID, random run, stable command key
and digest together with the written marker, after the final policy/question
check. No deferred marker may commit without that recoverable payload. This
also covers codec-held and resumed first-input emissions. Recovery never
reruns hooks, changes rendering, generates another key or bypasses readiness.

The host serializes identity/digest lookup and the entire batch under its input
lock. It reserves the key before the first write and stores a terminal result
before replying, even if the client disconnected. Concurrent repeats wait for
or return that original result; a mismatched digest/run refuses without I/O.
Stop at the first error; a short write is an error. Partial/uncertain writes
are terminal and never replayed. A pure receipt lookup distinguishes absent,
in-flight and terminal keys. The host retains every reserved key for its
entire run, with no eviction; at 128 keys or above a 128-KiB batch it refuses
new keys before I/O. Repeated known keys remain inspectable at capacity.
Queue text is bounded to 16 KiB. These limits bound memory without making a
forgotten key executable again. No forced restart is part of backpressure.

Reconnect preserves warmIO, committed retained reads, matching input-ID
correlation and catch-up ordering. It reconciles frozen commands before any
codec timeout or later pending input can act. A pre-request crash executes
only the exact frozen command on the same surviving host; a lost reply gets
its cached result. A daemon death during the host handler does not stop its
reserved operation. The matching agent input acknowledgement, not the host's
write receipt, records delivered in the same transaction as normal input
acknowledgement/cleanup. The queue retains the input mapping after pending
rows clear. Receiver completion time is distinct from freeze time: a command
that never reached the host must not expire merely from the old pre-write
marker, and a genuinely completed old write must not gain a renewed timeout
on every reconnect. Failure to establish a receipt/run is a hold or explicit
uncertainty, never raw replay.

This proves one complete input batch across daemon-only restarts when its
original receiver survives. Host death/replacement, physical partial write,
unsupported transport, lost session/conversation or unavailable proof has a
durable explicit refusal/failure/uncertainty, with no replay into another
host or replacement agent. No filesystem transaction can atomically commit
arbitrary PTY/pipe effects with the death of that receiver. This different
failure domain is not reported as delivered or silently retried. A matching
input acknowledgement proves input receipt, not model obedience.

## Integration and proof plan

Use new focused helpers/files for queue state and policy. Bounded existing
insertions are: core guarded-send acceptance, proposal approval branch and
transaction helper; session input acknowledgement recording; existing runner
initial/pending validation entry points; supervisor caller/API/budget/tool
bindings; daemon dispatcher lifecycle; control/wire operation registration and
additive proposal metadata; chat proposal wording; an optional adapter
atomic-input declaration; a private owned-host capability/keyed-batch helper
and opt-in runner freeze/reconciliation hook. No generic raw or remote retry
semantics change. Preserve independent
attachment, caller-mark, asynchronous-question, shared-file and remote-readiness
hunks. Coordinate actual overlaps with their owners, not entire paths.

The additive operation/optional event field needs a new protocol minor after
current 3.47, full docs and new golden directory/checksum; earlier versions are
immutable. A forward-only table migration appends after current main and any
concurrently landed owner. Appendix D and supervisor instructions document the
new contract. No live routing, production configuration or provider is needed.

Focused fixture regressions must cover immediate busy unchanged, durable
queue/idempotent retry, busy to idle/asleep to one matching input, restart
before/after queue commit, input acceptance, atomic freeze/mark, host
reservation/write, lost reply and acknowledgement commit, including two
consecutive daemon reconstructions and actual controlled-writer byte counts.
Prove a late native turn while the codec holds an IdleOnly input: no steer,
no same-text false ACK, and one turn/start only after eligibility returns.
Repeat identical text in another native turn and reconstruct twice, retaining
exact frozen request correlation and existing immediate steering behavior.
Exercise host capacity/digest/run/partial-write failures, unsupported adapter/
holder/remote preflight and same-session asleep resume without replacement. Test open/new/resolved questions,
source/target replacement/closure, scope/person/repository/machine revocation,
watch/propose/act changes, real proposal approval/refusal/cancel races, hourly
and daily-budget exhaustion and restart accounting. Negative guard removals
must fail their intended assertions. Use record-only core fixtures and pure
runner/host fixtures locally; terminal E2E and full suites stay in CI.

Final code requires separate independent implementation and authority reviews,
applicable full ready-head checks and the actual merge-main push checks, merge
and issue closure. Every existing explicit Claude gate remains with its owner.
