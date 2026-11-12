# hataori design overview

Status: proposal for review. Nothing here is implemented yet.

hataori succeeds [c-lord](https://github.com/yousan/c-lord). It keeps c-lord's model: one chat thread is one agent session, and one agent session owns one working directory. This document says what hataori takes from c-lord, what it changes, and how it is built.

How to read it:

- Issue numbers written `c-lord#123` are issues of `yousan/c-lord`.
- c-lord facts come from its docs and code at v1.4.420.
- Claims about the agent CLIs were checked on 2026-11-12 against Claude Code 2.1.322 and codex-cli 0.163.0 on Linux, by driving the real programs. Appendix B lists each check.
- Anything marked **(unverified)** was not checked by running it.

## 1. Summary

1. **Talk to agents through their structured interfaces, not their screens.** Claude Code runs as `claude -p` with `stream-json` in both directions. Codex runs as `codex app-server` (JSON-RPC). Both kept the subscription login, ran a 15-minute turn, and delivered permission prompts, questions, interrupt and resume as messages (section 3).
2. **This removes about half of c-lord's bugs by construction.** Of 443 bug issues, 106 came from driving a terminal (reading it, typing into it, keeping track of tmux windows), and 123 from rebuilding output and turn state out of a transcript file and timers (section 2).
3. **The price is live typing into a running session.** A person can still take a session over in the agent's own terminal UI between turns, and hand it back. They cannot type into it while hataori is driving it (section 3.4).
4. **Each agent process runs under a small session host that outlives the hataori daemon.** Restarting or upgrading hataori does not stop running turns, and output is read from a file with a stored position, so nothing is lost or posted twice (section 4.3).
5. **Everything needed after a restart is in SQLite before it is acted on:** inbound messages, outbound posts, open questions, turn state (section 6).
6. **hataori writes nothing into a working tree**, and removes a workspace only after proving there is nothing in it that exists nowhere else (section 4.6).
7. **One known risk:** Claude Code's docs say `--bare` will become the default for `-p`, and bare mode does not use the subscription login. The agent adapter boundary is kept narrow enough that a different Claude Code adapter can replace this one (section 3.5, question 1).

## 2. What c-lord taught us

### 2.1 Feature inventory

c-lord has grown to about 64,000 lines of Python and roughly 60 slash and text commands. Appendix A lists its user-visible features in 105 rows: 61 keep, 19 change, 25 drop.

What is kept is the daily loop: start a thread from a command, a text trigger or a webhook; get a clone of the bound repository; talk to the agent in the thread; answer its questions with buttons; see a lamp on your message; get pinged when the turn ends; stop, sleep, wake, close and clean up; hand files in and out; per-thread model choice; owner-only access by default; several instances on one host.

Most kept features keep their behaviour but not their mechanism: they are driven by an event from the agent instead of by text found on a screen or in a transcript. "Change" marks the features whose behaviour a user will notice changing: a message during a turn no longer interrupts it, nothing is written into the checkout, the HTTP calls become a local command.

What is dropped falls into three kinds:

- **Things that exist only because of tmux:** attaching a thread to a hand-made window, window lists, pane screenshots, resync, the folder-trust dialog handling, vim-mode handling, the unknown-prompt safety net, the menu watchdog.
- **A second control surface:** the HTTP API with its scheduled notifications and "lounge" endpoints. It accounts for most of the input-validation bugs. A local `hataori` command replaces it.
- **Opt-in extras and tooling:** table images, the mockup renderer, the status board, thread-name lamps, subagent "voices", the fuzz harness, evidence capture, localised READMEs.

### 2.2 Bug issues by cause

c-lord has 698 issues. 443 are bugs (labelled `bug`, or titled `bug:` / `fix:`). Each was put into one group by its title; borderline cases went to the cause named first in the title. The grouping is a judgment, so read the counts as ±10%. Appendix C lists every number.

| Cause | Count | Examples | What removes it in hataori |
|---|---:|---|---|
| Reading the terminal screen: finding menus, dialogs, limits and login state in pane text | 52 | c-lord#388, c-lord#990 | No screen. Questions, approvals, limits and errors arrive as typed messages. |
| Typing into the terminal: keystrokes lost, split, reinterpreted or sent to the wrong dialog | 22 | c-lord#560, c-lord#1284 | Input is one JSON message on stdin, any length, any characters. |
| tmux window bookkeeping: which window is which thread, twins, reaping | 32 | c-lord#649, c-lord#701 | No tmux. One process per session, tracked by the database. |
| Tailing the transcript file: replies lost, posted twice, user input echoed, internal text leaked | 39 | c-lord#553, c-lord#773 | Output is the event stream of a process hataori started, with a stored read position. |
| Guessing turn state from timers and polling: false "finished", false timeouts, stuck lamps, wrong person pinged | 64 | c-lord#541, c-lord#1132 | Turn start and end are events with a status. One state machine per session. |
| Subagents and background tasks, tracked through the transcript | 20 | c-lord#1136, c-lord#1302 | Events carry the parent tool call; background tasks have start and end events. |
| Restart and reconnect: state held in memory, messages missed while down | 17 | c-lord#671, c-lord#776 | State is in SQLite before it is acted on. Agent processes outlive the daemon. |
| Sleep, wake and the resident cap | 22 | c-lord#922, c-lord#1171 | "Is it working" comes from events, not from file times. Sleep is an explicit state. |
| Cleanup: work deleted, or nothing reclaimed | 22 | c-lord#903, c-lord#1080 | One check that must prove there is nothing to lose. Nothing injected into the working tree. |
| Authorization and instance isolation | 22 | c-lord#713, c-lord#596 | One gate in the core before any dispatch. One root and one bot identity per instance. |
| Config and input validation: setup, environment values, HTTP API | 32 | c-lord#1029, c-lord#1081 | Typed config parsed once at start. No HTTP API. |
| Chat API limits: rate limits, message size, thread names, boards | 27 | c-lord#241, c-lord#282 | One outbox with per-route pacing. A failed decoration never fails a turn. |
| Rendering helpers: table images, mockups, screenshots | 15 | c-lord#415, c-lord#1036 | Dropped. |
| Workspace setup and git mirrors | 12 | c-lord#477, c-lord#1134 | Kept, with setup failures as a session state shown in the thread. |
| Other: staging scripts, docker tracking, tests, docs drift | 45 | c-lord#699, c-lord#612 | Mostly outside the product. |

Three lessons cut across the groups:

- **A fix for a guess is another guess.** About 80 of the 443 titles name an earlier issue they are the rest of, or a regression of. The folder-trust dialog alone has nine. Guessing from a screen cannot be finished; it has to be replaced.
- **One operation, several code paths.** Starting a session from chat, a schedule, a webhook and a skill command were four paths, and three of them forgot the tmux window (c-lord#621, c-lord#629, c-lord#762). Some commands skipped the authorization check that others had (c-lord#781). hataori has one path per operation, in the core.
- **Upstream changes break parsers silently.** One Claude Code release stopped every reply in every thread (c-lord#773). Code that reads agent output is tested against captures of real output, and an unknown message is logged and skipped, never a reason to stop.

## 3. How hataori talks to agents

This is the decision the rest of the design depends on.

### 3.1 What c-lord does

c-lord starts the interactive `claude` in a tmux window. Everything it sends is keystrokes (`tmux send-keys`: the prompt, Ctrl-C, menu answers, `/clear`). It reads replies from Claude Code's transcript file, polled twice a second, and reads the pane (`tmux capture-pane`) for menus, dialogs, limits, login state and "did my input go in". Every session runs with permission checks bypassed. Its own code names the trade: structured events are given up so that a person can `tmux attach` and see the live UI.

### 3.2 The options, against what c-lord relies on

| Need | Screen in tmux (c-lord) | Claude Code `-p` + `stream-json` | Codex `app-server` |
|---|---|---|---|
| Subscription login | Yes | **Yes.** `apiKeySource: "none"` with a claude.ai login and no API key set. Not with `--bare`. | **Yes.** `account/read` reports a ChatGPT login. |
| Long turns | Yes | **Yes.** One turn of 881 s completed. A heartbeat event arrives every 30 s while a tool runs. Hours-long turns **(unverified)**. | **Yes.** One turn of 889 s completed. Hours-long turns **(unverified)**. |
| Permission prompts | Bypassed. Menus are scraped. | **Yes.** A `can_use_tool` request with the tool and its input; the answer is allow or deny. Deny and allow both checked. | **Yes.** `item/commandExecution/requestApproval`; decline and accept both checked. |
| Questions to the user | Scraped from the pane | **Yes.** `AskUserQuestion` arrives as a `can_use_tool` request with questions and options; the answer goes back in the reply. | Arrived as an agent message with a `questions` field; answered by a normal message. The `item/tool/requestUserInput` request exists in the schema but was not seen. |
| Interrupt | Ctrl-C keystroke | **Yes.** An `interrupt` request ended the turn (`error_during_execution`), the running command was killed, and the next message worked in the same process. | **Partly.** `turn/interrupt` ended the turn as `interrupted`, but the running command was still alive 12 s later. hataori must kill it. |
| Input during a turn | Interrupts the turn | A second message was taken into the running turn; one result. | `turn/steer` did the same. |
| Resume | `--resume` | **Yes.** `--resume <id>` in a new process kept the conversation. | **Yes.** `thread/resume` in a new process kept it. |
| Working directory | Window's directory | Process working directory | `thread/start` takes `cwd`; honoured. |
| Attach by hand | `tmux attach`, live, any time | Take over between turns: `claude --resume <id>` in a terminal showed the history; a turn typed there was known to the next headless resume. Not live. | `codex resume --remote <socket>` opened the thread from a running app-server. Whether a terminal and hataori can both drive it live is **(unverified)**. |
| Turns the agent starts itself | Inferred from the transcript | A background task's end produced a `task_notification` and a new turn with its own result, with no input from hataori. | Not checked. |
| Context size, model switch | `/context` typed into the pane | `get_context_usage`, `set_model`, `set_permission_mode` requests answered. | `thread/tokenUsage/updated`, `account/rateLimits/read`. |
| Final answer vs progress text | Guessed | The `result` message closes the turn and carries the final text. | Agent messages carry `phase`: `commentary` or `final_answer`. |

The interactive path also showed the folder-trust dialog, with the cursor on "No, exit". `-p` does not show it. That dialog is behind nine c-lord bug issues.

### 3.3 The Agent SDK

The Claude Agent SDK exists for Python and TypeScript only. Its docs tell other languages to run the CLI with `-p`, and describe the SDK as a library that runs the Claude Code binary. So from Go, the interface *is* `claude -p` with `stream-json`, and hataori speaks the same messages the SDKs do. The same docs say that third-party developers may not offer claude.ai login for their products without approval. hataori does not handle login at all: it runs the CLI that the operator installed and logged in to. Whether that reading is right is the maintainer's call (question 1).

### 3.4 Recommendation

**Claude Code:** `claude -p --input-format stream-json --output-format stream-json --verbose --permission-prompt-tool stdio`, with `--session-id` on first start and `--resume` after. One long-lived process per session; each user message is one line on stdin.

**Codex:** `codex app-server` over stdio, one process per session, `thread/start` with `cwd`, then `turn/start`, `turn/steer`, `turn/interrupt`, `thread/resume`.

**Attach by hand:** `hataori attach <thread>` on the host. It waits for the turn to end, stops hataori's process for that session, and opens the same conversation in the agent's own terminal UI in the workspace. When the person exits, hataori resumes the session. `hataori tail <thread>` shows the live event log, read-only. This replaces `tmux attach`. A person can no longer type into a session while it is running a turn for chat.

### 3.5 Risks of this choice

- **`--bare` as the future default for `-p`.** Claude Code's docs say `--bare` "will become the default for `-p` in a future release". With `--bare`, the same login gave "Not logged in". If that default arrives without a way to opt out, this adapter loses the subscription login. Mitigation: the launch command is one function in the Claude Code adapter; the adapter interface (section 5) does not assume `-p`, so a screen-driven or background-session adapter can replace it without touching the core. `claude --bg` with `claude attach` exists in this CLI version and was not evaluated **(unverified)**.
- **The control messages are not a documented wire format** as far as we found **(unverified)**; they are what the SDKs exchange with the CLI. `--permission-prompt-tool stdio` is not in `claude --help`. Mitigation: decode against captures per CLI version, skip unknown messages, and run the capture suite before allowing a new CLI version in production.
- **Codex `app-server` is marked experimental** in its own help text. Same mitigation.
- **Codex interrupt leaves the command running** (observed). The session host runs the agent in its own process group so hataori can end what the agent left behind.

## 4. Core model and lifecycle

### 4.1 Three objects

- **Thread:** a conversation on a chat surface. Identified by `(surface, channel, thread)`.
- **Session:** one agent conversation, identified by the agent's own session id, and the process that currently serves it (or none).
- **Workspace:** one directory, a clone of one repository, owned by exactly one thread.

A thread has one workspace for its whole life and one current session. Clearing the conversation makes a new session in the same workspace.

### 4.2 Session states

```
creating ──► idle ◄──► running ◄──► waiting      (waiting: a question or approval is open)
   │          │           │
   ▼          ▼           ▼
 failed     asleep ──► idle        (asleep: no process; the next message resumes it)
              │
              ▼
           closed ──► removed      (closed: stopped by a person or by idle age; workspace kept)
```

Every transition is caused by one event (a chat message, an agent event, a timer, a command), is written to the database in the same transaction as its effects (rows in the outbox), and is the only place that state changes. Lamps, the progress line, pings and turn-end events are all derived from the transition, so they cannot disagree with each other.

### 4.3 The session host

Each agent process is started by a small host process (`hataori host`, the same binary). The host:

- starts the agent in its own process group, in the workspace;
- appends every line the agent writes to `events.ndjson` in the session's state directory;
- accepts input on a Unix socket and writes it to the agent's stdin;
- records the exit status when the agent ends.

The host does nothing else and does not parse anything, so it rarely needs to change. It is detached from the daemon. When the daemon restarts, running turns continue; the daemon reconnects to the socket and reads the log from the byte offset it last stored.

Checked: a `claude -p` process with stdin on a FIFO and stdout on a file finished its turn after the process that launched it had exited, and accepted a second message afterwards. The host itself is new code and is the first thing Phase 1 must prove.

### 4.4 Running a turn

1. The surface adapter delivers an inbound message. The core stores it in the **inbox**, keyed by the surface's message id. A message already in the inbox is ignored. This is what makes "run exactly once" true across reconnects and backfill.
2. The core authorizes the sender. One function, called for messages, commands and button presses alike.
3. The core finds or creates the thread, workspace and session, and sends the input. If a turn is running, the input is delivered into it (question 3).
4. The agent adapter decodes log lines into events. Each event has `(session, sequence number)`.
5. For each event the core updates state and writes rows to the **outbox**: a reply, an edit, a lamp change. A row is keyed by `(session, sequence, part)`, so replaying the log after a crash writes nothing twice.
6. The outbox sender posts rows in order per thread, paced per route, and stores the surface's message id on success. Rows marked as decoration (lamps, progress line, thread rename) are dropped after a few failures and never block a reply.

One gap remains: if the daemon dies after the surface accepted a post but before the id was stored, the post is sent again. An adapter closes the gap where its surface offers an idempotency key (Discord's message nonce, **unverified**).

Questions and approvals are rows too: the agent's request id, the question, the message that shows it. A button carries the request id. After a restart the row is still there, so the button still works (c-lord#671, c-lord#752).

### 4.5 Sleep, wake, restart

- **Sleep:** after an idle period with no running turn, no open request and no background task, the core stops the agent process. The session keeps its id. A cap on resident processes sleeps the longest-idle ones first.
- **Wake:** the next message starts the agent with resume. If the agent no longer has the conversation (its own retention removed it), the thread is told so, and a new session starts in the same workspace with the thread's recent messages as context.
- **Daemon restart:** nothing happens to sessions. The daemon reads each log from its stored offset and asks the surface for messages since its stored cursor.
- **Host reboot:** all processes are gone. Sessions that were `running` become `asleep` with reason `interrupted`; the thread gets one notice; the next message resumes.
- **Closed:** stopped by a person or by idle age. The workspace stays. A message does not wake a thread a person closed; a command does.

### 4.6 Cleanup without losing work

hataori never writes into a working tree: no injected skills, no history files, no attachments. Those go to the session's state directory and reach the agent by flags (extra directory, appended instructions, plugin directory). c-lord's injected file once made every workspace look dirty, so nothing was reclaimed (c-lord#749), and once got committed (c-lord#779).

A workspace is removed only when all of this is proven, and removal is refused when any step cannot be answered (error, timeout, unreadable directory):

1. No modified, staged or untracked files. Ignored files count as work unless they match a pattern the instance lists as disposable.
2. Every commit reachable from any ref, from `HEAD`, from the stash and from the reflog is on a remote. The remote is asked at that moment; remote-tracking refs are not trusted (c-lord#1245). A remote that is itself a workspace, or lies under any hataori root, does not count (c-lord#1265, c-lord#1289).
3. The same for every submodule, including ones with no checkout whose data is still under `.git/modules` (c-lord#1198), and for every nested repository (c-lord#1241).
4. No process is using the directory.

Removal is a rename into `trash/` inside the root. A later pass deletes from trash after a retention period. This is the only code in hataori that deletes user data, and it is tested case by case before anything else.

## 5. Interfaces

The core imports neither adapter package. Adapters register themselves under a name, and that name is used only as a key and a path segment.

```go
// Package surface: a chat service.

type Adapter interface {
	// Run connects and delivers inbound events to sink until ctx ends.
	// It reconnects by itself and calls sink in order per conversation.
	Run(ctx context.Context, sink Sink) error

	// Since returns the messages in a conversation after cursor,
	// for catching up after an outage.
	Since(ctx context.Context, conv Conversation, cursor string) ([]Inbound, error)

	OpenThread(ctx context.Context, parent Conversation, title string) (Conversation, error)
	Post(ctx context.Context, conv Conversation, key string, m Message) (MessageRef, error)
	Edit(ctx context.Context, ref MessageRef, m Message) error
	Delete(ctx context.Context, ref MessageRef) error
	SetStatus(ctx context.Context, ref MessageRef, s Status) error        // lamp on a message
	SetThread(ctx context.Context, conv Conversation, t ThreadMeta) error // title, archived

	Limits() Limits // message length, choices per message, which optional calls work
}

type Sink interface {
	Message(Inbound)  // text, files, author, ids
	Choice(Selection) // a button or menu was used
	Command(Command)  // a slash or text command
}

type Message struct {
	Text    string
	Files   []File
	Choices []Choice    // rendered as buttons or a menu; each carries an opaque token
	ReplyTo *MessageRef // quote-reply
	Notify  []Principal // people to ping
	Quiet   bool        // no notification
}
```

`Message` says what to show, not how. The adapter splits long text, picks buttons or a menu, and maps `Status` to whatever the surface has (a reaction, an edit). `Limits` lets the core avoid asking for what a surface cannot do.

```go
// Package agent: a coding agent.

type Adapter interface {
	// Probe reports whether the agent is installed and logged in.
	Probe(ctx context.Context) (Health, error)

	// Command returns how to start the agent for spec: a new session,
	// or a resumed one when spec.ResumeID is set.
	Command(spec Spec) (argv []string, env []string)

	// Interactive returns the command a person runs to take the session over
	// in the agent's own terminal UI.
	Interactive(spec Spec) (argv []string)

	// NewCodec returns the translator for one running process.
	NewCodec(spec Spec) Codec
}

// Codec turns the agent's output into events and requests into the agent's input.
// It does no I/O, so it is tested against captured output.
type Codec interface {
	Open() [][]byte                                      // bytes to send first (handshake), if any
	Decode(line []byte) (events []Event, reply [][]byte) // one output line
	Input(in Input) ([][]byte, error)                    // a user message, during a turn or not
	Answer(a Answer) ([][]byte, error)                   // to an approval or a question
	Interrupt() [][]byte
}

type Spec struct {
	Dir          string // workspace
	StateDir     string // attachments, extra instructions; outside the workspace
	ResumeID     string
	Model        string
	Instructions string // appended to the agent's own
	Approvals    Policy // bypass, or ask in chat
}

// Event is one thing the agent did. Kind is one of SessionReady, TurnStarted,
// Text, ToolStarted, ToolProgress, ToolEnded, Request, RequestWithdrawn,
// FileOut, Usage, Limit, TurnEnded, Unknown.
type Event struct {
	Seq  uint64
	Kind Kind
	// Plus the fields each kind needs: the text and whether it is final,
	// the parent tool call, the request id and its questions,
	// the end status and reason.
}
```

Splitting the agent adapter into a command line and a pure codec is what makes the repository's test rule practical: a capture of real output goes in `testdata/`, and the codec must turn it into the expected events. The session host and the core never see agent-specific bytes.

`TurnEnded` carries a status (`completed`, `interrupted`, `failed`) and a reason (`limit`, `login`, `api_error`, `crashed`). `TurnStarted` says who started it (a person, or the agent itself after a background task). These two fields replace most of the "guessing turn state" group.

## 6. Storage and layout

### 6.1 One root per instance

```
<root>/
├── bin/          one binary per version, and a link to the one in use
├── config/       this instance's config and secrets (private; never linked from another instance)
├── state/
│   ├── hataori.db            SQLite, WAL, one writer (the daemon)
│   ├── log/
│   └── sessions/<id>/        events.ndjson, host socket, attachments, extra instructions
├── workspaces/<surface>/<channel>/<thread>/
├── mirrors/      one bare mirror per remote
└── trash/        removed workspaces, kept for the retention period
```

An instance reads and writes only under its own root. A staging instance is another root with another bot identity. At start the daemon takes a lock in `state/`, checks that the identity it logged in as is the one in its config, and exits if either fails (c-lord#323, c-lord#325).

Unix socket paths are limited to about 100 bytes, so the socket path must stay short however deep the root is; if `state/sessions/<id>/` is too long the host uses a short path under the instance's runtime directory and records it.

### 6.2 SQLite

One database file. A schema version table and forward-only migrations (c-lord has no schema version). Main tables:

| Table | Holds |
|---|---|
| `threads` | `(surface, channel, thread)`, short number, title, owner, state |
| `workspaces` | path, remote, state |
| `sessions` | agent name, agent session id, state, model, host pid, log offset, last sequence |
| `turns` | who started it, the message that triggered it, start, end, status, reason |
| `inbox` | inbound messages by surface message id; the catch-up cursor per conversation |
| `outbox` | pending and sent posts by `(session, sequence, part)`, with the surface message id |
| `requests` | open questions and approvals, their message, their answer |
| `bindings` | channel to repository, and per-thread overrides |
| `settings` | model and other choices per scope |

Who is allowed is in the config file, not in the database.

### 6.3 Workspaces and mirrors

A workspace is a full clone per thread. To avoid downloading a repository for every thread, hataori keeps one bare mirror per remote, fetches it before a clone, and clones from it. The rule: **a workspace stays valid if its mirror is deleted.** So the clone takes its objects by hard link or copy, not by a reference to the mirror's object store, and its `origin` points at the real remote. (c-lord references the mirror's objects and protects them by never pruning the mirror.) A mirror that cannot be fetched in time is used as it is, or skipped; it never blocks a thread.

### 6.4 A second host

A second host (for example a macOS machine) is another instance: its own root, database, agent logins and bot identity. Hosts share nothing and do not coordinate. A channel belongs to one instance, by config. Which bot answers tells you which host the work is on. Moving a thread between hosts is not supported; push the branch and start a thread on the other host.

For macOS specifically: the service manager is launchd, not systemd; the host process must not depend on `/proc`; Claude Code keeps its login in the Keychain there, which a background service may not be able to read **(unverified)**; the default filesystem is case-insensitive, which matters for workspace paths. None of this was run.

## 7. Phases

Each criterion is met or not. "Core packages" means everything except the adapter packages and `cmd/`.

### Phase 1: Discord and Claude Code, enough for c-lord's daily use

- [ ] Captures of real Claude Code output for every scenario in Appendix B are in `testdata/`, and the codec turns each into the expected events.
- [ ] A command or text trigger in a bound channel creates a thread, a clone under `workspaces/discord/<channel>/<thread>/`, and a reply in the thread.
- [ ] A second message in the thread continues the same agent session id.
- [ ] A turn of 30 minutes ends with its final answer posted once and the sender pinged once.
- [ ] A question from the agent appears with buttons; pressing one delivers the answer. The same works when the daemon was restarted between question and answer.
- [ ] Stop ends a running command within 10 seconds and leaves no process of the agent's behind; the next message works.
- [ ] The daemon is killed with SIGKILL during a turn and started again: the turn finishes, and every reply appears exactly once.
- [ ] Three messages posted to a thread while the daemon is down are run once each, in order, after it starts.
- [ ] After the idle period the agent process is gone; the next message resumes the same conversation.
- [ ] Removal refuses, with the reason, for each of: modified file, untracked file, ignored file not listed as disposable, unpushed commit, stash, commit only in the reflog, submodule with an unpushed commit, nested repository, remote that is another workspace, git error. It removes a clean, pushed workspace. Each test fails when its check is removed.
- [ ] `hataori attach` opens the conversation in the agent's terminal UI, and a turn typed there is known to the next turn from chat.
- [ ] A message, a command and a button press from a user who is not allowed each do nothing and are logged.
- [ ] A usage limit and an expired login each end the turn with one specific message in the thread and one notice to the owner.
- [ ] One turn-end event (completion, blocked or failure) per turn reaches the configured outlet.
- [ ] Two instances with different roots on one host each ignore the other's channels.
- [ ] No core package imports an adapter package or contains the name of a surface or an agent (checked in CI).
- [ ] The end-to-end test with a fake surface and a fake agent opens a thread, runs a turn and sees it finish.
- [ ] The maintainer has used it for two weeks on a real project channel without going back to c-lord.

### Phase 2: Codex

- [ ] A channel or thread can be set to Codex; start, continue, sleep and wake work as in Phase 1.
- [ ] Approvals and questions from Codex appear in the thread and can be answered.
- [ ] After an interrupt, no command started by that turn is still running.
- [ ] A turn of 30 minutes completes.
- [ ] Captures of real `app-server` output are in `testdata/`, and the codec passes on them.
- [ ] The change touches no core package except the line that registers the adapter.

### Phase 3: Slack

Go has no official Slack SDK (official ones are JavaScript, Python and Java); a community library with Socket Mode is assumed **(unverified)**. Socket Mode needs no inbound endpoint.

- [ ] Phase 1's criteria for start, continue, questions, stop, restart and catch-up pass on Slack.
- [ ] Interactions are acknowledged within Slack's deadline while the work continues.
- [ ] The change touches no core package except the line that registers the adapter.

### Phase 4: Microsoft Teams

Microsoft's bot SDKs are C#, TypeScript and Python; the older Bot Framework SDK is archived. There is nothing for Go, so the adapter is written against the HTTP protocol. By hand **(the list is from memory, unverified)**:

- an HTTPS endpoint that receives activities, and validation of the signed token on each one;
- getting and refreshing the service token for outbound calls;
- the activity types hataori needs: message, message update, invoke (button press), conversation update;
- Adaptive Cards for choices;
- conversation references, so hataori can post without an inbound activity;
- file upload and download;
- pacing and retry.

Unlike Discord and Slack Socket Mode, Teams calls hataori. The host needs a public HTTPS address, which is a deployment decision.

- [ ] Phase 1's criteria for start, continue, questions, stop, restart and catch-up pass on Teams.
- [ ] An activity with an invalid or missing token is rejected.
- [ ] The change touches no core package except the line that registers the adapter.

## 8. Open questions for the maintainer

1. **Do we accept depending on `claude -p` keeping the subscription login?** It works today. The docs announce `--bare` as a future default, and `--bare` drops the login; the Agent SDK docs restrict third parties from offering claude.ai login. *Recommendation:* go ahead. hataori never handles the login, the dependency is isolated in one adapter, and the alternative is the screen-driving that produced half of c-lord's bugs. State in the README that hataori runs the operator's own CLI and login.
2. **Is take-over between turns enough, in place of `tmux attach`?** *Recommendation:* yes. `hataori attach` for hands-on work, `hataori tail` to watch.
3. **A message arrives while a turn runs: interrupt it (c-lord) or deliver it into the turn?** Both agents accept input mid-turn. *Recommendation:* deliver it into the turn; Stop is the explicit way to interrupt.
4. **Permission checks.** c-lord bypasses them in every session. *Recommendation:* keep bypass as the default in Phase 1 so daily use does not change, and add "ask in chat" as a per-channel setting afterwards; the events for it already exist.
5. **Is dropping these acceptable?** The HTTP API (replaced by the `hataori` command and webhook triggers), scheduled tasks and notifications, docker container tracking, table images, the status board. *Recommendation:* drop them all for Phase 1; bring scheduled tasks back first if they are missed.
6. **One bot identity per instance, including a second host?** *Recommendation:* yes. It is the only arrangement in which two instances cannot both act on one message.
7. **Do existing c-lord threads move to hataori?** *Recommendation:* no. Run both side by side on different channels; let c-lord threads finish where they are.

## Appendix A: c-lord feature inventory

Compiled from c-lord's README, `docs/`, `docs/specs/` and code. A reading agent collected the facts; the bridge facts in section 3.1 were re-checked in code, the rest of the rows were not re-checked one by one. Features that c-lord documents but does not run (tool-call embeds, permission buttons, starting a session by posting in the main channel, automatic resume after restart) are left out.

### Starting a session

| Feature | Verdict | Why |
|---|---|---|
| Thread = session = working directory | keep | The core model. |
| Slash command starts a thread in a bound channel | keep | Daily entry point. |
| Text trigger (`!…`) and webhook post start a thread | keep | How other tools and agents dispatch work. |
| Per-thread repository (`repo:<url>`) | keep | Needed for work outside the channel's repository. |
| Only threads hataori created are sessions | keep | Prevents adopting ordinary conversations (c-lord#551). |
| Clear error when the channel is unbound or the bot lacks permissions | keep | A silent start failure looks like a hang. |
| Run a skill as a command (`/skill`) | change | Becomes an ordinary first message beginning with the skill's name; no separate launch path. |
| Attach a thread to a hand-made tmux window | drop | No tmux. |
| Programmatic spawn over HTTP | change | `hataori spawn` on the host, or the text trigger through a webhook. |
| CI webhook trigger with a fixed server-side prompt | drop | Not wired in c-lord's default setup; the text trigger covers it. |

### Input and turns

| Feature | Verdict | Why |
|---|---|---|
| A message in the thread is the next prompt | keep | |
| A new message interrupts the running turn | change | Delivered into the running turn instead (question 3). |
| Stop button and stop command | keep | Now an interrupt request plus killing the turn's process group. |
| Messages missed during an outage or restart are run once | keep | Built into the inbox instead of added later. |
| Limit on turns running at once | keep | Needed on a shared host. |
| Trusted bots and webhooks may post into existing threads | keep | Agents talk to agents this way. |
| Pasted chat links and replies are expanded into context | keep | Small, used daily. |
| Input checked on screen after typing; vim mode, paste folding, long-input splitting | drop | No typing. |
| Post into a thread over HTTP | change | `hataori send`. |

### Output

| Feature | Verdict | Why |
|---|---|---|
| Progress text posted quietly, final answer as a notifying reply | keep | Now decided by the agent's own "final" marker. |
| Long replies split without breaking code blocks | keep | Done by the surface adapter, which knows the limit. |
| `progress.txt` with the turn's tool calls attached to the answer | keep | Built from tool events. |
| Text typed by a person in the pane is shown in the thread | change | Applies to turns typed during `hataori attach`. |
| Hiding of internal harness text (reminders, notifications, slash-command records) | drop | The event stream separates them; nothing to scrub. |
| Repeated identical lines folded into a counter | keep | A looping agent can still flood a thread. |
| Subagent start and end lines with an attached record | change | One line each, from events that carry the parent tool call. |
| Named subagent "voices" through webhooks | drop | Opt-in; caused a class of bugs of its own. |
| Context-compaction notice | keep | From the agent's event. |
| API error shown once, shortened | keep | From the turn's end reason. |
| Table images | drop | Opt-in extra. |
| Link previews suppressed | keep | Surface adapter option. |

### Questions and prompts

| Feature | Verdict | Why |
|---|---|---|
| Agent questions as buttons or a menu, with free-text answer | keep | From the structured request. |
| Multi-select questions | keep | Same request. |
| Plan approval as a menu | keep | Same mechanism **(not checked against the CLI)**. |
| Ping when input is needed | keep | |
| Buttons work after a restart; old cards are disarmed | keep | Requests are rows; nothing to recover. |
| Typing a sentence instead of pressing a button | keep | An open request takes the next message as its answer. |
| Tool permission prompts auto-accepted | change | Default stays bypass; "ask in chat" becomes possible (question 4). |
| Folder-trust dialog handling | drop | `-p` shows no dialog. |
| Unknown-prompt safety net, menu watchdog, dead-pane guard | drop | No screen. |
| Usage-limit menu answered with "wait" | drop | The limit is an event, not a menu. |

### Status and visibility

| Feature | Verdict | Why |
|---|---|---|
| Lamp on the triggering message (working, done, error) | keep | Derived from the state machine. |
| Stall lamps after 10 s and 30 s of silence | change | Driven by the tool heartbeat and process liveness, not by file silence. |
| Quiet-gap progress line, edited in place | keep | From tool events. |
| Turn footer: context use, model, versions | keep | From usage events and `get_context_usage`. |
| Thread name `W<N> │ #issue topic` | change | The tmux window number becomes a short thread number that `hataori attach` accepts. |
| Issue or pull request number in the thread name; manual rename wins | keep | |
| Thread-name lamp, automatic topic, re-title | drop | Opt-in in c-lord; a source of rate limiting. |
| Status command with a table of sessions | keep | From the database. |
| Pinned status board | drop | Opt-in in c-lord (question 5). |
| Pane screenshot, window list, resync | drop | No pane. |
| Coordination-channel notices | drop | Unused extra. |

### Lifecycle

| Feature | Verdict | Why |
|---|---|---|
| Agent stays resident after a turn | keep | Fast follow-ups. |
| Sleep after idle hours; silent wake on the next message | keep | Explicit states. |
| Cap on resident sessions from host memory; memory brake | keep | Simplified to counting processes. |
| Sleep never interrupts work, including background tasks | keep | Background tasks are events now (c-lord#922). |
| Manual stop, start and delete of a workspace | keep | |
| Automatic stop after idle days; closed threads renamed and archived | keep | |
| Automatic delete at the agent's transcript-retention age | change | Moves to trash first, behind the proof in section 4.6. |
| Running turns survive a bot restart | change | By the session host, not by tmux. |
| Notice when the agent was found stopped mid-turn | keep | Only after a host reboot now. |
| Clear and compact the conversation | keep | Compact through the agent **(unverified in `-p`)**; clear is a new session. |
| Restart the agent process, keep the conversation | keep | Stop and resume. |
| Reattach a thread whose record was lost | drop | Records are not swept away while a workspace exists. |
| Drain before an upgrade restart | drop | Restarts no longer stop turns. |

### Workspaces and git

| Feature | Verdict | Why |
|---|---|---|
| Channel-to-repository binding; per-thread override | keep | |
| One full clone per thread | keep | Isolation is the point. |
| Shared bare mirrors | change | Clone must survive the mirror's deletion (section 6.3). |
| Repository URL normalisation and validation | keep | With errors at bind time (c-lord#1135). |
| Setup failure shown in the thread with git's error | keep | A session state, not an exception. |
| Requester as co-author on commits | keep | Uses a git hook under `.git/`, not in the working tree. |
| Delete refuses to lose work | change | One proof, stricter (section 4.6). |
| Orphan directory sweep | change | Same proof; unknown directories are reported, not removed. |
| Skill and history files written into the checkout | change | Passed from outside the working tree. |
| Docker containers per thread tracked and listed | drop | Deployment-specific (question 5). |

### Files

| Feature | Verdict | Why |
|---|---|---|
| Chat attachments handed to the agent as files | change | Stored in the session's state directory, not the checkout. |
| Limits and named reasons for skipped attachments | keep | |
| Files the agent hands over are posted to the thread | keep | From the tool event; Codex needs its own way. |

### Scheduling, notifications, events

| Feature | Verdict | Why |
|---|---|---|
| Turn-end ping to the requester | keep | |
| Turn-end events (completion, blocked, failure) to a log or signed webhook | keep | The contract that orchestration builds on. |
| Events for turns the agent starts itself | keep | `TurnStarted` says who started it. |
| Completion held while background subagents run | keep | From task events instead of a side file. |
| Scheduled tasks | drop | Not in Phase 1 (question 5). |
| Scheduled and immediate notifications, "lounge" notes | drop | A second product inside the bot. |

### Models, limits, login

| Feature | Verdict | Why |
|---|---|---|
| Model and effort per thread, channel and instance | keep | Applied by a request to the running session or at the next start. |
| Usage limit shown with its reset time | keep | From the rate-limit event, not from screen text. |
| Messages during a limit are accepted | keep | |
| Owner told once when the agent is logged out | keep | From `Probe` and the turn's end reason. |
| Logging in again from a direct message | drop | Drove a login screen; log in on the host. |

### Authorization and security

| Feature | Verdict | Why |
|---|---|---|
| Owner-only by default; allow-list by user or role; explicit switch to open | keep | |
| One rule for messages, commands and buttons | keep | One function in the core. |
| Secrets removed from the agent's environment | keep | The host starts the agent with a built environment, not an inherited one. |
| Identity and single-instance checks at start | keep | |
| Chat-reading skill given to every session | change | Provided from outside the working tree; each surface adapter supplies its own. |
| Local HTTP API with peer check | drop | Replaced by the `hataori` command over a Unix socket. |

### Operating

| Feature | Verdict | Why |
|---|---|---|
| Several instances on one host | keep | One root each. |
| Config validated at start with the key named | keep | Typed config, parsed once. |
| Setup wizard | change | `hataori init` writes a config file to edit; no interactive token entry. |
| Service unit and staging scripts | drop | Belong to a deployment, not to this repository. |
| macOS | keep | A goal; not run yet. |
| Use as a library; auto-upgrade | drop | One binary. |
| Text twins of commands | change | Only the ones a webhook needs: start, stop, status. |
| Old command names kept as aliases | drop | New product, one name each. |
| Fuzz harness, traffic monitor, evidence capture, mockup renderer | drop | Tooling around c-lord, not features. |
| Localised READMEs | drop | English only. |

## Appendix B: checks against the real CLIs

Claude Code 2.1.322 and codex-cli 0.163.0, Linux, 2026-11-12. Each agent was started by a script with a minimal environment (no API key variables) and driven over stdin and stdout. Cheap models were used. The captures are not in this change; Phase 1 records them again into `testdata/`.

### Claude Code: `claude -p --input-format stream-json --output-format stream-json --verbose --permission-prompt-tool stdio`

| Check | Observed |
|---|---|
| Login | `claude auth status`: `authMethod: "claude.ai"`. The `system/init` message: `apiKeySource: "none"`. |
| Two turns in one process | Second message on stdin answered with the first turn's context. |
| Permission prompt | In the default permission mode, a file write produced `control_request` / `can_use_tool` with the tool name and input. A deny reply was reported to the model as a refused tool call; an allow reply ran the tool. |
| Question | `AskUserQuestion` produced `can_use_tool` with questions and options. Replying with the input plus an `answers` map delivered the choice. |
| Interrupt | `control_request` / `interrupt` during a running command: `result` with `error_during_execution` within a second; the command's process was gone 2 s later; the next message worked in the same process. An interrupt while a permission request was open withdrew the request (`control_cancel_request`). |
| Message during a turn | Taken into the running turn; one `result`. |
| Turn started by the agent | After a background command ended: `task_notification`, then a new `init`, reply and `result`, with nothing sent by the driver. |
| Long turn | One turn of 881 s (three commands of about 290 s). 27 `tool_progress` messages, one per 30 s. |
| Resume | A new process with `--resume <id>` answered from the old conversation. |
| Take-over by hand | `claude --resume <id>` in a terminal showed the headless turns. It first showed the folder-trust dialog with the cursor on "No, exit". A turn typed there was known to a later headless `--resume`. |
| Control requests | `get_context_usage`, `set_model`, `set_permission_mode`, `mcp_status` returned success; an unknown subtype returned an error. `/context` sent as a message returned a usage table. |
| Usage information | `rate_limit_event` with status, window and reset time; `result` with token usage, context window and cost. |
| Detached process | Started with stdin on a FIFO and stdout on a file; the launching process exited; the turn completed, and a second message through the FIFO was answered. |
| `--bare` | With the same login and no API key: `Not logged in · Please run /login`. |

Not checked: turns of hours; what the stream shows at a real usage limit or an expired login; `/compact` in `-p`; plan approval; two processes on one session at once.

### Codex: `codex app-server` (stdio, one JSON object per line)

The method list comes from `codex app-server generate-json-schema` of the installed binary.

| Check | Observed |
|---|---|
| Login | `account/read`: account type `chatgpt`. `account/rateLimits/read` returned both usage windows. |
| Working directory | `thread/start` with `cwd`: the thread reported that directory and commands ran in it. |
| Approval | With `approvalPolicy: "untrusted"`, a command produced `item/commandExecution/requestApproval`. `decline` was reported to the model; `accept` ran the command. |
| Interrupt | `turn/interrupt`: `turn/completed` with `status: "interrupted"` at once. The command and its sandbox wrapper were still running 12 s later, and were gone after the app-server exited. The next `turn/start` worked. |
| Message during a turn | `turn/steer` with the running turn's id was taken into the turn. |
| Long turn | One turn of 889 s. |
| Resume | A new app-server with `thread/resume` answered from the old conversation. |
| Question | The model's question arrived as an agent message with `questions` and `delivery: "async"`, and the turn went on waiting. No `item/tool/requestUserInput` request was seen. |
| Final answer | Agent messages carry `phase`: `commentary` or `final_answer`. |
| Second client | With `--listen unix://<path>` (the socket's directory must be private to the user), `codex resume --remote unix://<path> <thread>` showed the thread in the terminal UI. A raw client through `app-server proxy` got no reply to `initialize`; the framing it needs was not found. |

Not checked: turns of hours; behaviour at a usage limit; two clients driving one thread; turns the agent starts itself.

### Documentation read on 2026-11-12

- Agent SDK overview: Python and TypeScript only; other languages run the CLI with `-p`; the note on third parties and claude.ai login.
- "Run Claude Code programmatically": `--bare` does not read the subscription login and "will become the default for `-p` in a future release"; on SIGTERM the turn is left unfinished and running commands are terminated.

## Appendix C: c-lord bug issues by group

All numbers are `yousan/c-lord` issues.

- **Reading the screen (52):** 153 156 165 166 169 172 179 180 184 219 222 251 252 262 263 292 359 388 399 412 418 453 468 480 485 510 535 536 549 579 611 630 631 633 650 657 684 686 695 707 746 804 812 858 911 915 921 925 989 990 993 1046
- **Typing into the terminal (22):** 76 147 171 358 527 529 544 560 651 674 716 809 861 872 879 924 1076 1132 1247 1263 1283 1284
- **tmux window bookkeeping (32):** 19 37 111 113 158 187 270 410 420 427 438 474 570 595 600 608 617 621 629 649 677 701 762 790 986 1060 1085 1095 1112 1120 1193 1236
- **Tailing the transcript (39):** 143 215 218 379 380 433 487 530 542 553 627 628 680 682 719 747 755 773 808 834 864 914 926 970 987 988 1041 1043 1044 1059 1067 1102 1125 1143 1186 1187 1188 1191 1216
- **Guessing turn state (64):** 22 190 213 236 293 365 541 562 565 583 718 751 757 769 799 800 815 870 871 876 878 895 905 906 908 909 912 944 961 967 977 978 979 992 1040 1092 1093 1099 1101 1131 1137 1138 1169 1170 1178 1179 1180 1189 1190 1212 1234 1235 1251 1254 1260 1261 1262 1270 1271 1285 1294 1303 1306 1307
- **Subagents and background tasks (20):** 913 916 1042 1077 1078 1079 1104 1110 1136 1139 1144 1177 1249 1281 1282 1296 1302 1304 1305 1308
- **Restart and reconnect (17):** 149 406 537 671 672 717 742 745 752 776 786 796 877 907 920 954 1248
- **Sleep, wake, resident cap (22):** 123 538 604 642 685 803 806 896 922 964 991 994 1033 1034 1086 1094 1142 1171 1192 1213 1214 1244
- **Cleanup (22):** 554 749 779 882 902 903 1045 1080 1121 1181 1198 1241 1242 1245 1246 1250 1265 1269 1286 1289 1295 1297
- **Authorization and isolation (22):** 323 324 325 347 353 405 457 466 507 508 522 556 596 713 739 781 811 1075 1108 1109 1140 1176
- **Config and input validation (32):** 1029 1030 1031 1037 1038 1039 1062 1063 1064 1065 1066 1081 1082 1083 1084 1105 1106 1107 1119 1183 1185 1194 1237 1238 1239 1240 1255 1256 1258 1274 1287 1288
- **Chat API limits (27):** 75 144 235 241 246 266 277 281 282 414 423 443 460 462 593 632 720 721 748 754 824 856 857 904 910 930 1114
- **Rendering helpers (15):** 210 408 415 588 607 623 664 683 750 1035 1036 1087 1096 1097 1175
- **Workspace setup and mirrors (12):** 366 476 477 681 923 1116 1133 1134 1135 1173 1174 1268
- **Other (45):** 3 61 65 66 98 148 154 164 183 186 233 331 341 397 401 428 432 436 437 491 492 520 523 528 543 551 559 612 616 619 634 678 687 693 699 810 836 880 881 927 976 1100 1103 1113 1182
