# The control protocol

Programs outside the daemon (the `hataori` command, a supervisor, a dashboard, plugins) reach the control API over the instance's Unix socket. This document describes every message, operation, type and event on that socket. `internal/wire/doc_test.go` fails when the code and this document disagree on a name, a field, a type or whether a field is required.

## Connection

- The socket is `<root>/state/control.sock`. When that path is too long for a Unix socket, the daemon listens on a short path in the user's runtime or temporary directory and writes that path into `<root>/state/control.sock.path`.
- Only the OS user that runs the daemon may connect. A connection whose client hello has no `token` acts as the instance's owner, except as below.
- A connection whose client hello has a `token` acts as the caller the daemon issued that token for, with the scopes and operations the daemon chose; nothing the client sends widens them. The daemon issues a token for each supervisor session, limited to that supervisor's scope (design, section 7.1). It may also ask for approvals shown in its own thread. Since 3.26 the scope's autonomy (`scopes[].supervisor.autonomy`, read when the daemon binds the token) limits it further: under `watch` its `Start`, `Send` and `AskInterrupt` are `forbidden`, with a message that names the mode; under `propose`, the default, its `Start` and `Send` are not made but stored as proposals that a person decides (`proposed`, below); under `act` they are made as before. A token the daemon does not know, or no longer knows, is refused: the daemon's hello carries `error`, it logs the refusal without the token, and it closes the connection. A client that streams events or terminal output presents the token on that connection too. Since 2.2; an older daemon ignores the field and makes the connection the owner's, so a client with a token checks that the daemon speaks 2.2 or later.
- A connection without a `token` from a process in the process group of a plugin the daemon started is refused: the daemon's hello carries `error`, and it logs the refusal with the plugin. A plugin connects with its token. This is a guard against a plugin that forgot its token, not a boundary between processes of one OS user. It changes no message, so it has no version of its own.
- Each message is one JSON object on one line.
- Each side sends a `hello` first. The daemon reads the client's hello before it answers with its own. When the daemon refuses the client, its hello carries `error` and the daemon closes the connection.
- After the hellos, **either side may send requests**. A request has `op` set; a response has no `op`. Each side numbers its own requests, and a response carries the `id` of the request it answers. Several requests may be outstanding at once, and the answers may come in any order.
- The client sends the operations below. The daemon may send the client requests of its own (Requests the daemon sends, below): since 3.16, to a plugin, its hooks and the notices that it started and is stopping. A side that gets a request whose `op` it does not know answers with the error code `not_supported` and carries on.
- An `Events` request turns the connection into a stream: the daemon answers it once with an empty `response`, then sends one `response` with `event` set per event, until the client closes the connection. A client follows events on a connection of its own.
- A `Terminal` request does the same with terminal output: an empty `response`, then one `response` with `terminal` set per chunk. Since 1.9.
- On an `Events` or a `Terminal` stream the daemon reads nothing the client sends: no request on that connection is answered or carried out, and nothing reaches a terminal.
- An `OperateHost` request turns the connection into a stream in both directions, for typing into one tmux pane on the host. Since 3.11. The daemon answers it once with a `response` whose `result` is a `HostOperation`. Then the client sends `HostKeys` requests, and may send one `End`, numbered as on any connection. The daemon carries out the batches in order and answers each when it is done; at most 64 may wait behind the one being sent, and one more is answered `error` and not sent. `End`, and a connection that closes, take effect at once, also while a batch is being sent: no further key of it is sent. When the operation ends, for whatever reason, the daemon sends one last `response` with the `id` of the `OperateHost` request and `ended` set to the reason, and closes the connection. A connection that closes ends the operation. No other request is carried out on that connection: one is answered `not_supported`.

## Versions

The protocol version is `major.minor`, now **3.32**. Both hellos carry it.

- A client and a daemon whose **major** numbers differ refuse each other. The refusal names both versions, for example `protocol version mismatch: client speaks 2.0, daemon speaks 3.0`.
- With the same major number and different minor numbers they work together.

**A 2.x client and a 3.0 daemon.** 3.0 names a thread by its number. Every operation that took a thread (`Thread`, `Send`, `Stop`, `Workspace`, `Terminal`, `AttachTerminal`, `CloseThread`, `RemoveThread`) takes the number as `id`, `Start` returns it, and every event carries it as `thread`, with the thread's `scope` beside it. The chat thread a thread is shown in is a link (`LinkRef`): `ThreadInfo` lists them in `links`, and `Filter.link` finds the thread linked to one conversation. `ThreadInfo.number` is gone, since `id` is the number; `login_restored` lists numbers in `threads`; `Start.thread` names the first link's conversation in the scope's own channel. The two refuse each other as below. Events written before 3.0 are read back with their thread's number and scope.

**A 1.x client and a 2.0 daemon.** 2.0 changed one thing: `title` in `ThreadInfo` and `ThreadDetail` became optional, and it is absent for a caller that may not read text. Because 1.x promised it, the two refuse each other. A 1.x client that connects gets the daemon's hello with an error, for example `{"version":"2.0","error":"wire: protocol version mismatch: client speaks 1.13, daemon speaks 2.0"}`, and the daemon closes the connection without reading a request. The project's 1.x client reports that refusal and makes no call. A client of 2.0 or later that may read text sees one other difference from 1.13: an empty title, from an empty first line, is left out instead of sent as `""`.

What a change does to the version:

| Change | Version |
|---|---|
| A new optional field in a message, a type or an event | minor |
| A new event kind | minor |
| A new operation, or a new request the daemon sends | minor |
| A new error code | minor |
| Removing or renaming an operation, a field, an event kind or an error code | major |
| Making an optional field required, or a required field optional | major |
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
| `token` | string | optional | the client's: a credential the daemon issued, which makes the connection that caller's instead of the owner's; never logged and never in an event; since 2.2 |

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
| `terminal` | `TerminalChunk` | optional | one chunk of a `Terminal` stream; since 1.9 |
| `ended` | string | optional | the last message of an `OperateHost` stream: why the operation ended, one of the reasons under `OperateHost`; since 3.11 |

### Type `WireError`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `code` | string | required | one of the error codes below |
| `message` | string | required | a sentence for a person |
| `proposal` | integer | optional | for `proposed`: the proposal's number; since 3.26 |

### Error codes

| Code | Type | - | Meaning |
|---|---|---|---|
| `forbidden` | error code | - | the caller may not make this call in this scope |
| `disabled` | error code | - | the scope is not enabled |
| `busy` | error code | - | a turn is running and the call said to refuse; for `OperateHost`, the pane is being operated already (since 3.11) |
| `not_found` | error code | - | no such thread or request; also a thread, a request or a conversation's thread of a scope that is not the caller's, which is answered as one that does not exist, with the same message |
| `closed` | error code | - | the thread is closed or removed: nothing was sent, and a message does not wake it; since 2.3 |
| `removal_refused` | error code | - | the workspace may hold work that exists nowhere else, so it was not removed; the message says why; since 2.3 |
| `linked_elsewhere` | error code | - | the conversation shows another thread; since 3.4 |
| `ended` | error code | - | the operation of a host pane has ended: nothing was sent; since 3.11 |
| `refused` | error code | - | the session's state does not allow the call |
| `stopped` | error code | - | a plugin's handler on an input hook point stopped the input, or failed where its policy blocks: nothing was accepted; the message names the plugin, the point and why; since 3.18 |
| `proposed` | error code | - | a `Start` or a `Send` on a connection whose calls are proposals: nothing was made; it was stored as proposal `proposal`, which a person decides (see `Answer`); since 3.26 |
| `reopen_refused` | error code | - | a `Reopen` changed nothing; the message says why, such as the open supervisor thread it names; since 3.31 |
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
| `id` | integer | required | the thread's number; since 3.0, before it a `{scope, key}` object |
| `recent` | integer | required | how many bytes of its last output to return |
| `events` | integer | optional | how many of its last events to return, at most 100; absent or 0 for none; since 1.5 |

Result: `ThreadDetail`

### Operation `ThreadEvents`

One thread's events before a position, newest last, each as `Events` delivers it and as `Thread` returns them, with text left out for a caller that may not read it. Asking again before the oldest event returned reads the thread's log back to its start; an empty result is the start. Since 3.3.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the thread's number |
| `before` | integer | optional | the position to read before; absent or 0 for the thread's last events |
| `limit` | integer | required | how many events to return, at most 100 |

Result: [`Event`]

### Operation `Scopes`

The scopes the caller may see, each with the name a person knows it by. It takes no arguments. Since 3.5.

Result: [`ScopeInfo`]

### Operation `SetName`

Records the name the chat service shows people for a conversation: a scope's own channel when `link.thread` is empty (`link.client` owns the scope and `link.channel` is its key), else the chat thread of a link. A change writes one `named` event; the same name again changes nothing. It needs the operation `SetName` in the conversation's scope, which a chat front end's caller and the owner's connection have; a conversation of no scope or link is `not_found`. Since 3.5.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `link` | `LinkRef` | required | the conversation; `thread` empty for a scope's channel |
| `name` | string | optional | the name as the service shows it, such as `#general` for a Discord channel, at most 256 bytes; absent or empty forgets it |

Result: none

### Operation `SetLayout`

