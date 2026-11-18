# The control protocol

Programs outside the daemon (the `hataori` command, a supervisor, a dashboard, plugins) reach the control API over the instance's Unix socket. This document describes every message, operation, type and event on that socket. `internal/wire/doc_test.go` fails when the code and this document disagree on a name, a field, a type or whether a field is required.

## Connection

- The socket is `<root>/state/control.sock`. When that path is too long for a Unix socket, the daemon listens on a short path in the user's runtime or temporary directory and writes that path into `<root>/state/control.sock.path`.
- Only the OS user that runs the daemon may connect. Every connection acts as the instance's owner.
- Each message is one JSON object on one line.
- Each side sends a `hello` first. The daemon reads the client's hello before it answers with its own. When the daemon refuses the client, its hello carries `error` and the daemon closes the connection.
- After the hellos, **either side may send requests**. A request has `op` set; a response has no `op`. Each side numbers its own requests, and a response carries the `id` of the request it answers. Several requests may be outstanding at once, and the answers may come in any order.
- The client sends the operations below. The daemon may send the client requests of its own; there are none in this version, and plugin hooks and scope owners in other processes will use them. A side that gets a request whose `op` it does not know answers with the error code `not_supported` and carries on.
- An `Events` request turns the connection into a stream: the daemon answers it once with an empty `response`, then sends one `response` with `event` set per event, until the client closes the connection. A client follows events on a connection of its own.

## Versions

The protocol version is `major.minor`, now **1.7**. Both hellos carry it.

- A client and a daemon whose **major** numbers differ refuse each other. The refusal names both versions, for example `protocol version mismatch: client speaks 2.0, daemon speaks 1.0`.
- With the same major number and different minor numbers they work together.

What a change does to the version:

| Change | Version |
|---|---|
| A new optional field in a message, a type or an event | minor |
| A new event kind | minor |
| A new operation, or a new request the daemon sends | minor |
| A new error code | minor |
| Removing or renaming an operation, a field, an event kind or an error code | major |
| Making an optional field required | major |
| Changing a field's type, or what a field or a value means | major |

So a reader must ignore what it does not know: a field it does not know, an event kind it does not know, a request it does not know (answered `not_supported`). The project's own client does: it skips an event of a kind it does not know.

Events are stored, and a reader reads them from a position, possibly long after they were written. Their schema is part of this version: an event kind or a field changes under the same rules.

**Golden files.** `internal/wire/testdata/protocol/<version>/` holds one encoded example of every message, of every operation's request and response, and of every event kind, exactly as they go over the socket. `internal/wire/testdata/protocol/SUMS` holds the checksum of each version's directory. A test fails when an encoding differs from its golden file, and when a version's golden files differ from the checksum recorded for that version. To change an encoding: change the version by the rules above, then run `go test ./internal/wire -run Golden -update`, which writes the new version's directory and its line in `SUMS`. The directories of earlier versions stay as they are.

**Text is optional.** Every field that holds text written by a person or an agent (prompts, answers, questions, terminal output) is optional, so it can be left out for a reader that may not read it.

## Messages

### Message `hello`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `version` | string | required | the protocol version the sender speaks |
| `error` | string | optional | the daemon refuses the connection, and why |

### Message `request`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the sender's number for this request |
| `op` | string | required | the operation |
| `args` | any | required | the operation's arguments, an object; `null` for none |

### Message `response`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the `id` of the request this answers |
| `result` | any | optional | the operation's result; absent when it has none or failed |
| `error` | `WireError` | optional | the operation failed |
| `event` | `Event` | optional | one event of an `Events` stream |

### Type `WireError`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `code` | string | required | one of the error codes below |
| `message` | string | required | a sentence for a person |

### Error codes

| Code | Type | - | Meaning |
|---|---|---|---|
| `forbidden` | error code | - | the caller may not make this call in this scope |
| `disabled` | error code | - | the scope is not enabled |
| `busy` | error code | - | a turn is running and the call said to refuse |
| `not_found` | error code | - | no such thread or request |
| `refused` | error code | - | the session's state does not allow the call |
| `not_supported` | error code | - | the receiver does not know the operation |
| `error` | error code | - | any other failure |

