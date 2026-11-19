# The control protocol

Programs outside the daemon (the `hataori` command, a supervisor, a dashboard, plugins) reach the control API over the instance's Unix socket. This document describes every message, operation, type and event on that socket. `internal/wire/doc_test.go` fails when the code and this document disagree on a name, a field, a type or whether a field is required.

## Connection

- The socket is `<root>/state/control.sock`. When that path is too long for a Unix socket, the daemon listens on a short path in the user's runtime or temporary directory and writes that path into `<root>/state/control.sock.path`.
- Only the OS user that runs the daemon may connect. A connection whose client hello has no `token` acts as the instance's owner.
- A connection whose client hello has a `token` acts as the caller the daemon issued that token for, with the scopes and operations the daemon chose; nothing the client sends widens them. The daemon issues a token for each supervisor session, limited to that supervisor's scope (design, section 7.1). A token the daemon does not know, or no longer knows, is refused: the daemon's hello carries `error`, it logs the refusal without the token, and it closes the connection. A client that streams events or terminal output presents the token on that connection too. Since 2.2; an older daemon ignores the field and makes the connection the owner's, so a client with a token checks that the daemon speaks 2.2 or later.
- Each message is one JSON object on one line.
- Each side sends a `hello` first. The daemon reads the client's hello before it answers with its own. When the daemon refuses the client, its hello carries `error` and the daemon closes the connection.
- After the hellos, **either side may send requests**. A request has `op` set; a response has no `op`. Each side numbers its own requests, and a response carries the `id` of the request it answers. Several requests may be outstanding at once, and the answers may come in any order.
- The client sends the operations below. The daemon may send the client requests of its own; there are none in this version, and plugin hooks and scope owners in other processes will use them. A side that gets a request whose `op` it does not know answers with the error code `not_supported` and carries on.
- An `Events` request turns the connection into a stream: the daemon answers it once with an empty `response`, then sends one `response` with `event` set per event, until the client closes the connection. A client follows events on a connection of its own.
- A `Terminal` request does the same with terminal output: an empty `response`, then one `response` with `terminal` set per chunk. Since 1.9.
- On a stream the daemon reads nothing the client sends: no request on that connection is answered or carried out, and nothing reaches a terminal.

## Versions

The protocol version is `major.minor`, now **3.7**. Both hellos carry it.

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
| `closed` | error code | - | the thread is closed or removed: nothing was sent, and a message does not wake it; since 2.3 |
| `removal_refused` | error code | - | the workspace may hold work that exists nowhere else, so it was not removed; the message says why; since 2.3 |
| `linked_elsewhere` | error code | - | the conversation shows another thread; since 3.4 |
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

### Operation `Start`

Starts a thread in a scope. The same `key` starts nothing twice and returns the thread the first call started.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `key` | string | required | makes the call safe to repeat |
| `scope` | string | required | the scope; for chat, a channel |
| `thread` | string | required | the conversation of the first link, in the scope's own channel, of the scope's owner; empty to let the scope's owner open one; since 3.0 it names a link, before it the thread's key |
| `agent` | string | optional | the agent's registered name; empty for the scope's agent; since 3.7 |
| `repo` | string | required | the repository the workspace is cloned from |
| `input` | `Input` | required | the first input |
| `model` | string | required | the model; empty for the scope's default |
| `effort` | string | required | the effort; empty for the scope's default |

Result: integer

The new thread's number; since 3.0, before it a `{scope, key}` object.

### Operation `Send`

Gives input to a thread. A thread with an open question takes it as the answer; an asleep thread is woken with it. A closed or removed thread takes nothing and is not woken: `closed`, since 2.3.

On a supervisor's connection (a token, since 2.2) `busy` is always taken as 0: a `Send` with 1 or 2 to a running thread is refused with `busy` and logged, nothing is queued and the turn goes on. There, and in `Start`, the daemon puts the line `From the supervisor of this channel:` before the input's text; the `input` event has the text with that line, and the thread's title is made without it. A `Send` that answers an open question is not marked: the agent gets the answer, and the `answer` event its text, as given.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the thread's number; since 3.0, before it a `{scope, key}` object |
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
| `id` | integer | required | the thread's number; since 3.0, before it a `{scope, key}` object |

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

### Operation `Remove`

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

A session whose host has ended (an asleep session, for one) sends the end of its last run's log with `reset` set and an empty `run`, then nothing until it runs again; then the new run's output, starting with a chunk marked `reset`. When the thread gets a new session, because the agent could not resume the old one, the stream goes on with the new session, from a chunk marked `reset`. A session that runs on pipes has no terminal and is `not_found`. A reader that falls far behind is cut off by the session host; the daemon then asks again from the last position, so the reader gets either a continuation or a `reset`.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the thread's number; since 3.0, before it a `{scope, key}` object |
| `run` | string | required | the run that `after` counts in; empty for none |
| `after` | integer | required | the position to continue after; 0 for the end of the log |

Result: `TerminalChunk`

### Operation `HostTerminals`

Since 1.10. The terminals on the host that hataori does not own, for display only: every pane of the daemon user's default tmux server, then every other terminal a process holds. Panes of a tmux server on another socket (`tmux -L`, `-S`) are listed as other terminals, without a screen. A terminal used by a session host's command is left out; it is shown as its thread. hataori never judges these terminals' state and never sends them anything.

Only the instance's owner may call it, and only when the config's `host_terminals` is `true`; every other call is answered `forbidden`. It takes no arguments. The result is an array, empty when there is no terminal.

| Field | Type | Presence | Meaning |
|---|---|---|---|

Result: [`HostTerminal`]

### Operation `HostScreen`

