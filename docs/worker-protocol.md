# Worker protocol

Status: protocol foundation for #957, part of #956. This document specifies
worker protocol **1.0**. It is independent of the control protocol described in
`protocol.md`; a control socket and an unversioned host socket are never worker
endpoints. The resident, SSH transport and runner integration ship in later
issues. There is no remotely usable worker in this change.

## Compatibility and golden files

`internal/workerproto` defines the encoding and validates messages before a
dispatcher may look up a host, open a resource or execute anything. Its examples
live under `internal/workerproto/testdata/protocol/<major.minor>/`. Every request,
reply, stream kind and refusal has a golden example. The `SUMS` manifest freezes
each version's complete example set. Never regenerate a released version or its
checksum. Change `workerproto.Version`, add a new directory and append its sum.

A major change breaks an existing promise, field meaning, required field or
authority boundary. A minor change adds optional fields, operations or
capabilities. Peers with different majors refuse before any effects, naming
both versions. Same-major peers use the lower minor and the intersection of
their offered capabilities; an operation requires its negotiated capability.
Unknown operations and required capabilities refuse. Unknown optional fields
are ignored; they never become addresses, authority or commands. Neither peer
infers support just from a larger minor. Capability additions must preserve the
semantics of already negotiated operations.

## Trust boundary

The main initiates SSH to an already running, private resident socket. The
operator registers a main installation and a worker installation out of band.
The hello must match that existing binding on both sides. A hello cannot create
or replace a binding. There is no owner-by-default hello, person identity,
supervisor credential, arbitrary command, environment, socket address or file
path in this protocol. A worker is never an instance control socket. A
replacement installation cannot serve the old installation's sessions.

Connection messages name the binding. Every session operation, reply and
stream additionally names the exact thread, session and host run recorded by
the main and resident. The main reserves a fresh run identity before starting
a host; a PID is diagnostic data only and never authority. The receiver checks
all identities before resolving a handle or touching a resource. Inventory may
introduce only records durably owned by the registered main on this worker;
the main reconciles these with its own records before readiness. Unexpected
threads are quarantined, never adopted into another thread. A connection has
no implicit current session.

The root layout is **not a sandbox**. Compromising the main gives an attacker
the reach of its configured SSH credentials and the worker operations available
to that main. Compromising the worker OS user exposes that user's workspaces,
agent/repository credentials and processes. A malicious worker can forge output
for its own sessions. Main-side scope checks remain necessary; worker output
never authorizes another session or promotes a caller. No bot, forge, person or
supervisor token is copied to a worker. SSH agent forwarding is disabled.

## Transport and limits

Messages are UTF-8 JSON objects, one per line, on a private duplex connection.
Binary data uses JSON base64. A frame, including its newline, is at most 1 MiB;
one byte chunk is at most 64 KiB. Oversize, duplicate JSON keys, trailing JSON,
malformed versions and invalid identities refuse before dispatch. Opaque IDs
are 1–128 ASCII letters, digits, `_` or `-`; they cannot be paths or addresses.
Requests and replies carry a nonempty request ID. Stream frames carry the ID
of their opening request. No request ID conveys authority.

The hello exchanges `version`, `binding` and `capabilities`. Successful
negotiation returns the negotiated minor and capabilities with the same
binding. Refusals are bounded structured codes with a safe explanation; they
must never echo credentials, login material or untrusted diagnostic payloads.
The message kinds, bodies, operation table and crash/replay rules below are
normative; golden files demonstrate their encodings.

`required` in a hello lists capabilities which must be in the intersection.
Unknown required capabilities refuse. Negotiation refuses a durable capability
without `receipts`. Each frame has `kind` (`request`, `reply`, `stream` or
`refusal`), `id`, `binding`, `op`, `body`, and for a session operation `target`.
`target` contains `thread` (a positive integer), `session` and `run` (opaque
IDs). Durable operations also require `key` on requests and replies. A refusal
uses the original envelope and a `{code,message}` body; it grants no effects.
Refusals of unknown operations or unnegotiated capabilities remain decodable:
the receiver matches the outstanding request ID, operation and target, without
requiring support for the operation it just had refused. `Decode` checks the
binding and target; the dispatcher correlates ID and operation with its own
outstanding request before consuming the refusal.
The `incompatible_major` handshake refusal instead names `local` and `peer`
versions and an `error` object. Only fixed, safe explanations may be returned.

## Operations and body schemas

The exported structs in `internal/workerproto/bodies.go` define the JSON field
names and types. All bodies are objects, including `Empty` (`{}`). The table is
exhaustive for 1.0; an unlisted operation refuses. **S** means a registered
session/run is required, **D** means durable acceptance and a receipt reply.