## Operations

The client's requests. Every operation's arguments are one object.

### Operation `Threads`

The threads the caller may see.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `filter` | `Filter` | required | which threads |

Result: [`ThreadInfo`]

### Operation `Thread`

One thread's state, open requests, last output and last events.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | `ThreadID` | required | the thread |
| `recent` | integer | required | how many bytes of its last output to return |
| `events` | integer | optional | how many of its last events to return, at most 100; absent or 0 for none; since 1.5 |

Result: `ThreadDetail`

### Operation `Start`

Starts a thread in a scope. The same `key` starts nothing twice and returns the thread the first call started.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `key` | string | required | makes the call safe to repeat |
| `scope` | string | required | the scope; for chat, a channel |
| `thread` | string | required | the thread's key, or empty to let the scope's owner make one |
| `repo` | string | required | the repository the workspace is cloned from |
| `input` | `Input` | required | the first input |
| `model` | string | required | the model; empty for the scope's default |
| `effort` | string | required | the effort; empty for the scope's default |

Result: `ThreadID`

### Operation `Send`

Gives input to a thread. A thread with an open question takes it as the answer; an asleep thread is woken with it.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | `ThreadID` | required | the thread |
| `key` | string | required | makes the call safe to repeat: a key accepted before is ignored |
| `input` | `Input` | required | the input |
| `busy` | integer | required | when a turn is running: 0 refuse (`busy`), 1 deliver into the turn, 2 interrupt it and then send |

Result: none

### Operation `Answer`

Answers an open question or approval.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `request` | string | required | the request's `id`, as in `Request` and the `request` event |
| `answer` | `Answer` | required | the answer |

Result: none

### Operation `Stop`

Interrupts the thread's running turn. Nothing happens when no turn runs.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | `ThreadID` | required | the thread |

Result: none

### Operation `Events`

The events after a position, then new ones as they are written. Once the events stored when the call was made are sent, the daemon sends one event of kind `caught_up`. A reader that only wants the stored events stops there.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `filter` | `Filter` | required | which threads' events |
| `after` | integer | required | the position to start after; 0 for all |

Result: `Event`

### Operation `Workspace`

What the thread's workspace holds that may exist nowhere else, as numbers; since 1.4. It changes nothing in the workspace, and it does not ask the remote: commits are compared with the remote-tracking refs as last fetched. A workspace the daemon cannot read, a partial clone (which git would complete from the remote), a configured upstream that cannot be read, or a git failure, is an error, never zeros.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | `ThreadID` | required | the thread |

Result: `WorkspaceState`

### Operation `SignInLink`

Makes a link that signs the owner in to the instance's dashboard. The link works once and only for a few minutes. Only the owner's connection on this socket may call it; a client of the control API inside the daemon, such as a chat front end or the dashboard itself, is refused with `forbidden`. An instance without a dashboard answers with `error`. Since 1.7.

| Field | Type | Presence | Meaning |
|---|---|---|---|

Result: string

## Types

### Type `ThreadID`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `scope` | string | required | opaque; for chat, a channel |
| `key` | string | required | opaque; for chat, the chat thread |

### Type `Filter`

A field left empty matches everything the caller may see.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `scope` | string | required | only this scope |
| `thread` | `ThreadID` | required | only this thread; a key without a scope matches it in every scope |

### Type `Input`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `text` | string | required | what is given to the agent |

### Type `Answer`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `text` | string | required | a free-text answer to a question |
| `choices` | [string] | required | the tokens of the chosen options, for a question with choices; may be `null` |
| `allow` | boolean | required | the decision, for an approval |