Records how a chat client's service lays out the conversations it can see, as a person sees them in the service's own client: its groups (Discord's categories), the channels of each in order, and each channel's open threads in order, none archived. It replaces what the client recorded before. A change writes one `layout` event; the same layout again changes nothing. The daemon keeps it in memory only: after a restart a client has none until it records it again, as a chat front end does after each connect. The core sets `scope` and `thread` itself and ignores what the call gives for them; names are cut to 256 bytes, and a client's layout to 2000 channels and 5000 threads. It needs the operation `SetLayout` in a scope of the caller's whose owner is `client`, which a chat front end's caller and the owner's connection have; otherwise it is `forbidden`. Since 3.21.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `client` | string | required | the chat client's name, as in `LinkRef` |
| `groups` | [`LayoutGroup`] | required | in the service's order; the channels in no group are a group with no `id` |

Result: none

### Operation `Layout`

The layout each chat client recorded (`SetLayout`), one entry per client, as the caller may see it. The owner sees every channel the client recorded. Any other caller sees only the channels that are the own channels of its scopes, and only the groups that hold one of them; the threads of such a channel are all its open threads. Names and URLs are text, absent for a caller that may not read text. A client that recorded none has no entry, and then a reader shows its scopes as before. It takes no arguments. Since 3.21.

Result: [`ChatLayout`]

### Operation `Start`

Starts a thread in a scope. The same `key` starts nothing twice and returns the thread the first call started.

Since 3.26 a `key` that starts with `proposal:` is kept for the calls of approved proposals: any caller's `Start` or `Send` under one is `forbidden`. On a supervisor's connection whose scope's autonomy is `propose`, a `Start` that the daemon would make is not made: it is checked as far as the scope, the agent and the effort go, then stored as a proposal in the supervisor's own thread, with the agent, the model and the effort it would run with, and answered `proposed` with the proposal's number. No thread, chat thread or workspace is made, and no hook runs. A repeated `key` returns the same proposal. A person decides it with `Answer`; approved, the daemon makes the `Start` then, as the supervisor, with that model and effort, in a repository the scope's `supervisor.repos` still lists (otherwise the outcome is `failed`), its input marked and its `thread_created` and `input` naming `via` `proposal`, under a key made from the proposal, so that it starts one thread however often it is approved, also across restarts.

Since 3.18, when plugins handle the input hook points in the scope, the daemon runs them in the call, before it accepts the input (design, section 7.3): `before_agent_run`, then `before_prompt_build`. One that stops the input makes the call `stopped`: no thread, no chat thread and no workspace is made, and nothing is recorded; the daemon logs the stop, without the text or the reason. The text `before_prompt_build` adds reaches the agent around the input, on lines of their own; the `input` event has the input as given in `text`, and the added text in `before` and `after`. A key accepted before runs no hook again.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `key` | string | required | makes the call safe to repeat |
| `scope` | string | required | the scope; for chat, a channel |
| `thread` | string | required | the conversation of the first link, in the scope's own channel, of the scope's owner; empty to let the scope's owner open one; since 3.0 it names a link, before it the thread's key |
| `agent` | string | optional | the agent's registered name; empty for the scope's agent; since 3.8 |
| `repo` | string | required | the repository the workspace is cloned from |
| `input` | `Input` | required | the first input |
| `model` | string | required | the model; empty for the scope's default |
| `effort` | string | required | the effort; empty for the scope's default |

Result: integer

The new thread's number; since 3.0, before it a `{scope, key}` object.

### Operation `Send`

Gives input to a thread. A thread with an open question takes it as the answer; an asleep thread is woken with it. A closed or removed thread takes nothing and is not woken: `closed`, since 2.3.

Since 3.18 the input hook points run in the call, before the input is accepted, as for `Start`: a stopped input is `stopped`, takes nothing, and writes no event. An answer to an open question runs `before_agent_run` only, and reaches the agent as given, as the supervisor's mark does not go before it. A key accepted before, and a closed or removed thread, run no hook. The reason an approved `AskInterrupt` gives the thread is a `Send` too: a stopped one makes the approving `Answer` `stopped`, and the approval stays open.

On a supervisor's connection (a token, since 2.2) `busy` is always taken as 0: a `Send` with 1 or 2 to a running thread is refused with `busy` and logged, nothing is queued and the turn goes on. There, and in `Start`, the daemon puts the line `From the supervisor of this channel:` before the input's text; the `input` event has the text with that line, and the thread's title is made without it. A `Send` that answers an open question is not marked: the agent gets the answer, and the `answer` event its text, as given.

A `Send` to a supervisor's thread whose session answers a wake in a conversation of its own (design, section 7.1) is answered `busy` whatever `busy` says, from when that conversation is started until it has ended and the session sleeps: the input is for the thread's own conversation, which the next `Send` then resumes. An answer to a question that the wake's conversation asked is taken as usual.

Since 3.26, on a supervisor's connection whose scope's autonomy is `propose`, a `Send` gives the thread nothing: it is stored as a proposal in the supervisor's own thread and answered `proposed` with the proposal's number, whatever the thread's state but closed or removed (`closed`) or failed (`refused`); the supervisor's own thread is `forbidden`. One to a thread waiting on a question is proposed as the answer to that question. A repeated `key` returns the same proposal. A person decides it with `Answer`; approved, the daemon makes the `Send` then, as the supervisor, marked as its input, with busy 0 and under a key made from the proposal, so that the thread gets it once however often it is approved, also across restarts. A thread that runs a turn then, or waits on a question it was not proposed to answer, is not given it (outcome `busy`); a proposed answer whose question is no longer open is not given (outcome `ended`). Its `input` or `answer` event names `via` `proposal`.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the thread's number; since 3.0, before it a `{scope, key}` object |
| `key` | string | required | makes the call safe to repeat: a key accepted before is ignored |
| `input` | `Input` | required | the input |
| `busy` | integer | required | when a turn is running: 0 refuse (`busy`), 1 deliver into the turn, 2 interrupt it and then send |

Result: none

### Operation `Answer`

Answers an open question, approval or proposal. An approval asked with `AskInterrupt` is approved by `allow: true` or `choices: ["approve"]`; otherwise it is refused. The asker's own person, a guest and a person not in the scope are `forbidden`. Since 3.26 a supervisor's proposal (`proposed`) is decided the same way, by the same people; approved, its call is made then (see `Start` and `Send`), and one `proposal_answered` says what came of it. A proposal decided before is `error`, as an answered approval is. An approval is stored before its call is made. When the call failed in a way that may pass later, such as a chat service that did not open the new thread's chat thread, or the daemon stopped before the outcome was stored, the proposal stays listed in `requests`. If that call took the input or made the thread, the next answer, whatever it says, records `run` as that first approval's outcome; if it took nothing, the next answer decides the proposal as an open one: a refusal refuses it, an approval makes the call again under the same key.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `request` | string | required | the request's `id`, as in `Request` and the `request` event |
| `answer` | `Answer` | required | the answer |

Result: none

### Operation `AskInterrupt`

Since 3.10. Asks a person in the scope to approve interrupting the running turn. The request is shown in the thread the daemon bound the caller to, a supervisor's own thread. The caller must have been given asking: a supervisor's connection has it; the owner's connection without such a thread is `forbidden`. Nothing is interrupted until approval. Once approved, the daemon takes the reason as the target's input, as `Send` with busy 2 would, as the asker, only if the turn that ran when asked still runs. The interrupt names that turn, and the daemon checks it in the same step as the interrupt. So when the turn ends after the approval took the reason and before the interrupt, nothing is interrupted, the turn after it neither, and the outcome is `ended`; the reason, taken while the turn ran, reaches the agent as any message sent into a turn that ended first does. When an answer that approves fails after it took the reason (its interrupt failed, or recording the outcome did), the approval stays open; approved again, it is decided again: the reason is not taken or given again, the turn that ran when asked is interrupted only if it still runs, checked in the same step, and otherwise the outcome is `ended`. A repeated key returns the same request; an empty key makes no repeat check. An open request for the same running turn is returned instead of a second one. The caller's own approval thread cannot be the target (`forbidden`). A thread without a running turn is `refused`; send instead.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the target thread's number |
| `key` | string | required | makes the request safe to repeat; empty for no repeat check |
| `reason` | string | required | the reason given to the thread |

Result: string

### Operation `Stop`

Interrupts the thread's running turn. Nothing happens when no turn runs.

Since 3.14 a call may name the turn to stop, by the number its `turn_started` event gives it. The daemon then interrupts nothing unless that turn is the thread's running turn, and it checks this in the same step as the interrupt. So a call made for a turn that the daemon has seen end never stops the turn after it. The daemon sees a turn end when it reads the agent's output, every 50 milliseconds, as soon as the agent has reported it. A turn that hataori did not start (one typed at the agent's terminal, or one the agent starts by itself after a background task) that begins before the daemon has read the named turn's end can still get the interrupt, from an agent whose interrupt names no turn, as Claude Code's does; Codex's names the turn. The same holds for the daemon stopping the session when an interrupted turn has not ended after the grace period: it looks at the turn once more right before, as far as it has read the agent's output. A call that names a turn which is not running is answered `refused`, and nothing changes. A daemon before 3.14 ignores the field and interrupts whatever turn runs, so a client that names a turn checks that the daemon speaks 3.14 or later; the project's client refuses to send it otherwise.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the thread's number; since 3.0, before it a `{scope, key}` object |
| `turn` | integer | optional | only this turn, by `turn_started`'s `turn`; absent or 0 for whatever turn runs; since 3.14 |

Result: none

### Operation `Link`

Since 3.4. Shows the thread in one more conversation, and returns its link. It is checked as `Send` is: the caller's person must be allowed in the thread's scope. A link outside the scope's own channel needs the channel in the scope's `show_in` (`<client>:<channel>`, docs/configuration.md); without it the call is `forbidden`, nothing is made, and the daemon logs one line naming the thread, the client and the channel. With `link.thread` empty, the daemon asks the client to open a conversation in `link.channel`, titled like the thread, once the link is allowed, and returns it; when the thread has a link of that client in that channel already, it returns that one and opens nothing. A conversation linked to the thread already changes nothing; one linked to another thread is `linked_elsewhere`.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the thread's number |
| `link` | `LinkRef` | required | the conversation; `thread` empty to have one opened in `channel` |

Result: `LinkRef`

### Operation `Close`

Since 2.3. Ends the agent process of an idle, asleep or failed thread and closes the thread: its state becomes `closed`, its workspace stays as it is, and a message does not wake it (`Send` answers `closed`). It answers once the agent process has ended. A thread with a turn running, a question open or a session starting is answered `refused`; stop the turn first. So is an idle thread whose last turn left background tasks running, as the idle period does not put one to sleep, and an idle thread with a message or an answer that was accepted and that no turn has reported yet, such as a message sent while a turn ran, which waits for that turn to end: nothing changes, and the message's turn runs; close again once it has. Closing a closed thread does not close it again, but stops its agent if that still runs, as after a stop that failed. A failed session that its thread has already replaced is answered `refused`. Whether the caller may close the thread is checked as for `Stop`: the owner or a person of the thread's scope; anyone else is answered `forbidden`, nothing changes and one log line is written.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the thread's number; since 3.0, before it a `{scope, key}` object |

Result: none

### Operation `Reopen`

Since 3.31. Takes a closed or removed thread back to `asleep`, so that its next message wakes it, as any asleep thread's does ([design section 5.1](design/overview.md#51-the-control-api)): in the same directory, with the same number, links and key. Nothing runs until that message. The result says what became of the directory and of the conversation:

- A closed thread whose directory is in place keeps it (`kept`). A removed thread's directory is taken back from `trash/` to its place while it is there (`restored`), with its unpushed work and its recorded conversation; when the trash pass deleted it, or a closed thread's directory is gone, it is made again at its place (`remade`): a new clone of the thread's repository, or an empty directory for a thread started confined. Neither ever replaces anything: when the place of a removed thread's directory is taken by something else, the call is answered `reopen_refused` and nothing changes.
- The next message resumes the recorded conversation (`resume`). It starts a new one in the directory (`new`) when the directory was made again, or when the session never recorded its conversation; the agent's first prompt then says that the chat thread holds the earlier exchange, and gives its last part. A conversation the agent no longer has when it is resumed is replaced as at any wake: a `session_replaced` event, and a new session in the same directory.

It writes a `state` event to `asleep` with reason `reopen`, then a `reopened` event. It is answered `reopen_refused`, and changes nothing, for a thread that is neither closed nor removed, a thread whose agent was never started, a removed thread that was taken in from a person's window (`TakeIn`), a thread whose directory cannot be put back, and a thread started confined (a supervisor's) while another confined thread of its scope is neither closed nor removed: the message names that one. Only the owner may reopen: the caller needs the `Reopen` permission, which the owner's connection on the control socket has and a chat front end gives the caller it makes for a press of its Reopen button, and its person must be the owner; any other caller is answered `forbidden`, nothing changes and one log line is written.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the thread's number |

Result: `Reopened`

### Operation `TakeIn`

Since 3.17. Makes a thread of a window that a person started ([design, section 4.8](design/overview.md#48-taking-in-a-session-a-person-started)): a tmux pane in which a session reported through hataori's Claude Code plugin, as `HostTerminals` gives it in `reported`. Only the owner's connection on the control socket may call it; every other caller is answered `forbidden`, and the refusal is logged.

Before anything changes it checks:
- the caller may act in the scope, else `forbidden` (so for a scope that is not configured), and the scope is enabled, else `disabled`;
- a session reported from the window and its process still runs, else `not_found`;
- that session is interactive, else `refused`;
- the window is not taken in already, else `refused`;
- the window's holder confirms that the window exists, and that the process that reported runs on the window's terminal as its foreground process, else `refused`. An environment is inherited, so a process that the session's own shell started names the same window.

Each refusal changes nothing and is logged. The daemon keeps no report of a session that is not interactive, so such a window is answered `not_found`. Then the thread is made in the scope as `Start` makes one, with its first link opened by the scope's owner. Its session is the window's session id, its title is the name of the session's directory, and its workspace is that directory, recorded as not hataori's: no removal touches it (`Remove`). No agent is started. From the window's next hook on, the hooks of the process that was taken in report to the thread, also while the daemon is down, and the scope's rules answer its permission requests; today they are allowed automatically. The hooks of any other process in the window are only listed. Input from chat or `Send` is pasted into the window, and only while the process that was taken in is the window's foreground process; `Stop` interrupts the running turn with Escape, and nothing ends the window's process. The same `key` takes in nothing twice and returns the thread the first call made.

Since 3.20, when the window closes, the thread goes asleep with the reason `window closed` and its window is let go, as by `Release`: when the agent that was taken in reports its end (its `SessionEnd`, but for a `/clear`, after which it goes on), or when the window's holder finds it gone or its agent ended with no hook. The next message resumes the session under hataori's own host, in the same directory, with `--resume`, the model the session last reported and the configuration directory the window used; the options the person started it with are not reused. A session that starts in another window with the same session id is listed with the thread's number (`ReportedSession.thread`) and is not taken in.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `window` | string | required | the window, as `ReportedSession.window` names it; not text |
| `scope` | string | required | the scope the thread is made in |
| `key` | string | required | makes the call safe to repeat |

Result: integer

The new thread's number.

### Operation `Release`

Since 3.17. Ends a take-in: the thread goes asleep, as an idle thread does after its idle period, the window's hooks only report again, and the window is left as it is. A thread that was not taken in, that is not idle, or that has a message or an answer accepted and not reported by a turn yet, is answered `refused`, and nothing changes. It is checked as `TakeIn` is.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the thread's number |

Result: none

### Operation `Trust`

Since 3.28. Lets through the files a start-up dialog named, which blocked the start of the thread's current session, which `session` must name: its last `state` event, from `creating` to `failed` with reason `blocked at start`, lists them in `trust`, and `files` must be exactly those, in any order. The files are recorded as trusted for the thread's scope and repository (`TrustedFile`), and the thread starts again: a new session in the same workspace with the same first input, which names the same `via`, with one `session_replaced` event of reason `trusted`; a session that was woken from asleep gets the thread's earlier conversation before its input, as a session that replaces one that cannot resume does. From then on every launch in that scope on that repository may answer a dialog that names these files and no other, as the config's `trust_files` does. It needs a caller whose person is the owner; anyone else is answered `forbidden`, with one log line, and nothing is recorded. A thread whose current session is not `session` or did not fail that way, or `files` that are not those, are answered `refused`, and nothing changes. The owner's connection on the control socket may call it, and so may a chat front end's caller for a person who pressed its button.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the thread's number |
| `session` | integer | required | the session the `state` event names |
| `files` | [string] | required | the files, as the `state` event's `trust` names them |

Result: none

### Operation `StartWithoutSettings`

Since 3.28. Starts the thread again without its repository's settings, when a start-up dialog that named files blocked the start of its current session, which `session` must name (its last `state` event, from `creating` to `failed` with reason `blocked at start`, has `trust`). The thread is marked so that every later launch of it, after a sleep, a restart or a recovery too, leaves the project's and the local settings files out: their hooks and commands never run, and a dialog that names only those files is answered. The repository's code and instructions are still read. A new session starts in the same workspace with the same first input, with one `session_replaced` event of reason `without project settings`, as for `Trust`. It is checked as `Start` is: the caller's person must be allowed in the thread's scope; anyone else is answered `forbidden`, with one log line, and nothing changes. A connection whose calls are proposals (a supervisor's under the autonomy `propose`) is answered `forbidden` too: a person makes this choice. A thread whose current session is not `session` or did not fail that way is answered `refused`.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the thread's number |
| `session` | integer | required | the session the `state` event names |

Result: none

### Operation `Trusts`

Since 3.28. Lists the files `Trust` recorded, in the scopes the caller may see, oldest first. It takes no arguments.

Result: [`TrustedFile`]

### Operation `Untrust`

Since 3.28. Withdraws one file `Trust` recorded, by its `id`: the next start whose dialog names it is blocked again. A session already running is not changed. It is allowed as `Trust` is; an `id` that is not one, or one of a scope that is not the caller's, is answered `not_found`.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the `TrustedFile`'s `id` |

Result: none

### Operation `Remove`

Since 3.20, a closed thread taken in from a window (`TakeIn`) is removed without its workspace, which is the directory of the session a person started: its sessions' state directories go and the thread's state becomes `removed`, and the directory is left as it is, whatever it holds. Before 3.20 such a thread was refused with reason `the workspace is not hataori's`. Every other thread is removed as follows.

Since 2.3. Removes the workspace of a closed thread, and only when every step of the proof of [design section 4.6](design/overview.md#46-cleanup-without-losing-work) holds at that moment: no modified, staged, untracked or non-disposable ignored file, every commit on a remote asked at that moment, the same in every submodule and nested repository, and no process using the directory. The workspace is renamed into `trash/` inside the root, and the thread's state becomes `removed`, with a `state` event whose `reason` says what asked for it. When any step does not hold or cannot be answered, nothing changes: the answer is `removal_refused` with the reason, the thread stays `closed`, and one `removal_refused` event is written. In one case something has changed: when the rename into `trash/` started and could not finish, the workspace is no longer at its place, the reason starts with `workspace moved into trash, removal not finished`, and `detail` says where it is; the thread stays `closed`. Before the proof, the closed thread's own agent must have ended: one that cannot be stopped refuses with reason `agent not stopped`. A thread that is not closed is answered `refused`, with one `removal_refused` event of reason `thread not closed`. It is checked as `Close` is.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the thread's number; since 3.0, before it a `{scope, key}` object |

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
| `id` | integer | required | the thread's number; since 3.0, before it a `{scope, key}` object |

Result: `WorkspaceState`

### Operation `SignInLink`

Makes a link that signs a person in to the instance's dashboard. The link works once and only for a few minutes, and the dashboard session it opens belongs to the person it was made for: that session sees only the threads of the scopes the person may act in, every scope for the owner. Since 1.7.

On this socket the link is the owner's, for a connection without a token; a connection with a token is refused unless its caller may sign in, which a supervisor's may not. Since 1.11 a chat front end inside the daemon may also ask, for the person who asked in chat: the daemon resolves the author's id to a person and makes the link only for a person who may sign in, which is the owner alone in this version. A guest, and a person who may not sign in, are refused with `forbidden` and get no link. The dashboard's own client is always refused. An instance without a dashboard answers with `error`. The link and its token are not written to the event log or to any log.

Since 3.2 the request may name the dashboard's listener the link is for: `local`, the loopback listener (a browser on the host, or one on another machine through a port forwarded over SSH), or `web`, the web listener, over HTTPS. Without it the link is for the loopback listener when the instance has one, and for the web listener otherwise, which is what a link was before. A listener the instance does not have, and a value that names no listener, answer with `error`. A link signs in only on its listener, and the session it opens works only there.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `way` | string | optional | the listener: `local` or `web`; since 3.2 |

Result: string

### Operation `Terminal`

The terminal output of the thread's current session, then new output as the session writes it; since 1.9. It turns the connection into a stream, as `Events` does, and takes no input.

Positions count the bytes the session host wrote since its run started; the host keeps the end of them. The first chunk continues after `after` of `run`. When `after` is 0, `run` is another run, or the bytes after `after` are no longer kept, the first chunk is the end of the kept log with `reset` set, and the reader redraws. Every later chunk continues the one before it, or, when the stream had to start again, has `reset` set.

Since 3.25 the terminal's size may change while a stream is open: the dashboard gives the terminal the size of its owner's pane while the owner types into it (design, section 7.2). Every chunk carries the size its `data` and what follows are drawn at. A chunk whose size differs from the one before it says the terminal was resized at its position; it may have no `data`, and the agent's redraw at the new size follows it. A reader that meets one resizes its copy before it draws what follows. Before 3.25 the size never changed during a run.

A session whose host has ended (an asleep session, for one) sends the end of its last run's log with `reset` set and an empty `run`, then nothing until it runs again; then the new run's output, starting with a chunk marked `reset`. When the thread gets a new session, because the agent could not resume the old one, the stream goes on with the new session, from a chunk marked `reset`. A session that runs on pipes has no terminal and is `not_found`. A caller that may not read text is answered `forbidden`, since the terminal shows the prompts and the agent's output. A reader that falls far behind is cut off by the session host; the daemon then asks again from the last position, so the reader gets either a continuation or a `reset`.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the thread's number; since 3.0, before it a `{scope, key}` object |
| `run` | string | required | the run that `after` counts in; empty for none |
| `after` | integer | required | the position to continue after; 0 for the end of the log |

Result: `TerminalChunk`

### Operation `HostTerminals`

Since 1.10. The terminals on the host that hataori does not own, for display only: every pane of the daemon user's default tmux server, then every other terminal a process holds. Panes of a tmux server on another socket (`tmux -L`, `-S`) are listed as other terminals, without a screen. A terminal used by a session host's command is left out; it is shown as its thread. hataori never judges these terminals' state and never sends them anything.

Only the instance's owner may call it, and only when the config's `host_terminals` is `true`; every other call is answered `forbidden`. It takes no arguments. The result is an array, empty when there is no terminal.

Since 3.15, a tmux pane in which a session a person started runs has `reported`: what the hooks of hataori's Claude Code plugin (`hataori plugin`) reported from that pane. Only an interactive session is listed, and only while the pane is listed and its terminal is the one of the session's process; the session's end removes it. The daemon keeps these reports in memory only, and they hold no text of the session.

| Field | Type | Presence | Meaning |
|---|---|---|---|

Result: [`HostTerminal`]

### Operation `HostScreen`

Since 1.10. The visible screen of one tmux pane, as text, one line per row. Allowed as `HostTerminals` is. hataori runs only `tmux list-panes`, `ps` and `tmux capture-pane`, which change nothing; a hook the user set in their own tmux for these commands (`after-list-panes`, `after-capture-pane`) still runs, as it does when the user runs them. A pane that `HostTerminals` would not list is answered `not_found`. A caller that may not read text is answered `forbidden` for every pane, even one that `HostTerminals` would not list, before any pane is read, since a pane can keep a session's screen after `hataori attach` ends.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `pane` | string | required | the pane, as `TmuxPane.pane` |

Result: string

### Operation `OperateHost`

Since 3.11. Types into one tmux pane on the host that hataori does not own, by `TmuxPane.pane` as `HostTerminals` lists it. It turns the connection into a stream in both directions (see Connection).

Only the instance's owner may call it, and only when the config's `host_input` is `true` (which needs `host_terminals`); every other call is answered `forbidden`, and each refusal is logged. A pane that `HostTerminals` would not list is answered `not_found`. A pane in which one of hataori's own processes runs, such as `hataori attach`, is answered `forbidden`. A pane that is being operated already is answered `busy`, and the message names the person and since when. Each start and each end is one line in the daemon's log, with the person and the pane (its id, session, window and index), and for the end the reason. What is typed is written to no log, event, database or file, nor how much.

The operation ends, and the daemon sends its last message, with one of these reasons:

- `ended`: the client sent `End`;
- `closed`: the connection closed, or the caller's context ended;
- `idle`: no batch with a key came for the idle period (`host_input_idle`, 5 minutes by default);
- `pane gone`: tmux no longer lists the pane;
- `hataori's own`: one of hataori's processes runs in the pane now, by the command tmux reports for it or by the process table. The table is read once a second, or as soon as the previous read has finished when one takes longer;
- `stopping`: the daemon stops.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `pane` | string | required | the pane, as `TmuxPane.pane` |

Result: `HostOperation`

### Operation `HostKeys`

Since 3.11, and only on an `OperateHost` stream; elsewhere it is answered `not_supported`. Types the keys into the pane, in order. Before the batch the daemon reads the command tmux reports for the pane: when it is hataori's own or the pane is gone, nothing is sent, the answer is `ended` and the operation ends; when it cannot be read, nothing is sent, the answer is `error` and the operation goes on. A batch with an input that is not a key name of the list (under `HostKey`), that has both a key and a text, or neither, is refused whole with `error` and changes nothing. An empty batch does nothing and does not count as a key.

A key goes to the pane by `tmux send-keys` with its name, so tmux encodes it for the pane's modes (`Up` is `ESC [ A`, or `ESC O A` once the program asked for application cursor keys). A text goes into a tmux buffer of the operation's own by `tmux load-buffer` from standard input, never on a command line, then into the pane by `tmux paste-buffer -d`, which deletes the buffer. A text that is not a paste also gets `-S`, so that its control characters, such as a DEL, arrive as they are: since tmux 3.7 `paste-buffer` shows them as `^X` without it, and an older tmux, which refuses the flag before it does anything, is asked again without it. A paste uses `-p`, so tmux brackets it when the program asked for that, and goes as tmux pastes it: since 3.7 with its control characters shown as `^X`. tmux turns each line feed of a text into a carriage return. The buffer is also deleted when a paste fails, when the operation ends, and when the daemon starts, for any left over from an earlier run. No tmux client is attached, so no window changes size.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `keys` | [`HostKey`] | required | the inputs, in order |

Result: none

### Operation `End`

Since 3.11, and only on an `OperateHost` stream; elsewhere it is answered `not_supported`. Ends the operation with the reason `ended`. It takes no arguments.

| Field | Type | Presence | Meaning |
|---|---|---|---|

Result: none

### Operation `HostView`

Since 3.13. One tmux pane's screen, as `HostScreen` gives it, and where its cursor is, read right after the screen. Allowed as `HostScreen` is, and refused alike to a caller that may not read text. The dashboard reads it for a pane being operated (`OperateHost`), so that the page draws the cursor where tmux has it. hataori runs `tmux display-message -p` for the cursor, which changes nothing. A pane that `HostTerminals` would not list, and one that goes between the two reads, is answered `not_found`.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `pane` | string | required | the pane, as `TmuxPane.pane` |

Result: `HostView`

### Operation `AttachTerminal`

Since 2.1. Readies a thread's agent for a terminal on the host, for `hataori attach`. Only the instance's owner may call it; every other caller is answered `forbidden` and the refusal is logged. A thread that is closed, removed or failed, and a thread whose agent runs without a terminal (on pipes), are answered `refused` and nothing changes. An asleep thread is woken first, with no input: its agent resumes its conversation and the thread becomes idle, as after a message, but no turn starts. The answer comes once the session host listens, and at most after 90 seconds.

The result names the socket of the thread's session host. The terminal connects there itself and not through the daemon, so a daemon restart does not end it. What it then speaks to the session host is internal to hataori and not part of this protocol. While a terminal is attached the thread is not put to sleep, and the idle period starts again when the last one goes. A turn typed at the terminal is reported like any other, with `by` `terminal` in `turn_started` and `turn_ended`.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the thread's number; since 3.0, before it a `{scope, key}` object |

Result: `Attachment`

### Operation `Limits`

Since 1.13. The remaining usage of each agent, as its sessions last reported it: one entry per agent that has reported, by its registered name. An agent that has not, because nothing it gives says so, has no entry. A later report replaces an earlier one, whichever of the agent's sessions made it; it changes no thread and writes no event. The usage is the agent's account's, so it needs the `Threads` permission and no scope. It takes no arguments.

| Field | Type | Presence | Meaning |
|---|---|---|---|

Result: [`AgentLimits`]

### Operation `Clients`

Since 3.22. Whether each chat client is connected to its chat service: one entry per client that owns a scope the caller may see, by name, sorted by it. A chat front end is not connected from the daemon's start until its first connection succeeds, while its connection is down, and while it tries again after a failure; it keeps trying for as long as the daemon runs. A client that does not report its connection has no entry. It needs the `Threads` permission. It takes no arguments. A daemon before 3.22 answers `not_supported`.

| Field | Type | Presence | Meaning |
|---|---|---|---|

Result: [`ClientState`]

### Operation `Forge`

Since 3.27. One read of a forge, read only, for a repository of the caller's scope (design, section 7.1): its issues, one issue with its comments, its pull requests, one pull request with its reviews, comments, changed files, merge state, merge commit and the checks of its head, or the checks and commit statuses of a branch, a tag or a commit. Only the daemon answers it, on a supervisor's token connection, for a repository its scope lists under `supervisor.repos`, named as listed there or by its forge name (`<owner>/<name>`, of any case), that has a forge name and a forge of the instance (`forges`) that serves its address; any other repository is `forbidden`, and nothing is read. Every other connection reaches the core, which answers `not_supported`. The daemon makes the request with the forge's token, which no message carries. Text written by an author without write access to the repository (a title, a pull request's branch, a body, a comment, a review) is left out unless the scope's `supervisor.trusted_authors` lists that login; the item then has `withheld` and still names its author. In a pull request so withheld, the paths of its changed files and of the comments on them, and the names of its head's checks, are left out too. A `checks` read leaves out the names of the checks of a ref that is a commit (4 to 40 hexadecimal digits) or a pull request's ref (one with a part `pull`), since whoever wrote that commit may have named them; a branch or a tag has them. The answer is the daemon's: the text left out never goes on the socket. No CI log is read. A forge's refusal or rate limit is `error`, with the forge's reason in its message. A daemon before 3.27 answers `not_supported`.

Since 3.29, the read `deployed` tells whether a commit, or a pull request's merge commit, is in the running daemon's build: the commit its build info records (`vcs.revision`, as `hataori version` shows it, and `vcs.modified`), and whether the commit asked about is that commit or an ancestor of it, as the forge compares the two. An unmerged pull request is answered `merged` false, with no comparison. When the build info records no commit, or the forge cannot tell (it does not know one of the commits, or refuses), `in` is absent and `reason` says why; only a refusal of the pull request's read is an `error`. A daemon before 3.29 answers it `error`, as a read it does not know.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `repo` | string | required | the repository, as `supervisor.repos` lists it or by its forge name |
| `read` | string | required | `issues`, `issue`, `pulls`, `pull`, `checks` or, since 3.29, `deployed` |
| `number` | integer | optional | the issue's or the pull request's number, for `issue` and `pull`; the pull request's for `deployed` |
| `ref` | string | optional | the branch, tag or commit, for `checks`; for `deployed` without `number`, the commit, 7 to 40 hexadecimal digits |
| `state` | string | optional | for `issues` and `pulls`: `open`, the default, `closed` or `all` |

Result: `ForgeAnswer`

### Operation `Remind`

Since 3.32. Sets a reminder of the caller's scope, a wake of its supervisor at a time it asks for, with a note that the wake gives back; or cancels an open one by its number (design, section 7.1). Exactly one of `at`, `delay` and `cancel` is given. The time is from 5 minutes to 7 days ahead, and `note`, which a reminder that is set needs, is at most 1024 bytes. At most `supervisor.reminders_max` reminders of a scope are open, each until its wake was given; one that a wake already being given carries counts still, and cannot be cancelled any more. Anything else is answered `error` with the reason in its message, and nothing changes. A reminder that is set counts as one send of the supervisor's `sends_per_hour`, whatever becomes of it, and over that budget it is `forbidden`; a cancel counts as none. Reminders are the scope's, kept in the store: a daemon restart keeps them, and a new supervisor thread of the scope gets the open ones. Each wakes once. Only the daemon answers it, on a supervisor's token connection, in every autonomy. Every other connection reaches the core, which answers `not_supported`, as a daemon before 3.32 does. The call is not safe to repeat: each that is answered sets a reminder of its own.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `at` | time | optional | when the reminder is due |
| `delay` | integer | optional | or how many seconds from now |
| `note` | string | optional | what the wake gives back; required to set a reminder |
| `cancel` | integer | optional | the number of the open reminder to cancel |

Result: `RemindAnswer`

## Requests the daemon sends

Since 3.16. The daemon sends these to a plugin, on a connection made with the plugin's token that is not a stream: the oldest such connection that is still open. A connection that becomes a stream takes none from then on, so a plugin keeps one connection for requests, as the project's client does, and follows events on others. Until the daemon has read a connection's first request, it does not know that the connection will be a stream, and may send a request on it: a client skips such a line, unanswered, on its stream, and the daemon counts the request as failed once the stream starts. A plugin that does not know one answers `not_supported`, as for any request.

### Request `hook`

Calls the plugin's handler on one hook point (design, section 7.3). The handlers of a point run one after another: by the priority of the plugin's grant, higher first, then in the config's order, each given the text as the one before left it. A handler fails when it does not answer within its timeout (`left`), when its plugin has no connection open or the connection closes before the answer, when it answers with an error (`not_supported` too), and when its answer is one the point does not allow. A failure blocks or skips by the handler's policy (`GrantedHook.on_failure`): blocked, what passes the point is stopped, with hataori's account of the failure and the plugin's name; skipped, the next handler gets the text as it was. The daemon logs each call with the plugin, the point, the thread and the outcome, and no text.

| Point | May answer `stop` | May answer `text` | May answer `before`, `after` | Policy unless the config says otherwise |
|---|---|---|---|---|
| before_agent_run | yes | no | no | block |
| before_prompt_build | no | no | yes | skip |
| message_sending | yes | yes | no | skip |

At `message_sending` (since 3.19) the chat front end calls the handlers before it posts a row of its outbox, and before an edit that changes the text a post shows, as the post or its last edit left it, the progress line's edits too; not before a lamp, a delete, or an edit that keeps the text. `text` is the post's text. `continue` with `text` posts that text instead, in every link that shows the same part with the same text; `stop` cancels the post: it stays in the outbox with the plugin and the reason, nothing is posted in its place, and the rows behind it go on. What the handlers made of a row is stored with it before it is posted, so a row posted again, after a restart too, is not hooked again; a daemon that stops during the call calls the handlers again. A row of a thread's link is called with the thread's number and scope; a row with no link in a scope's own channel or a chat thread of it, such as the reply to a trigger, with that scope and no thread; a row in neither, such as a direct message, is not hooked.

At `before_prompt_build` each handler's `text` is the input with the text the handlers before it added, around it; what a handler adds goes around that. Before 3.18 `before_prompt_build` allowed `text` and not `before` and `after`; no version before 3.18 called it.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `point` | string | required | the hook point |
| `thread` | integer | optional | the thread's number; absent at a point before the thread has one |
| `scope` | string | required | the thread's scope |
| `text` | string | optional | what passes the point: the input or the post, as the handler before left it; text, left out for a plugin without the `conversation` grant |
| `left` | integer | required | milliseconds the handler has to answer; a later answer is not waited for and counts as a failure |

Result: `HookAnswer`

### Request `plugin_started`

Tells the plugin that it is connected, with its grant: what its manifest asks for and the config grants. The daemon sends it on the plugin's first connection, and again on a connection made when it had no connection open that is not a stream, such as after the plugin was started again. It sends `hook` requests whether or not this one was answered; its answer, an error too, changes nothing and is logged.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `plugin` | string | required | the plugin's id |
| `scopes` | [string] | required | the scopes it may see and act in |
| `operations` | [string] | optional | the operations it may call; absent for none |
| `events` | [string] | optional | the event kinds it may read; absent for none |
| `hooks` | [`GrantedHook`] | optional | the hook points it handles; absent for none |
| `conversation` | boolean | optional | whether it may read text |

Result: none

### Request `plugin_stopping`

Tells the plugin that hataori is about to stop it. The daemon waits for the answer at most `left`, and stops the plugin then whether it answered or not. A plugin with no connection open is told nothing.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `plugin` | string | required | the plugin's id |
| `left` | integer | required | milliseconds before it is stopped |

Result: none

## Types

### Type `LinkRef`

Where a thread is shown: one conversation of one client; since 3.0. A thread has one or more, and a conversation links to one thread at most.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `client` | string | required | the client's name, such as the chat front end's |
| `channel` | string | required | opaque; for chat, a channel |
| `thread` | string | required | opaque; for chat, the chat thread |

### Type `Filter`

A field left empty matches everything the caller may see. A `thread`, or the thread of a `link`, in a scope the caller may not see is read as a thread that does not exist: the answer is the same, and does not name that scope.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `scope` | string | required | only this scope |
| `thread` | integer | required | only the thread of this number; 0 for any; since 3.0, before it a `{scope, key}` object |
| `link` | `LinkRef` | optional | only the thread linked to this conversation; none when no thread is; since 3.0 |

### Type `Input`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `text` | string | required | what is given to the agent |
| `link` | `LinkRef` | optional | the conversation it came through, a link of the thread; the daemon keeps it only when it is one, and the `input` event names it; since 3.6 |

### Type `Answer`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `text` | string | required | a free-text answer to a question |
| `choices` | [string] | required | the tokens of the chosen options, for a question with choices; may be `null` |
| `allow` | boolean | required | the decision, for an approval |
| `link` | `LinkRef` | optional | the conversation it came through, as in `Input`; since 3.6 |

### Type `ChatLayout`

One chat client's layout (`Layout`); since 3.21.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `client` | string | required | the client's name, as in `LinkRef` |
| `groups` | [`LayoutGroup`] | required | in the chat service's order |

### Type `LayoutGroup`

A group of channels, such as a Discord category; since 3.21.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | string | optional | opaque; absent for the channels in no group |
| `name` | string | optional | text |
| `channels` | [`LayoutChannel`] | required | in the chat service's order |

### Type `LayoutChannel`

One channel of a `LayoutGroup`; since 3.21.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | string | required | opaque, as `LinkRef.channel` |
| `name` | string | optional | text, such as `#general` |
| `url` | string | optional | opens the channel in the chat service's own client; text, since a service may put a name in it |
| `scope` | string | optional | set by the daemon: the scope whose own channel this is, when the caller may see it; absent for a channel hataori does not serve |
| `threads` | [`LayoutThread`] | required | the open threads, in the chat service's order |

### Type `LayoutThread`

One open thread of a `LayoutChannel`; since 3.21.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | string | required | opaque, as `LinkRef.thread` |
| `name` | string | optional | text |
| `url` | string | optional | opens the thread in the chat service's own client; text, as in `LayoutChannel` |
| `thread` | integer | optional | set by the daemon: the number of the thread linked to this conversation, when the caller may see that thread's scope; absent for a thread hataori does not hold |

### Type `ScopeInfo`

One scope the caller may see; since 3.5.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `key` | string | required | the scope's key |
| `name` | string | optional | the config's name for it, else its channel's as the chat service shows it (`SetName`), which is text and absent for a caller that may not read text; absent when neither is known |

### Type `TrustedFile`

One file the owner let through with `Trust`; since 3.28.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | its number, which `Untrust` takes |
| `scope` | string | required | the scope whose threads it is trusted for |
| `repo` | string | required | the repository URL, as the threads' `Start` gave it |
| `file` | string | required | the file, as the dialog named it |
| `by` | string | required | the person who trusted it |
| `at` | time | required | when |

### Type `ThreadInfo`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the thread's number, unique in the instance; since 3.0, before it a `{scope, key}` object beside `number` |
| `scope` | string | required | the scope it belongs to; since 3.0 |
| `links` | [`LinkRef`] | required | where it is shown, the first link first; empty for a thread from before 3.0 whose scope is no longer in the config; since 3.0 |
| `title` | string | optional | from the first line of the first prompt; text; absent for a caller that may not read text, and when it is empty; optional since 2.0, required before |
| `name` | string | optional | the chat thread's own name as the chat service shows it, of the thread's first link (`SetName`), which follows a rename; text; absent for a caller that may not read text, and when none is known; since 3.7 |
| `repo` | string | required | the repository of its workspace |
| `state` | string | required | `creating`, `idle`, `running`, `waiting`, `asleep`, `closed`, `removed` or `failed` |
| `started_by` | string | required | the principal that started it; a thread recorded by an older daemon holds its author's chat id (before callers were resolved to people, #221) or the name an old-form config, one without `people`, gave them (`owner`, or `<client>/<id>` for a poster): it reports the person who lists the id its `thread_created` event's `from` records, else the person who lists the recorded id, else the recorded value (`owner` among them) |
| `created` | time | required | when it was started |
| `updated` | time | required | when its state last changed |
| `agent` | string | optional | the registered name of the agent of its current session; since 1.5 |
| `model` | string | optional | the model of its current session: as the agent reports it, and as asked for (or the scope's default) until it does; since 1.5 |
| `effort` | string | optional | the effort, in the same way as `model`; since 1.5 |
| `turns` | integer | optional | the turns started in the thread, in all its sessions; absent for none; since 1.5 |
| `last` | string | optional | the last line that is not blank of its last `text` event that has one, trimmed, at most 200 bytes; text; since 1.5 |
| `background` | integer | optional | the `background` of the thread's last `turn_ended` event; absent for 0 or before its first turn ends; since 1.5 |
| `context` | `Context` | optional | the last context use the agent reported; absent until it reports one; since 1.8 |
| `tokens` | `TokenCount` | optional | the thread's tokens; absent until the agent reports usage; since 1.8 |
| `terminal` | string | optional | the terminal device the agent of its current session runs in, as its session host opened it, such as `/dev/pts/3` or `/dev/ttys003`; absent for a session on pipes, while the thread is asleep, closed, removed or failed, once the agent's command has exited, while its session host does not answer within a second, and for a session host started before 1.12 until the session's next start; not text; since 1.12 |

### Type `ThreadDetail`

The fields of `ThreadInfo`, and:

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | as in `ThreadInfo` |
| `scope` | string | required | as in `ThreadInfo` |
| `links` | [`LinkRef`] | required | as in `ThreadInfo` |
| `title` | string | optional | as in `ThreadInfo` |
| `name` | string | optional | as in `ThreadInfo` |
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
| `context` | `Context` | optional | as in `ThreadInfo` |
| `tokens` | `TokenCount` | optional | as in `ThreadInfo` |
| `terminal` | string | optional | as in `ThreadInfo` |
| `requests` | [`Request`] | required | the open questions and approvals; open approvals asked here are listed whatever the thread state, and since 3.26 so are the open proposals shown here, and the approved ones whose outcome is not stored yet, as approvals with the choices Approve and Refuse; may be `null` |
| `recent` | string | required | the end of its last output, at most the bytes asked for: its `text` events in order, each a whole message of the agent, with a blank line (`\n\n`) between two of them, also between two turns; cut at the start of a character, and with no part of a blank line left at the cut. Before 3.12 the messages were joined with nothing between them, and the cut could fall inside a character |
| `events` | [`Event`] | optional | its last events, as many as `events` asked for, newest last, each as `Events` delivers it; since 1.5 |
| `cancelled` | [`CancelledPost`] | optional | its last posts that a plugin's `message_sending` handler cancelled, at most 20, newest last, from its `post_cancelled` events; absent for none. A summary, as `recent` is: a caller limited to some event kinds gets it whatever kinds it was granted; since 3.19 |

### Type `CancelledPost`

Since 3.19. A post of the chat front end, or an edit of one, that a plugin's `message_sending` handler cancelled. It stays in the outbox, and nothing was sent in its place.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `time` | time | required | when it was cancelled |
| `link` | `LinkRef` | required | the conversation it was to be posted in |
| `event` | integer | required | the position of the event it was to show |
| `part` | integer | required | which of that event's messages it was |
| `plugin` | string | required | the plugin whose handler cancelled it |
| `failed` | boolean | optional | the handler failed where its policy blocks, rather than answered `stop`; `reason` is then hataori's account of the failure |
| `edit` | boolean | optional | it was an edit of a post, which keeps what it showed before |
| `reason` | string | optional | why: the plugin's own, or, when `failed`, hataori's account of the failure; text either way, left out for a caller that may not read text |

### Type `Context`

A context use, in tokens; since 1.8.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `used` | integer | required | tokens in the context |
| `size` | integer | required | the context window |

### Type `TokenCount`

Tokens counted from the `usage` events: input, output, cache reads and cache writes together; since 1.8.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `turn` | integer | required | the highest total reported in the last turn that reported usage |
| `today` | integer | required | the tokens of every turn of the thread reported today, in the daemon's local time zone; a report adds its total less the turn's highest before it, so a count that goes down adds nothing and a turn that runs past midnight counts in both days |

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

### Type `Reopened`

What a `Reopen` did; since 3.31.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `workspace` | string | required | `kept` (in place), `restored` (taken back from `trash/`) or `remade` (made again at its place) |
| `conversation` | string | required | `resume` (the next message resumes the recorded conversation) or `new` (it starts a new one in the directory) |

### Type `Attachment`

Since 2.1. The result of `AttachTerminal`.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `number` | integer | required | the thread's number |
| `socket` | string | required | the path of the Unix socket of the thread's session host; not text |

### Type `TerminalChunk`

Since 1.9.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `run` | string | required | the session host's run, an opaque id; a new run starts a new count; empty when no host runs |
| `pos` | integer | required | the position after `data`; 0 when `run` is empty |
| `reset` | boolean | required | `data` is the end of the log, not a continuation: redraw |
| `cols` | integer | required | the terminal's width, at which `data` and what follows are drawn; since 3.25 it may change during a stream (see `Terminal`) |
| `rows` | integer | required | the terminal's height, as `cols` |
| `data` | string | optional | the raw bytes the terminal was sent, in base64; uninterpreted |

### Type `HostTerminal`

Since 1.10.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `device` | string | required | the terminal device, such as `/dev/pts/3` or `/dev/ttys003` |
| `process` | string | optional | the command of its foreground process |
| `activity` | time | optional | its last output, when known; for tmux, the window's last activity |
| `tmux` | `TmuxPane` | optional | where it is in tmux; absent for a terminal outside tmux |
| `reported` | `ReportedSession` | optional | what the hooks of the session in this tmux pane reported; absent without them; since 3.15 |

### Type `TmuxPane`

Since 1.10. A tab in a name or a directory is given as a space.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `pane` | string | required | the pane's id, such as `%3`; what `HostScreen` takes |
| `session` | string | required | the tmux session's name |
| `window` | string | required | the window's index |
| `window_name` | string | optional | the window's name |
| `index` | string | required | the pane's index in its window |
| `dir` | string | optional | the pane's current directory |
| `attached` | boolean | required | a tmux client shows the session now |

### Type `HostView`

Since 3.13.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `screen` | string | optional | the visible screen, one line per row, as `HostScreen` gives it; text, so optional |
| `cursor_x` | integer | required | the cursor's column, from 0 |
| `cursor_y` | integer | required | the cursor's row, from 0 at the top of the screen |

### Type `ReportedSession`

Since 3.15. What the hooks of a session that a person started in their own window last reported, through hataori's Claude Code plugin. It holds no prompt, message or tool input.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `window` | string | required | the window: its holder, a colon, and the holder's reference to it; for tmux `tmux:<socket path>,<server pid>,<pane id>`; not text |
| `agent` | string | required | the adapter's name, such as `claude-code` |
| `session` | string | required | the agent's session id |
| `dir` | string | required | the session's working directory; not text |
| `event` | string | required | the name of the last hook event reported |
| `at` | time | required | when the daemon took that report |
| `thread` | integer | optional | the number of the thread whose session this is, when it is one: a thread's session opened again in a person's window |

### Type `HostOperation`

Since 3.11. `OperateHost`'s result.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `idle` | integer | required | seconds without a key after which the operation ends |

### Type `HostKey`

Since 3.11. One input to an operated pane: either `key` or `text`.

A key name is tmux's name for the key, after modifiers, each of `C-`, `M-` and `S-` at most once and in any order: `Up`, `Down`, `Left`, `Right`, `Home`, `End`, `PPage`, `NPage`, `IC`, `DC` and `F1` to `F12` with any modifiers; `Tab` with `C-` or `M-`; `Enter`, `BSpace`, `Escape` and `BTab` with `M-` only; or one printable ASCII character other than a space with `M-`, or with `C-` when it is a letter or one of `@ [ \ ] ^ _ ?`, never with `S-`. These are the names tmux 3.4 encodes for the pane; it types a name it cannot encode, such as `S-Tab`, into the pane as its letters, so such names are refused. A character alone is text. A key name is on tmux's command line while it runs, so a name with a character shows that character to other users of the host; text never is.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `key` | string | optional | a key name, as above |
| `text` | string | optional | text, written to the pane as it is; text, so optional |
| `paste` | boolean | optional | the text is a paste: tmux brackets it when the program asked for that |

### Type `HookAnswer`

Since 3.16. A plugin's answer to `hook`.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `action` | string | required | `continue`: it passes; `stop`: it does not, at a point that allows it |
| `text` | string | optional | with `continue`, what passes the point instead, at a point that allows it; absent, it passes unchanged |
| `reason` | string | optional | with `stop`, why, which the thread shows; text |
| `before` | string | optional | with `continue`, text to add before what passes the point, on lines of its own, at a point that allows it; text; since 3.18 |
| `after` | string | optional | with `continue`, text to add after it, likewise; text; since 3.18 |

### Type `GrantedHook`

Since 3.16. One hook point of a plugin's grant.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `point` | string | required | the hook point |
| `priority` | integer | optional | the manifest's; higher runs first; absent for 0 |
| `timeout` | integer | required | milliseconds its handler has: the config's, else the manifest's, else 15000 |
| `on_failure` | string | required | `block` or `skip`: what its failure does, the config's, else the point's |

### Type `AgentLimits`

Since 1.13. A report older than `every` seconds is not current: a reader shows it with its time, since the agent's sessions would have reported again by then if one ran.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `agent` | string | required | the agent's registered name |
| `at` | time | required | when the report was made |
| `every` | integer | required | seconds: how often the agent's sessions report while one runs |
| `windows` | [`LimitWindow`] | required | as the agent reports them, in its order; may be empty |

### Type `ClientState`

Since 3.22.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `client` | string | required | the client's name, as a scope names its owner and a `LinkRef` its client |
| `connected` | boolean | required | connected to its chat service now |
| `since` | time | required | when it last connected; when not connected, since when: the moment its connection ended, or the daemon's start when it has not connected since |

### Type `LimitWindow`

Since 1.13.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `name` | string | required | the agent's name for the window, such as `five_hour`, `seven_day`, `primary` |
| `minutes` | integer | optional | its length; absent when the agent does not say |
| `used` | integer | required | the share used, in percent, 0 to 100 |
| `resets` | time | optional | when it starts again; absent when the agent does not say |

### Type `ForgeAnswer`

The result of `Forge`: the one read asked for, in its field. Since 3.27.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `repo` | string | required | the forge name of the repository read |
| `issues` | `ForgeIssueList` | optional | of `issues` |
| `issue` | `ForgeIssueDetail` | optional | of `issue` |
| `pulls` | `ForgePullList` | optional | of `pulls` |
| `pull` | `ForgePullDetail` | optional | of `pull` |
| `checks` | `ForgeChecks` | optional | of `checks` |
| `deployed` | `ForgeDeployed` | optional | of `deployed`; since 3.29 |

### Type `RemindAnswer`

The result of `Remind`: the reminder that was set or cancelled. Since 3.32.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `number` | integer | required | its number, which no other reminder of the instance has |
| `at` | time | required | when it is due, or was |

### Type `ForgeDeployed`

The result of a `deployed` read. Since 3.29.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `build` | string | optional | the commit the running daemon was built from, as its build info records it; absent when it records none |
| `modified` | boolean | optional | the build had changes not in `build` |
| `pull` | integer | optional | the pull request asked about |
| `merged` | boolean | optional | of a pull request: whether it was merged |
| `commit` | string | optional | the commit checked: the one asked about, or the pull request's merge commit |
| `in` | boolean | optional | `commit` is `build` or an ancestor of it; absent when not known |
| `reason` | string | optional | why `in` is absent |

### Type `ForgeAuthor`

Who wrote an item's text, as the forge says. Since 3.27.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `login` | string | required | the author's login on the forge |
| `write` | boolean | optional | the author has write access to the repository |

### Type `ForgeIssue`

One issue of a list. Since 3.27.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `number` | integer | required | its number |
| `title` | string | optional | text; absent when withheld |
| `state` | string | required | `open` or `closed` |
| `author` | `ForgeAuthor` | required | who opened it |
| `labels` | [string] | optional | its labels |
| `assignees` | [string] | optional | the logins it is assigned to |
| `updated` | time | required | its last change |
| `url` | string | required | the page a person opens |
| `withheld` | boolean | optional | its author has no write access and the scope does not trust them: its title and body are left out |

### Type `ForgeIssueList`

Since 3.27.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `items` | [`ForgeIssue`] | required | the issues read, at most the forge adapter's fixed number |
| `more` | boolean | optional | the forge has more than were read |

### Type `ForgeIssueDetail`

An issue with its body and its comments: the fields of `ForgeIssue`, and these. Since 3.27.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `number` | integer | required | as in `ForgeIssue` |
| `title` | string | optional | as in `ForgeIssue` |
| `state` | string | required | as in `ForgeIssue` |
| `author` | `ForgeAuthor` | required | as in `ForgeIssue` |
| `labels` | [string] | optional | as in `ForgeIssue` |
| `assignees` | [string] | optional | as in `ForgeIssue` |
| `updated` | time | required | as in `ForgeIssue` |
| `url` | string | required | as in `ForgeIssue` |
| `withheld` | boolean | optional | as in `ForgeIssue` |
| `body` | string | optional | text; absent when withheld |
| `comments` | `ForgeCommentList` | required | its comments, each withheld by its own author |

### Type `ForgeComment`

A comment on a conversation, or on a line of a changed file. Since 3.27.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `author` | `ForgeAuthor` | required | who wrote it |
| `body` | string | optional | text; absent when withheld |
| `path` | string | optional | the file a comment on a changed line is on; absent in a withheld pull request, whose author wrote it |
| `created` | time | required | when it was written |
| `url` | string | required | its page |
| `withheld` | boolean | optional | its text is left out, as for `ForgeIssue` |

### Type `ForgeCommentList`

Since 3.27.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `items` | [`ForgeComment`] | required | the comments read, oldest first |
| `more` | boolean | optional | the forge has more than were read |

### Type `ForgePull`

One pull request of a list. Since 3.27.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `number` | integer | required | its number |
| `title` | string | optional | text; absent when withheld |
| `state` | string | required | `open`, `closed` or `merged` |
| `draft` | boolean | optional | it is a draft |
| `branch` | string | optional | its head branch; absent when withheld, since its author named it |
| `base` | string | required | the branch it is to be merged into |
| `author` | `ForgeAuthor` | required | who opened it |
| `updated` | time | required | its last change |
| `url` | string | required | the page a person opens |
| `withheld` | boolean | optional | its author has no write access and the scope does not trust them: its title, branch and body are left out |

### Type `ForgePullList`

Since 3.27.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `items` | [`ForgePull`] | required | the pull requests read, at most the forge adapter's fixed number |
| `more` | boolean | optional | the forge has more than were read |

### Type `ForgePullDetail`

A pull request with its discussion, its changed files, its merge state and the checks of its head: the fields of `ForgePull`, and these. Since 3.27.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `number` | integer | required | as in `ForgePull` |
| `title` | string | optional | as in `ForgePull` |
| `state` | string | required | as in `ForgePull` |
| `draft` | boolean | optional | as in `ForgePull` |
| `branch` | string | optional | as in `ForgePull` |
| `base` | string | required | as in `ForgePull` |
| `author` | `ForgeAuthor` | required | as in `ForgePull` |
| `updated` | time | required | as in `ForgePull` |
| `url` | string | required | as in `ForgePull` |
| `withheld` | boolean | optional | as in `ForgePull` |
| `body` | string | optional | text; absent when withheld |
| `head` | string | required | its head commit |
| `merged` | boolean | optional | it was merged |
| `merge_commit` | string | optional | the merge commit, once merged |
| `merge_state` | string | optional | the forge's word for whether it can be merged; absent when the forge gives none |
| `reviews` | `ForgeReviewList` | required | its reviews |
| `comments` | `ForgeCommentList` | required | the comments on its conversation |
| `review_comments` | `ForgeCommentList` | required | the comments on lines of its changed files |
| `files` | `ForgeFileList` | required | its changed files |
| `checks` | `ForgeChecks` | required | the checks of its head commit |

### Type `ForgeReview`

A person's review of a pull request. Since 3.27.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `author` | `ForgeAuthor` | required | who reviewed |
| `state` | string | required | the forge's word, such as `APPROVED`, `CHANGES_REQUESTED` or `COMMENTED` |
| `body` | string | optional | text; absent when withheld |
| `submitted` | time | required | when it was submitted |
| `url` | string | required | its page |
| `withheld` | boolean | optional | its text is left out, as for `ForgeIssue` |

### Type `ForgeReviewList`

Since 3.27.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `items` | [`ForgeReview`] | required | the reviews read |
| `more` | boolean | optional | the forge has more than were read |

### Type `ForgeFile`

One file a pull request changes. Since 3.27.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `path` | string | optional | its path, which the pull request's author wrote; absent when withheld |
| `previous` | string | optional | its path before a rename; absent when withheld |
| `status` | string | required | the forge's word, such as `added`, `removed`, `modified` or `renamed` |
| `additions` | integer | required | lines added |
| `deletions` | integer | required | lines removed |
| `withheld` | boolean | optional | the pull request is withheld, so its paths are left out |

### Type `ForgeFileList`

Since 3.27.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `items` | [`ForgeFile`] | required | the changed files read |
| `more` | boolean | optional | the forge has more than were read |

### Type `ForgeCheck`

One check run or commit status, without its log or its output. Since 3.27.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `kind` | string | required | `check_run` or `status` |
| `name` | string | optional | its name; absent when withheld: a check of the head of a withheld pull request, or of a commit or a pull request's ref in a `checks` read, which whoever wrote the commit may have named |
| `state` | string | required | a check run's state (`queued`, `in_progress`, `completed`, ...) or a status's (`pending`, `success`, `failure`, `error`) |
| `conclusion` | string | optional | a completed check run's: `success`, `failure`, `neutral`, `cancelled`, `skipped`, `timed_out` or `action_required` |
| `url` | string | optional | its page; absent when withheld, since a commit status's poster gives it |
| `withheld` | boolean | optional | its name and its page are left out |

### Type `ForgeChecks`

The checks and commit statuses of one commit. Since 3.27.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `commit` | string | required | the commit they are of |
| `state` | string | optional | the combined commit status; absent without statuses |
| `items` | [`ForgeCheck`] | required | check runs first, then commit statuses |
| `more` | boolean | optional | the forge has more than were read |

### Type `Event`

One entry of the event log. `hataori events` prints each event as this object, one per line.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `pos` | integer | required | its position: unique and increasing in the instance; read on after the last one seen |
| `thread` | integer | optional | the number of the thread it belongs to; absent for an event of no thread; since 3.0, before it a required `{scope, key}` object |
| `scope` | string | optional | the scope of that thread, or of the event; since 3.0 |
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
| `via` | string | optional | as in `input`: `proposal` for the thread of a supervisor's proposal that a person approved; since 3.26 |

### Event `state`

The session's state changed.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `from` | string | required | the state before |
| `to` | string | required | the state now |
| `reason` | string | optional | why, such as `idle`, `interrupted`, `crashed`, `window closed` (a thread taken in whose window closed, since 3.20), `blocked at start`, `cannot resume`, `wake`, `reopen` (from `closed` or `removed` to `asleep`, by the operation `Reopen`, since 3.31); for `to: closed` or `to: removed`, what asked for it: `command` (the `Close` or `Remove` operation) or `closed age` (the removal pass of the config's `cleanup.remove_closed_after`), since 2.3 |
| `detail` | string | optional | text for a person, such as the end of the terminal output of a session blocked at start |
| `trust` | array | optional | for `to: failed` with reason `blocked at start`: every file the start-up dialog named, as it named them, when the start was blocked because some of them are not trusted; `Trust` takes them; text; since 3.28 |

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

### Event `post_sent`

Since 3.19. The chat front end posted a row of its outbox in a link of the thread. It is written in the transaction that marks the row as posted, so once per row, also when the row is posted again after a restart; never for a cancelled row, an edit, a lamp or a delete. The daemon writes it only while a plugin of the config is granted this event kind.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `link` | object | required | the link it was posted in, a `LinkRef` |
| `event` | integer | required | the position of the event it shows |
| `part` | integer | required | which of that event's messages it is |
| `message` | string | required | the chat service's id of the posted message |
| `text` | string | optional | the text posted, as the plugins left it; text |

### Event `post_cancelled`

Since 3.19. A plugin's `message_sending` handler cancelled a row of the chat front end's outbox in a link of the thread: nothing was posted. `ThreadDetail.cancelled` lists them.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `link` | object | required | the link it was to be posted in, a `LinkRef` |
| `event` | integer | required | the position of the event it was to show |
| `part` | integer | required | which of that event's messages it was |
| `plugin` | string | required | the plugin whose handler cancelled it |
| `failed` | boolean | optional | the handler failed where its policy blocks |
| `edit` | boolean | optional | it was an edit of a post |
| `reason` | string | optional | why, as in `CancelledPost`; text |

### Event `input`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `turn` | integer | required | the turn it went to |
| `text` | string | optional | the input |
| `principal` | string | optional | the person who gave it; since 2.2 |
| `from` | string | optional | the `<client>:<id>` it came from, as in `thread_created`; since 2.2 |
| `link` | object | optional | the link it came through, a `LinkRef`: `client`, `channel` and `thread`; absent for input from outside chat, such as the supervisor's; since 3.6 |
| `before` | string | optional | text the plugins' `before_prompt_build` handlers added before the input, which the agent got on lines of their own before `text`; absent when none did; since 3.18 |
| `after` | string | optional | the same, after it; since 3.18 |
| `via` | string | optional | the way of the daemon's own it came through when it came through no link: `dashboard` for input given in the dashboard; `terminal` for the prompt of a turn typed at the agent's terminal, when the agent reports it, written right after that turn's `turn_started` with the owner as `principal` and no `from` or `link` (since 3.24); `proposal` for the supervisor's input that a person approved as its proposal, with the supervisor as `principal` and `from` (since 3.26); absent for input through a link, from the control socket and from the supervisor's own sends; since 3.23 |

### Event `answer`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `turn` | integer | required | the turn the answer starts |
| `text` | string | optional | the answer |
| `principal` | string | optional | the person who gave it; since 2.2 |
| `from` | string | optional | the `<client>:<id>` it came from, as in `thread_created`; since 2.2 |
| `link` | object | optional | the link it came through, as in `input`; since 3.6 |
| `request` | string | optional | the request it answers, as in `request`; since 3.9 |
| `via` | string | optional | as in `input`; since 3.23 |

### Event `stop`

Since 3.23. A `Stop` interrupted the thread's running turn. It is written in the step that sends the interrupt, before it, so it comes before that turn's `turn_ended`: once per `Stop` that interrupted a turn, and none for a `Stop` while no turn runs or one that names a turn which is not the running one (`refused`). An interrupt that no `Stop` asked for, such as a `Send` with busy 2 or an approved `AskInterrupt`, writes none. A `turn_ended` with the reason `interrupted` that follows names nobody; the person who stopped it is here.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `turn` | integer | required | the turn interrupted, as in `turn_started` |
| `principal` | string | optional | the person whose `Stop` it was |
| `from` | string | optional | the `<client>:<id>` it came from, as in `thread_created` |
| `via` | string | optional | as in `input` |

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

### Event `approval_asked`

An interrupt approval request in the asker's thread, since 3.10.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `request` | string | required | what `Answer` names |
| `action` | string | required | `interrupt` |
| `target` | integer | required | the target thread |
| `turn` | integer | required | the target's turn that was running when asked |
| `reason` | string | optional | the reason, text |
| `principal` | string | optional | the person who asked |
| `from` | string | optional | the `<client>:<id>` it came from, as in `thread_created` |

### Event `approval_answered`

The decision in the asker's thread, since 3.10.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `request` | string | required | what `Answer` names |
| `allow` | boolean | required | whether approved |
| `outcome` | string | required | `interrupted`, `ended`, or `refused` |
| `target` | integer | required | the target thread |
| `principal` | string | optional | the person who answered |
| `from` | string | optional | the `<client>:<id>` it came from, as in `thread_created` |
| `link` | object | optional | the conversation where answered |
| `via` | string | optional | as in `input`; since 3.23 |

### Event `proposal_made`

A supervisor's `Start` or `Send`, stored as a proposal in its own thread instead of being made, since 3.26.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `request` | string | required | what `Answer` names |
| `number` | integer | required | the proposal's number, as `proposed` gave it |
| `action` | string | required | `send` or `start` |
| `target` | integer | optional | for `send`, the thread |
| `question` | string | optional | for `send` to a thread waiting on a question, the request it answers, as in `request` |
| `repo` | string | optional | for `start`, the repository |
| `agent` | string | optional | for `start`, the agent it runs |
| `model` | string | optional | for `start`, the model it runs with; for `send`, the thread's |
| `effort` | string | optional | the same, for the effort |
| `text` | string | optional | the input as the supervisor gave it, without the mark; text |
| `principal` | string | optional | the supervisor's person |
| `from` | string | optional | the supervisor's id, as in `thread_created` |

### Event `proposal_answered`

A person's decision of a proposal, in the thread it was shown in, and what came of it, since 3.26. One per proposal.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `request` | string | required | what `Answer` names |
| `number` | integer | required | the proposal's number |
| `action` | string | required | `send` or `start` |
| `allow` | boolean | required | whether approved |
| `outcome` | string | required | `run`: the call was made; `refused`: a person refused it; `busy`: approved, but the thread ran a turn or waited on a question it was not proposed to answer, so nothing was sent; `ended`: approved, but the question it answered was no longer open; `closed`: approved, but the thread is closed or removed; `failed`: approved, but the call was refused for another reason, in `reason` |
| `target` | integer | optional | the thread: for `send` its target, for a `start` that ran the thread it made |
| `reason` | string | optional | for `failed`, why, in the daemon's words: such as `not allowed`, when the supervisor's person may no longer act in the scope, or `the thread takes no input in its state` |
| `principal` | string | optional | the person who decided |
| `from` | string | optional | the `<client>:<id>` it came from, as in `thread_created` |
| `link` | object | optional | the conversation where decided |
| `via` | string | optional | as in `input` |

### Event `request`

The agent asks a question or an approval; the turn ends `blocked`.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `request` | string | required | what `Answer` names |
| `approval` | boolean | required | an approval rather than a question |
| `text` | string | optional | the question |
| `choices` | array | optional | its options, strings |
| `multi` | boolean | optional | several of `choices` may be chosen at once; absent when not; since 3.9 |

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

What the agent reports about the tokens of the running turn. Each count is the turn's total so far, not what was added since the last report. Since 3.30 a report may come after the turn's `turn_ended`: an agent may record a turn's last model message only after it reported the turn's end (Claude Code 2.1.330 does). That report names the ended turn in `turn`, and its counts go toward that turn, also when a caller's input has started the next turn by then (its `turn_started` may come first). It comes before the agent reports anything of its next prompt. Before 3.30 such a report was dropped, and every report with a `turn` came before that turn's end.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `turn` | integer | optional | the turn the counts are of: the running one, or, for a report after a turn's end, the turn that ended last (since 3.30); absent for a report outside a turn, whose counts go toward no `ThreadInfo.tokens`; since 1.8 |
| `model` | string | optional | the model the tokens were used with, when the agent says; since 1.8 |
| `input` | integer | optional | input tokens of the turn so far; since 1.8 |
| `output` | integer | optional | output tokens of the turn so far; since 1.8 |
| `cache_read` | integer | optional | tokens read from the prompt cache in the turn so far; since 1.8 |
| `cache_write` | integer | optional | tokens written to the prompt cache in the turn so far; since 1.8 |
| `context_used` | integer | optional | tokens in the context now, with `context_size`; since 1.8 |
| `context_size` | integer | optional | the context window, when the agent says; since 1.8 |

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
| `threads` | array | required | the numbers of the threads it stopped; since 3.0, before it `{scope, key}` objects |
| `scopes` | array | required | their scopes; a caller that may not see all of them does not get the event |

### Event `session_replaced`

The agent no longer had the conversation, or, since 3.28, a start that files blocked goes on: the owner let them through (`Trust`), or a person chose to start without the repository's settings (`StartWithoutSettings`); a new session started in the same workspace.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session that could not be resumed, or that a start-up dialog blocked |
| `new_session` | integer | required | the new session |
| `reason` | string | required | `cannot resume`, `trusted` or `without project settings` (both since 3.28) |

### Event `reopened`

Since 3.31. A closed or removed thread was taken back to `asleep` by the operation `Reopen`, after the `state` event of that change.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session the next message wakes |
| `principal` | string | optional | who reopened it |
| `workspace` | string | required | as `Reopened.workspace` |
| `conversation` | string | required | as `Reopened.conversation` |

### Event `removal_refused`

Since 2.3. A removal of the thread's workspace was refused, and nothing changed. A removal that is done is the `state` event to `removed`.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the thread's current session |
| `trigger` | string | required | what asked for it: `command` or `closed age`, as the `state` event's `reason` |
| `principal` | string | optional | the person who asked, for `command` |
| `reason` | string | required | why, such as `modified file`, `untracked file`, `ignored file not listed as disposable`, `unpushed commit on a branch`, `stash`, `commit only in a reflog`, `remote is another workspace`, `remote under a hataori root`, `process using the directory`, `git error`, `timeout`; or, from the daemon itself, `thread not closed`, `agent not stopped`, `no workspace recorded`, `the instance removes no workspaces`, `the workspace is not hataori's` (since 3.17, for a thread taken in; since 3.20 only when the daemon cannot remove a session's state, and such a thread is otherwise removed without its workspace), `the directory of a confined thread is never removed` (for a thread started confined, such as a supervisor's, whose directory is its scope's; before, the remover refused it as `not a workspace`) |
| `repo` | string | optional | where it was found, relative to the workspace, when not in the workspace's own repository |
| `kind` | string | optional | what `repo` is: `submodule`, `nested repository`, `nested bare repository` or `submodule without checkout` |
| `detail` | string | optional | text for a person, such as the file or the commit; for `thread not closed`, the thread's state |

### Event `named`

Since 3.5. The name the chat service shows for a conversation changed (`SetName`). The event belongs to the conversation's scope, and to the thread when the conversation is a linked chat thread.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `client` | string | required | as in `LinkRef` |
| `channel` | string | required | as in `LinkRef` |
| `thread` | string | optional | as in `LinkRef`; absent for a scope's channel |
| `name` | string | optional | the new name; text; absent when the name was forgotten, and for a reader that may not read text |

### Event `layout`

Since 3.21. A chat client's layout changed (`SetLayout`): read it again with `Layout`. The event belongs to no thread and no scope, holds only the client's name, and every caller that reads events gets it.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `client` | string | required | as in `LinkRef` |

### Event `caught_up`

Not stored and belongs to no thread: the mark in an `Events` stream after the events stored when the call was made. It has no data, and its `pos` is the last position sent (or `after`).

| Field | Type | Presence | Meaning |
|---|---|---|---|