| Operation | Capability | Scope | Request body | Reply body |
|---|---|---|---|---|
| `health` | health | connection | Empty | Health |
| `inventory` | health | connection | Empty | Inventory |
| `probe` | health | connection | AgentConfig | AgentHealth |
| `prepare` | host | S,D | Prepare | Receipt |
| `start` | host | S,D | Start | Receipt |
| `stop` | host | S,D | Stop | Receipt |
| `input` | host | S,D | Input | Receipt |
| `answer` | host | S,D | Answer | Receipt |
| `receipt` | receipts | S | ReceiptQuery | Receipt |
| `attach` | terminal | S | Attachment | Attachment |
| `keys` | terminal | S | Keys | Empty |
| `resize` | terminal | S | Attachment | Empty |
| `detach` | terminal | S | Handle | Empty |
| `follow` | replay | S | Follow | Source |
| `read` | replay | S | Read | Bytes |
| `history_release` | replay | S,D | HistoryRelease | Receipt |
| `clone` | workspace | S,D | Clone | Receipt |
| `fetch` | workspace | S,D | Workspace | Receipt |
| `inspect` | workspace | S | Workspace | Inspection |
| `remove` | workspace | S,D | Workspace | Receipt |
| `restore` | workspace | S,D | Trash | Receipt |
| `acknowledge` | workspace | S,D | Trash | Receipt |
| `purge` | workspace | S,D | Trash | Receipt |
| `upload` | transfer | S,D | Upload | Receipt |
| `download` | transfer | S | Download | FileBytes |
| `login_start` | login | connection | Login | LoginAttempt |
| `login_input` | login | connection | LoginInput | LoginAttempt |
| `login_cancel` | login | connection | LoginAttempt | LoginAttempt |

`read` and `attach` may emit `stream` frames with `Bytes` bodies. The attachment
output has its own source/generation and ordered byte ranges; it is not codec
reconstruction history. `login_start` alone may emit `LoginStream` frames.
Other operations have no stream form. A refusal is legal for every operation.

The resident builds launches and probes using the named, locally configured
agent/configuration. `start` accepts only the own holder, a session-bound
workspace handle, model and optional adapter conversation ID. `prepare` accepts
instructions and uploaded policy handles, never a main path or environment.
Repository IDs in `clone` resolve only through operator-approved repository
registration. All workspace, trash, source, upload, download and attachment
handles are scoped to binding/thread/session/run, looked up in receiver-owned
records after envelope validation. Guessing an opaque ID does not grant access.
A remote dispatcher never turns an ID into a path by string concatenation.
Follow sources are adapter-reported IDs; the resident alone resolves an allowed
file, including adapter-owned files outside its root. Main never opens it.

Health names the current boot, readiness, per-agent/configuration installation
and login (`logged_in`, `logged_out`, `unknown`), and resource telemetry.
`installed:false,login:unknown` means missing. Telemetry is bounded to a maximum
60-second validity period. Main checks sample age, boot and limits; stale,
missing, future-dated or unknown observations never prove room for new work.
Readiness is false until identity, owned hosts, pending receipts and waiting
hooks are reconciled. Inventory reports `reserved`, `running`, `ended`, `gone`
or `unknown`, plus original turn/hook IDs. Only a complete inventory can prove
absence, and only for that boot. A timeout never proves a host is gone.
Main calls `CheckInventory` against its own durable target set before using
reports; discrepancies block readiness. Reboot recovery never assumes that a
local PID identifies a worker process.

`input` names the intended turn; `stop` names the original turn; `answer` names
both turn and request. The dispatcher checks these against the live owned host
after catch-up and immediately before effects. Ended turns, withdrawn/expired
hooks and replaced runs refuse as `stale`. A stop can report only that the
interrupt was written; the agent's report establishes the outcome. If an agent
cannot identify the interrupted turn, its existing interrupt race remains.

## Durable acceptance and recovery

A durable operation key is scoped by binding, target and operation. Before any
effect the resident records the key, the SHA-256 `request_hash` and the planned
object identity, flushes the receipt and required parent directories, then
performs the effect. `RequestHash` hashes UTF-8 JSON encoding of the object
`{binding,target,operation,body}` in that field order, recursively sorting body
object keys, removing insignificant whitespace and preserving number spellings.
It excludes transport `id` and the operation key; unknown optional body fields
participate. Same key/different hash refuses `key_conflict`. A retry with the
same key/hash returns the stored receipt and never starts a second operation.
After reply loss, query `receipt` using the original key and operation, then
`CheckReceipt` against the original durable request. A receiver must not accept
a peer-supplied expected hash as its own original record.

Receipt states are `pending` (accepted, effects not yet reconciled), `complete`
(effect durably known, with a Result), `unknown` (effect may have occurred), and
`refused` (confirmed no effect, with a Refusal). Pending/unknown have neither a
result nor a refusal. A pre-acceptance refusal creates no receipt and performs
nothing. A post-acceptance refusal is a durable `refused` receipt. Missing a
receipt is `not_found`, not evidence that a write did not happen. Receipts stay
available for at least the lifetime of their run and all outstanding recovery.
Expiry must never turn an old key into a new effect.