### Type `ThreadInfo`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | `ThreadID` | required | the thread |
| `number` | integer | required | a short number, unique in the instance |
| `title` | string | required | from the first line of the first prompt |
| `repo` | string | required | the repository of its workspace |
| `state` | string | required | `creating`, `idle`, `running`, `waiting`, `asleep`, `closed`, `removed` or `failed` |
| `started_by` | string | required | the principal that started it |
| `created` | time | required | when it was started |
| `updated` | time | required | when its state last changed |
| `agent` | string | optional | the registered name of the agent of its current session; since 1.5 |
| `model` | string | optional | the model of its current session: as the agent reports it, and as asked for (or the scope's default) until it does; since 1.5 |
| `effort` | string | optional | the effort, in the same way as `model`; since 1.5 |
| `turns` | integer | optional | the turns started in the thread, in all its sessions; absent for none; since 1.5 |
| `last` | string | optional | the last line that is not blank of its last `text` event that has one, trimmed, at most 200 bytes; text; since 1.5 |
| `background` | integer | optional | the `background` of the thread's last `turn_ended` event; absent for 0 or before its first turn ends; since 1.5 |

### Type `ThreadDetail`

The fields of `ThreadInfo`, and:

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | `ThreadID` | required | as in `ThreadInfo` |
| `number` | integer | required | as in `ThreadInfo` |
| `title` | string | required | as in `ThreadInfo` |
| `repo` | string | required | as in `ThreadInfo` |
| `state` | string | required | as in `ThreadInfo` |
| `started_by` | string | required | as in `ThreadInfo` |
| `created` | time | required | as in `ThreadInfo` |
| `updated` | time | required | as in `ThreadInfo` |
| `agent` | string | optional | as in `ThreadInfo` |
| `model` | string | optional | as in `ThreadInfo` |
| `effort` | string | optional | as in `ThreadInfo` |
| `turns` | integer | optional | as in `ThreadInfo` |
| `last` | string | optional | as in `ThreadInfo` |
| `background` | integer | optional | as in `ThreadInfo` |
| `requests` | [`Request`] | required | the open questions and approvals; may be `null` |
| `recent` | string | required | the end of its last output, at most the bytes asked for |
| `events` | [`Event`] | optional | its last events, as many as `events` asked for, newest last, each as `Events` delivers it; since 1.5 |

### Type `Request`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | string | required | what `Answer` names |
| `kind` | integer | required | 0 a question, 1 an approval |
| `text` | string | required | the question |
| `choices` | [`Choice`] | required | its options; may be `null` |

### Type `Choice`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `label` | string | required | what is shown |
| `token` | string | required | what `Answer.choices` carries back |

### Type `WorkspaceState`

Since 1.4.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `branch` | string | required | the checked-out branch; empty when no branch is checked out |
| `base` | string | required | the commit the branch started from: where it meets the remote's default branch, or else its upstream; the checked-out commit when there is neither |
| `added` | integer | required | lines added against `base`: commits, staged and unstaged changes of tracked files |
| `removed` | integer | required | lines removed, counted as `added` |
| `uncommitted` | integer | required | files modified, staged or untracked, each once; ignored files are not counted, and untracked files add no lines |
| `unpushed` | integer | required | commits reachable from the checked-out commit or a local branch and from no remote-tracking ref |
| `fetched` | time | optional | when the remote-tracking refs were last updated, by a fetch, a push or the clone; absent when the repository does not say |

### Type `Event`

One entry of the event log. `hataori events` prints each event as this object, one per line.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `pos` | integer | required | its position: unique and increasing in the instance; read on after the last one seen |
| `thread` | `ThreadID` | required | the thread it belongs to |
| `time` | time | required | when it was written |
| `kind` | string | required | one of the kinds below |
| `data` | any | optional | an object with the fields of its kind |

`time` is a string in RFC 3339 form with fractional seconds, such as `2026-01-02T03:04:05.678Z`.

## Events

The `data` of each event kind. Integers are the instance's own numbers for threads, sessions, turns and login outages.

### Event `thread_created`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `thread` | integer | required | the thread's number |
| `session` | integer | required | its first session |
| `repo` | string | required | the repository |
| `principal` | string | required | the person who started it; before 1.6, the id the caller gave |
| `from` | string | optional | the `<client>:<id>` the input came from, such as a chat message's author; absent for a caller with no such id, such as the `hataori` command; since 1.6 |
| `model` | string | required | the model asked for, or the scope's default |
| `effort` | string | required | the effort asked for, or the scope's default |

### Event `state`

The session's state changed.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `from` | string | required | the state before |
| `to` | string | required | the state now |
| `reason` | string | optional | why, such as `idle`, `interrupted`, `crashed`, `blocked at start`, `cannot resume`, `wake` |
| `detail` | string | optional | text for a person, such as the end of the terminal output of a session blocked at start |

### Event `turn_started`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `turn` | integer | required | the turn |
| `by` | string | required | `caller`, `agent` (by itself, after a background task) or `terminal` (a person at the agent's terminal) |
| `principal` | string | optional | the caller's person, for `by: caller` |
| `from` | string | optional | the `<client>:<id>` the input that started it came from, for `by: caller`, as in `thread_created`; since 1.6 |
| `key` | string | optional | the key of the call that started it, for `by: caller` (for chat, the message's id); since 1.1 |

### Event `turn_ended`

Exactly one per turn.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `turn` | integer | required | the turn |
| `by` | string | required | who started it, as in `turn_started` |
| `principal` | string | optional | as in `turn_started` |
| `from` | string | optional | as in `turn_started`; since 1.6 |
| `outcome` | string | required | `completion`, `blocked` (on a question) or `failure` |
| `reason` | string | optional | for a failure: `limit`, `login`, `api_error`, `crashed`, `interrupted`, `not delivered`, ... |
| `background` | integer | required | background tasks still running; a reader that wants "really finished" waits for 0 |

### Event `input`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `turn` | integer | required | the turn it went to |
| `text` | string | optional | the input |

### Event `answer`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `turn` | integer | required | the turn the answer starts |
| `text` | string | optional | the answer |

### Event `text`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `text` | string | optional | text the agent wrote |
| `final` | boolean | required | the turn's final answer |

### Event `tool_started`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `id` | string | required | pairs it with its `tool_ended` |
| `tool` | string | required | the tool's name |
| `parent` | string | optional | the `id` of the subagent the tool runs in; absent for the agent's own tools; since 1.2 |

### Event `tool_ended`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `id` | string | required | as in `tool_started` |
| `tool` | string | required | the tool's name |

### Event `subagent_started`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `id` | string | required | pairs it with its `subagent_ended` |
| `name` | string | optional | the subagent's name or type; since 1.2 |

### Event `subagent_ended`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `id` | string | required | as in `subagent_started` |

### Event `request`

The agent asks a question or an approval; the turn ends `blocked`.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `request` | string | required | what `Answer` names |
| `approval` | boolean | required | an approval rather than a question |
| `text` | string | optional | the question |
| `choices` | array | optional | its options, strings |

### Event `request_withdrawn`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `request` | string | required | the request that can no longer be answered |

### Event `file_out`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `file` | string | required | the path of a file the agent hands over |
| `more` | array | optional | the paths of further files handed over with it, in order, strings; since 1.3 |
| `caption` | string | optional | the text that goes with the files; since 1.3 |

### Event `usage`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |

### Event `login_lost`

A turn failed with reason `login`, and a login outage of the agent opened.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `agent` | string | required | the agent's registered name |
| `outage` | integer | required | the outage |
| `thread` | integer | required | the thread whose turn failed |

### Event `login_restored`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `agent` | string | required | the agent's registered name |
| `outage` | integer | required | the outage that closed |
| `threads` | array | required | the threads it stopped, as `ThreadID` objects |
| `scopes` | array | required | their scopes; a caller that may not see all of them does not get the event |

### Event `session_replaced`

The agent no longer had the conversation; a new session started in the same workspace.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session that could not be resumed |
| `new_session` | integer | required | the new session |
| `reason` | string | required | `cannot resume` |

### Event `caught_up`

Not stored and belongs to no thread: the mark in an `Events` stream after the events stored when the call was made. It has no data, and its `pos` is the last position sent (or `after`).

| Field | Type | Presence | Meaning |
|---|---|---|---|