Since 1.10. The visible screen of one tmux pane, as text, one line per row. Allowed as `HostTerminals` is. hataori runs only `tmux list-panes` and `tmux capture-pane`, which change nothing; a hook the user set in their own tmux for these commands (`after-list-panes`, `after-capture-pane`) still runs, as it does when the user runs them. A pane that `HostTerminals` would not list is answered `not_found`.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `pane` | string | required | the pane, as `TmuxPane.pane` |

Result: string

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

## Types

### Type `LinkRef`

Where a thread is shown: one conversation of one client; since 3.0. A thread has one or more, and a conversation links to one thread at most.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `client` | string | required | the client's name, such as the chat front end's |
| `channel` | string | required | opaque; for chat, a channel |
| `thread` | string | required | opaque; for chat, the chat thread |

### Type `Filter`

A field left empty matches everything the caller may see.

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

### Type `ScopeInfo`

One scope the caller may see; since 3.5.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `key` | string | required | the scope's key |
| `name` | string | optional | the config's name for it, else its channel's as the chat service shows it (`SetName`), which is text and absent for a caller that may not read text; absent when neither is known |

### Type `ThreadInfo`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | integer | required | the thread's number, unique in the instance; since 3.0, before it a `{scope, key}` object beside `number` |
| `scope` | string | required | the scope it belongs to; since 3.0 |
| `links` | [`LinkRef`] | required | where it is shown, the first link first; empty for a thread from before 3.0 whose scope is no longer in the config; since 3.0 |
| `title` | string | optional | from the first line of the first prompt; text; absent for a caller that may not read text, and when it is empty; optional since 2.0, required before |
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
| `requests` | [`Request`] | required | the open questions and approvals; may be `null` |
| `recent` | string | required | the end of its last output, at most the bytes asked for |
| `events` | [`Event`] | optional | its last events, as many as `events` asked for, newest last, each as `Events` delivers it; since 1.5 |

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
| `cols` | integer | required | the terminal's width |
| `rows` | integer | required | the terminal's height |
| `data` | string | optional | the raw bytes the terminal was sent, in base64; uninterpreted |

### Type `HostTerminal`

Since 1.10.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `device` | string | required | the terminal device, such as `/dev/pts/3` or `/dev/ttys003` |
| `process` | string | optional | the command of its foreground process |
| `activity` | time | optional | its last output, when known; for tmux, the window's last activity |
| `tmux` | `TmuxPane` | optional | where it is in tmux; absent for a terminal outside tmux |

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

### Type `AgentLimits`

Since 1.13. A report older than `every` seconds is not current: a reader shows it with its time, since the agent's sessions would have reported again by then if one ran.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `agent` | string | required | the agent's registered name |
| `at` | time | required | when the report was made |
| `every` | integer | required | seconds: how often the agent's sessions report while one runs |
| `windows` | [`LimitWindow`] | required | as the agent reports them, in its order; may be empty |

### Type `LimitWindow`

Since 1.13.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `name` | string | required | the agent's name for the window, such as `five_hour`, `seven_day`, `primary` |
| `minutes` | integer | optional | its length; absent when the agent does not say |
| `used` | integer | required | the share used, in percent, 0 to 100 |
| `resets` | time | optional | when it starts again; absent when the agent does not say |

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

### Event `state`

The session's state changed.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `from` | string | required | the state before |
| `to` | string | required | the state now |
| `reason` | string | optional | why, such as `idle`, `interrupted`, `crashed`, `blocked at start`, `cannot resume`, `wake`; for `to: closed` or `to: removed`, what asked for it: `command` (the `Close` or `Remove` operation) or `closed age` (the removal pass of the config's `cleanup.remove_closed_after`), since 2.3 |
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
| `principal` | string | optional | the person who gave it; since 2.2 |
| `from` | string | optional | the `<client>:<id>` it came from, as in `thread_created`; since 2.2 |
| `link` | object | optional | the link it came through, a `LinkRef`: `client`, `channel` and `thread`; absent for input from outside chat, such as the supervisor's; since 3.6 |

### Event `answer`

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `turn` | integer | required | the turn the answer starts |
| `text` | string | optional | the answer |
| `principal` | string | optional | the person who gave it; since 2.2 |
| `from` | string | optional | the `<client>:<id>` it came from, as in `thread_created`; since 2.2 |
| `link` | object | optional | the link it came through, as in `input`; since 3.6 |

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

What the agent reports about the tokens of the running turn. Each count is the turn's total so far, not what was added since the last report.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session |
| `turn` | integer | optional | the turn; absent for a report outside a turn, whose counts go toward no `ThreadInfo.tokens`; since 1.8 |
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

The agent no longer had the conversation; a new session started in the same workspace.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the session that could not be resumed |
| `new_session` | integer | required | the new session |
| `reason` | string | required | `cannot resume` |

### Event `removal_refused`

Since 2.3. A removal of the thread's workspace was refused, and nothing changed. A removal that is done is the `state` event to `removed`.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `session` | integer | required | the thread's current session |
| `trigger` | string | required | what asked for it: `command` or `closed age`, as the `state` event's `reason` |
| `principal` | string | optional | the person who asked, for `command` |
| `reason` | string | required | why, such as `modified file`, `untracked file`, `ignored file not listed as disposable`, `unpushed commit on a branch`, `stash`, `commit only in a reflog`, `remote is another workspace`, `remote under a hataori root`, `process using the directory`, `git error`, `timeout`; or, from the daemon itself, `thread not closed`, `agent not stopped`, `no workspace recorded`, `the instance removes no workspaces` |
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

### Event `caught_up`

Not stored and belongs to no thread: the mark in an `Events` stream after the events stored when the call was made. It has no data, and its `pos` is the last position sent (or `after`).

| Field | Type | Presence | Meaning |
|---|---|---|---|