| Operation / crash window | Required reconciliation |
|---|---|
| Start: before acceptance | No host exists for this key; reserve the planned run before a new attempt. |
| Start: after intent, before or after launch, before complete receipt | Inspect the recorded owned-host identity/lock. Reconnect the same host or prove it never launched; uncertain ownership blocks readiness. Never launch a second host by PID guess. |
| Input or answer: intent recorded, write not started | Only a durable no-write proof permits a refused result. |
| Input or answer: write may have started, completion not recorded | Record `unknown`; preserve main's written-input marker. Match agent reports; otherwise end as `not delivered`. Never automatically write it again, including after reboot. |
| Stop: intent before interrupt / lost acknowledgment | Reconcile the original run and turn; refuse stale. Do not interrupt a later turn. |
| Clone or restore: before/after rename, before receipt | Persist planned directory and filesystem identity before rename; reconcile only that directory. Existing unrelated destinations refuse, never overwrite or adopt. |
| Remove: proof before rename | Re-run the full local proof immediately before rename under the worker's locks. A cached Inspection never authorizes removal. Unreachable/unknown proof refuses; destructive work is never queued while away. |
| Remove: rename before complete receipt / main acknowledgment | Reconcile recorded source identity and unique trash entry. Return the same receipt, never remove again. No purge before main's durable acknowledgment and retention expiry. |
| Purge: before/after deletion | Journal the unique acknowledged entry; partial progress stays pending/unknown until reconciled. Never resolve a fresh path from a retry. |
| Upload: any chunk acknowledgment lost | Reconcile offset, total digest and exact existing bytes under its session handle. Different content, another session's file or an existing unrelated destination refuses; retry never overwrites. |

The worker uses its own Git, lsof, locks, proof destinations and repository
credentials for workspace operations. An inspection is diagnostic only. Taken-in
local records and confined directories do not become remotely removable.
Cross-root or unclassifiable proof destinations refuse, as specified by #961.

Raw attachment keys are deliberately **not** durable operations: `keys` requires
a live attachment handle. On either-side disconnect, detach the attachment,
invalidate its handle and visibly end its stream. Do not replay keys, reconnect
an attachment automatically or infer acknowledgment means agent delivery.
Resize and detach are likewise limited to the current live attachment.

## Ordered byte reads and reconstruction

`Source` contains `handle`, `source` and `generation`, all bound to the session
run. A `Read` adds monotonically assigned `order`, byte `offset` and `limit`.
The main records the interleaved read order across all sources, not just one
offset per file. A `Bytes` result repeats these identities and order, returns
`offset`, `next`, `state` and base64 `data`. `CheckRange` compares it with the
recorded request before decoding output. `data` advances by exactly the byte
count; `empty` is a successful zero-byte read with unchanged offset. The distinct
states `unavailable`, `missing`, `unreadable`, `replaced`, `truncated` and
`history_lost` carry no bytes and do not advance. They never count as empty or
caught up. Changed generation, offset, source or order pauses reconstruction.

The main commits ordered ranges, source identities, offsets and resulting
events in one database transaction. Rebuilding a codec replays those exact
ranges in the same order, then reconciles new output before held operations.
The resident retains immutable bytes and generations for the whole run,
including replaced/shortened sources and already committed ranges. Main's
committed offset is not permission to discard history. `history_release`
requires `retired:true` and a durably retired run which can no longer need
reconstruction; the dispatcher verifies retirement, not just the boolean.
Storage exhaustion or missing history visibly pauses that session; never
invent empty output or claim catch-up. A successful empty read is an observation
at that moment, not proof the host ended.

## Transfers and ephemeral login

Uploads use main-chosen opaque handles and chunk offsets, total size/digest and
a final flag. A dispatcher applies the existing adapter/surface per-file and
per-session limits before accepting bytes. Downloads use session-issued handles
and bounded ranges; `FileBytes` repeats handle, digest, total size, offset and
finality. Main correlates all of these with its accepted transfer record and
verifies the complete digest. It never treats a worker filename as a main path.

Login is connection-scoped and keyed by worker installation, agent,
configuration and fresh `attempt` ID. Main checks owner authorization before
opening it; a worker accepts no chat identity. The resident runs login directly,
outside session hosts. Login bytes bypass receipts, replay history, session
logs, diagnostics and payload logging. `login_input` and the stream are ephemeral;
their acknowledgments are not receipts. A stream ends `complete`, `cancelled`
or `expired`. Main/resident restart, connection loss or timeout invalidates the
attempt and all input handles. Nothing is replayed; the owner starts a new
attempt. The codec only validates these frames; implementations must provide
separate in-memory routing and must never log the body on decode failure.

## Implementation status

This foundation does not launch processes, open sockets, read files or remove
directories. Future dispatchers must call the binding/target/capability guards
before doing so, and implement the durable rules below before advertising a
capability. Specification tests run on Linux and macOS without tmux, agent
credentials, a service manager or an operator's SSH configuration. Runtime
platform conformance, including macOS login-session access, remains #969.
