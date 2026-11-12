# hataori design overview

Status: proposal for review. Nothing here is implemented yet.

hataori succeeds [c-lord](https://github.com/yousan/c-lord). It keeps c-lord's model: one chat thread is one agent session, and one agent session owns one working directory. This document says what hataori takes from c-lord, what it changes, and how it is built.

How to read it:

- Issue numbers written `c-lord#123` are issues of `yousan/c-lord`.
- c-lord facts come from its docs and code at v1.4.420.
- Claims about the agent CLIs were checked on 2026-11-12 against Claude Code 2.1.322 and codex-cli 0.163.0 on Linux, by driving the real programs. Appendix B lists each check.
- Section 3 first recommended `claude -p`. The maintainer decided against it; sections 3.4 and 3.5 describe the path chosen instead, and the earlier checks stay in the document as the record.
- OpenClaw facts (section 7.3) come from its documentation for version 2026.10.6, read on 2026-11-12.
- Anything marked **(unverified)** was not checked by running it.

## 1. Summary

1. **Learn what an agent is doing from what it reports, never from its screen.** Claude Code runs as the ordinary interactive `claude` in a terminal that hataori owns: turn start and end, questions and permission requests come from Claude Code hooks, text from the transcript the hooks point to, and input goes in by paste. Codex runs as `codex app-server` (JSON-RPC). hataori never touches login; whatever the installed CLI is logged in with works (section 3).
2. **This removes most of the causes behind half of c-lord's bugs.** Of 443 bug issues, 106 came from driving a terminal (reading it, typing into it, keeping track of tmux windows), and 123 from rebuilding output and turn state out of a transcript file and timers (section 2).
3. **A person can attach to a live session.** Because Claude Code runs in a real terminal, `hataori attach` can connect a person's terminal to it, to watch and to type, as `tmux attach` does today. That comes after phase 1 (section 3.4).
4. **Each agent runs under a small session host that outlives the hataori daemon.** The host owns the agent's terminal or pipes and writes what the agent reports to a file. Restarting or upgrading hataori does not stop running turns, and the file is read from a stored position, so nothing is lost or posted twice (section 4.3).
5. **Everything needed after a restart is in SQLite before it is acted on:** inbound messages, outbound posts, open questions, turn state (section 6).
6. **The core has one entry, the control API.** Chat adapters, a supervisor agent, a dashboard and plugins are all clients of it (sections 5 and 7). The first interface draft did not allow that; section 5.4 says what changed.
7. **hataori writes nothing into a working tree**, and removes a workspace only after proving there is nothing in it that exists nowhere else (section 4.6).
8. **Phase 1 is narrow:** replace c-lord for orders that a managing agent sends through a webhook, with the full test suite written before the first real order. Everything a person does by hand in chat comes after (section 8).
9. **Two things have no hook:** the folder-trust dialog at a workspace's first start, and the end of an interrupted turn. Section 3.5 says how each is handled; the first needs the maintainer's decision (section 10).

## 2. What c-lord taught us

### 2.1 Feature inventory

c-lord has grown to about 64,000 lines of Python and roughly 60 slash and text commands. Appendix A lists its user-visible features in 105 rows: 62 keep, 23 change, 20 drop.

What is kept is the daily loop: start a thread from a command, a text trigger or a webhook; get a clone of the bound repository; talk to the agent in the thread; answer its questions with buttons; see a lamp on your message; get pinged when the turn ends; stop, sleep, wake, close and clean up; hand files in and out; per-thread model choice; owner-only access by default; several instances on one host.

Most kept features keep their behaviour but not their mechanism: they are driven by an event from the agent instead of by text found on a screen or in a transcript. "Change" marks the features whose behaviour a user will notice changing: a message during a turn no longer interrupts it, nothing is written into the checkout, the HTTP calls become a local command.

What is dropped falls into three kinds:

- **Things that exist only because of tmux and screen reading:** attaching a thread to a hand-made window, window lists, pane screenshots, resync, the unknown-prompt safety net, the menu watchdog.
- **A second control surface:** the HTTP API with its scheduled notifications and "lounge" endpoints. It accounts for most of the input-validation bugs. The control API, reached through a local `hataori` command, replaces it.
- **Opt-in extras and tooling:** table images, the mockup renderer, the status board, thread-name lamps, subagent "voices", the fuzz harness, evidence capture, localised READMEs.

### 2.2 Bug issues by cause

c-lord has 698 issues. 443 are bugs (labelled `bug`, or titled `bug:` / `fix:`). Each was put into one group by its title; borderline cases went to the cause named first in the title. The grouping is a judgment, so read the counts as ±10%. Appendix C lists every number.

| Cause | Count | Examples | What removes it in hataori |
|---|---:|---|---|
| Reading the terminal screen: finding menus, dialogs, limits and login state in pane text | 52 | c-lord#388, c-lord#990 | The screen is not read. Questions, approvals, turn ends and failures arrive as hook calls or protocol messages. One start-up dialog is left (section 3.5). |
| Typing into the terminal: keystrokes lost, split, reinterpreted or sent to the wrong dialog | 22 | c-lord#560, c-lord#1284 | Input is one paste, and it counts as delivered only when the agent's own hook reports the same text. Nothing is typed before the session has reported its start. |
| tmux window bookkeeping: which window is which thread, twins, reaping | 32 | c-lord#649, c-lord#701 | No tmux. One process per session, tracked by the database. |
| Tailing the transcript file: replies lost, posted twice, user input echoed, internal text leaked | 39 | c-lord#553, c-lord#773 | The agent's hooks name the transcript file; it is read from a stored position, and only for text. What hataori pasted is known from the hook, so nothing has to be recognised as an echo. |
| Guessing turn state from timers and polling: false "finished", false timeouts, stuck lamps, wrong person pinged | 64 | c-lord#541, c-lord#1132 | Turn start and end are reported by the agent, with a reason. One state machine per session. |
| Subagents and background tasks, tracked through the transcript | 20 | c-lord#1136, c-lord#1302 | Subagent start and end are hook calls with their own transcript path; a turn's end lists the background tasks still running. |
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

This table compares c-lord's way with the two protocol interfaces. The path chosen for Claude Code is neither column; it is in section 3.4.

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

The Claude Agent SDK exists for Python and TypeScript only. Its docs tell other languages to run the CLI with `-p`, and describe the SDK as a library that runs the Claude Code binary. So from Go the SDK route is `claude -p`, and it carries the same risk as `-p` itself. The same docs say that third-party developers may not offer claude.ai login for their products without approval.

### 3.4 Decision

**Principle.** hataori never touches login. It runs the CLI the operator installed, and whatever that CLI is logged in with, a subscription or an API key, must work. `claude -p` does not meet this in the long run: Claude Code's docs say `--bare` "will become the default for `-p`", and with `--bare` the same subscription login gave "Not logged in". So hataori does not depend on `-p`. This was the maintainer's decision. Helping the owner log in again (section 4.7) keeps to the same rule: hataori runs the CLI's own login command and passes on the link and the owner's code, and stores neither.

**Claude Code: the interactive CLI in a terminal that hataori owns.** The session host (section 4.3) starts the ordinary `claude` in a pseudo-terminal, with `--session-id` on first start and `--resume` after, and with `--settings <file>` pointing at a file in the session's state directory that registers hooks. Nothing is written into the working tree or the user's settings. Each kind of information has one source:

| What hataori needs | Where it comes from | Checked |
|---|---|---|
| The session is up; its id and transcript file | `SessionStart` hook | Yes. The id equalled `--session-id`. |
| A turn started, and who started it | `UserPromptSubmit` hook, with the prompt text. A prompt hataori did not paste is the agent's own (it arrives as a task notification) or a person's at the terminal. | Yes |
| A turn ended; the final answer; background tasks still running | `Stop` hook: `last_assistant_message`, `background_tasks` | Yes |
| A turn failed, and why (rate limit, login, ...) | `StopFailure` hook: `error` | Documented; not triggered in the checks **(unverified)** |
| A permission request, and the answer to it | `PermissionRequest` hook; the hook's reply allows or denies | Yes. No dialog was answered by keys. |
| A question to the user, and the answer to it | `PreToolUse` hook for `AskUserQuestion`; the hook's reply carries the answers | Yes |
| Tool and subagent activity | `PreToolUse`, `PostToolUse`, `SubagentStart`, `SubagentStop` hooks | Yes |
| The session ended | `SessionEnd` hook, and the host's record of the exit | Yes |
| Text before the final answer | The transcript file named by the hooks, read from a stored position | Format seen; reader not built |
| Input | One bracketed paste into the terminal, then Enter. Delivered means the `UserPromptSubmit` hook reported the same text. | Yes: 6,188 characters in 64 lines, with a leading `-`, a trailing `;`, Japanese, backticks and quotes, arrived unchanged (inside a `pasted_content` tag) |
| Interrupt | The Escape key. | Yes: the running command was gone |

Each hook is the command `hataori hook`. It hands its payload to the session host and, for the two hooks that decide something, waits for the reply. The screen is never read to learn state.

**Permission requests in phase 1** are answered "allow" by the hook. This gives c-lord's behaviour without starting Claude Code in its bypass mode, which shows a warning dialog of its own (c-lord#695; not re-checked on this version). "Ask in chat" is later the same path with a person behind the reply.

**Codex:** `codex app-server` over pipes, one process per session, `thread/start` with `cwd`, then `turn/start`, `turn/steer`, `turn/interrupt`, `thread/resume`. Unchanged.

**Attach by hand.** A Claude Code session is a real terminal program, so a person can be connected to it: `hataori attach <thread>` relays their terminal to the session's through the host, to watch and to type while it runs. What they type is reported by the same hooks and shows in the thread. `hataori tail <thread>` is the read-only form. The host owns the terminal directly; tmux is not needed, and none of c-lord's window bookkeeping comes back. Attach is after phase 1 **(not built, not run)**.

**Two alternatives are kept as spikes**, separate from the adapter, because either could replace paste as the way in:

- *Channels.* A channel is an MCP server that Claude Code starts; it pushes messages into the session and offers a reply tool, and can relay permission requests. It is a research preview: a custom channel needs `--dangerously-load-development-channels`, which shows a warning dialog at start, and delivery is not acknowledged (from the docs; not run).
- *Background sessions.* `claude --bg` runs a session under Claude Code's own supervisor, `claude agents --json` reports its state, and `claude attach` opens it. The docs name no way to send input to a running one without a terminal (from the docs; not run).

### 3.5 What hooks do not cover, and other risks

- **The folder-trust dialog.** The first time the interactive CLI starts in a repository, it asks whether the folder is trusted, with the cursor on "No, exit". Until that is answered no hook runs at all (documented, and seen: zero hook calls in 12 seconds). Trust is recorded per repository root, and trusting a parent directory does not cover a clone inside it (documented). So every new workspace meets the dialog, and no hook can report or answer it. The ways out are: recognise this one dialog in the terminal output and answer it by keys; write the trust record into Claude Code's own state file before starting; or have a person accept it. None is clean. This is question 1 in section 10.
- **Any other start-up dialog** (a login prompt, a consent prompt for a new MCP server) has the same shape: nothing is reported. The rule: a session that has not reported `SessionStart` within the start timeout is `failed` with reason `blocked at start`, with its terminal output attached for a person to read. hataori does not guess and does not type.
- **An interrupted turn fires no hook** (documented, and seen). Its end is read from the transcript, which gets a line saying the request was interrupted. This is the one piece of turn state that rests on the transcript.
- **The transcript is not a documented format** as far as we found **(unverified)**. Hooks are: their payloads are specified field by field. So the split matters: turn state, questions, failures and the final answer come from hooks; only text before the final answer, and the interrupt line, come from the transcript. If the transcript changes, progress text degrades and nothing else.
- **A hook that waits for a person.** Command hooks time out after 600 seconds by default (documented). A question may wait for hours, so the hook's timeout must be raised, and what happens when it still expires must be known **(unverified)**.
- **Input while a turn is running** was not checked on this path **(unverified)**. Until it is, hataori holds such input and pastes it when the turn ends.
- **Paste depends on the terminal UI's input box.** It was checked in the default editing mode only; the user's own settings (vim mode) are **unverified**.
- **Whether the usage-limit menu still needs an answer** on screen when a limit is hit is **unverified**; c-lord answers it by keys.
- **Codex `app-server` is marked experimental** in its own help text. Captures per version, unknown messages skipped.
- **Codex interrupt leaves the command running** (observed). The session host runs the agent in its own process group so hataori can end what the agent left behind.

For every version of an agent CLI: decode against captures of that version's real output, skip what is unknown, and run the capture suite before the version is used for real work.

## 4. Core model and lifecycle

### 4.1 Three objects

- **Thread:** one line of work. Identified by `(scope, key)`. Both are opaque strings that a client gives the core: for a chat surface the scope is a channel and the key is the surface's thread. The core never looks inside them.
- **Session:** one agent conversation, identified by the agent's own session id, and the process that currently serves it (or none).
- **Workspace:** one directory, a clone of one repository, owned by exactly one thread.

A thread has one workspace for its whole life and one current session. Clearing the conversation makes a new session in the same workspace.

### 4.2 Session states

```
creating ──► idle ◄──► running ◄──► waiting      (waiting: a question or approval is open)
   │          │           │
   ▼          ▼           ▼
 failed     asleep ──► idle        (asleep: no process; the next message resumes it, through creating)
              │
              ▼
           closed ──► removed      (closed: stopped by a person or by idle age; workspace kept)
```

Every transition is caused by one event (a chat message, an agent event, a timer, a command), is written to the database in the same transaction as the events it produces, and is the only place that state changes. Lamps, the progress line, pings and turn-end events are all derived from those events, so they cannot disagree with each other.

### 4.3 The session host

Each agent is started by a small host process (`hataori host`, the same binary). The host:

- starts the agent in the workspace, in its own process group, on a pseudo-terminal or on pipes, whichever the adapter asks for;
- takes what the agent reports and appends it, one line each, to `events.ndjson` in the session's state directory. For an agent on pipes that is its output. For an agent in a terminal it is the calls its hooks make (`hataori hook`), and when a hook waits for an answer, the host holds the call open and passes the answer back;
- accepts input on a Unix socket and writes it to the agent's terminal or stdin;
- keeps the terminal's raw output in a bounded log, for a person to look at and for `attach`. Nothing reads it to decide anything;
- records the exit status when the agent ends.

The host does not parse or interpret anything, so it rarely needs to change. It is detached from the daemon. When the daemon restarts, running turns continue; the daemon reconnects to the socket and reads the log from the byte offset it last stored. A hook that is waiting for an answer keeps waiting across the restart.

Checked: a `claude -p` process with stdin on a FIFO and stdout on a file finished its turn after the process that launched it had exited, and accepted a second message afterwards. The interactive CLI was run in a terminal with its hooks writing to a file (Appendix B). The host itself is new code and is the first thing phase 1 must prove.

### 4.4 Running a turn

1. A client calls the control API: start a thread, or send to one. The chat front end does this for an inbound chat message. The call carries the caller and a key that makes it safe to repeat (for chat, the surface's message id). The core stores it in the **inbox**; a key already there is ignored. This is what makes "run exactly once" true across reconnects and catch-up.
2. The core authorizes the caller. One function, for every call from every client.
3. The core finds or creates the thread, workspace and session, and sends the input. If a turn is running, the call says what to do: refuse, deliver into the turn, or interrupt (section 10).
4. The agent adapter decodes what the agent reported into agent events: lines of the host's log, and lines of any file the adapter asked to follow, such as a transcript. Each event has `(session, sequence number)`.
5. For each agent event the core updates state and appends to the **event log** in the database: thread created, turn started, text, question asked, tool progress, turn ended. Events are numbered per instance. Any client can read them from a position, so a reader that was down misses nothing.
6. The chat front end is one such reader. For each event it decides what to show and writes rows to its **outbox**, keyed by `(event, part)`, so reading an event twice writes nothing twice. The sender posts rows in order per thread, paced per route, and stores the surface's message id on success. Rows marked as decoration (lamps, progress line, thread rename) are dropped after a few failures and never block a reply.

One gap remains: if the daemon dies after the surface accepted a post but before the id was stored, the post is sent again. An adapter closes the gap where its surface offers an idempotency key (Discord's message nonce, **unverified**).

Questions and approvals are rows too: the agent's request id, the question, the message that shows it. A button carries the request id. After a restart the row is still there, so the button still works (c-lord#671, c-lord#752).

**Turn-end events** are what a managing agent depends on most, so their rules are fixed here:

- Every turn produces exactly one end event, whoever started it. In the first deployment's c-lord log, 175 of 298 recent completions were turns the agent started by itself after a background task.
- The event says who started the turn, how it ended (`completion`, `blocked` on a question, `failure` with a reason) and how many background tasks are still running. A reader that wants "really finished" waits for an end event with none running. c-lord instead held the event back for up to an hour, and the held state had bugs of its own (c-lord#1249, c-lord#1296). For Claude Code the count comes from the `Stop` hook's list of background tasks; whether background subagents appear in that list like background commands is **(unverified)**.
- Events are read from a position (`hataori events --after <n>`), not only tailed, so a reader that restarts loses none.

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

### 4.7 Login alert and relay

When an agent's login runs out, every turn fails until someone logs in on the host, and the owner is usually not at the host. c-lord solved this in two steps, and hataori keeps both.

**Alert.** A turn that fails with reason `login` opens a *login outage* for that agent on this instance. The owner gets one direct message for the outage: which instance, which threads stopped. Threads that stop later are added to the same message. The outage is a row in the database, so a restart does not send a second message. It closes when any turn of that agent ends without a failure, or when the relay succeeds.

**Relay.** With the alert, hataori starts the agent CLI's own login command, as a process of its own, with the same config directory as the agent that is logged out.

1. The command prints a sign-in link. hataori puts the link into the owner's direct message, with the time until which a code is accepted.
2. The owner opens the link, signs in, and replies in the direct message with the code the page shows.
3. hataori writes that reply to the login command and reports the result in the direct message. Success is the command ending cleanly and the CLI's own status check saying it is logged in. On success each stopped thread gets one line: the login is back, send your message again.
4. If no code arrives in time, the command is stopped and the message says so. A button on the message starts a fresh login at any time, also after a restart.

The rules:

- **Only the owner's direct message is ever passed to the login command, and only while the command is waiting for a code.** A direct message from anyone else does nothing. The owner's message at any other time gets "not waiting for a code". A reply that is not shaped like a code (one word, no spaces) is not passed on.
- **The link and the code exist only in the direct message and in memory.** They are not written to the database, the event log, any log file or any other conversation, and no client of the control API can read them. The log says that a code arrived and how long it was. For this reason the login command is a direct child of the daemon and does not run under the session host, which logs what it sees. A daemon restart ends a login that is in progress; the button starts another.
- **Nothing is resent.** A message that failed is sent again by whoever sent it.
- **If someone logs in on the host instead**, the next turn that completes closes the outage, the waiting login is stopped, and the message says the login is back.
- The relay can be switched off in the config; the alert stays.

This is the only input hataori takes from direct messages in phase 1, and the only button.

For Claude Code the command is `claude auth login`. It was run with a throwaway config directory, so the existing login was not touched: it works on pipes without a terminal, prints the link on one line and then asks for the code, and answered a wrong code with one line and kept waiting (Appendix B). What it prints and how it ends after a correct code was not run **(unverified)**. Codex comes with its adapter: `codex login --device-auth` prints a link and a code and needs no reply, and `codex login status` reports the result (from its help text; not run).

## 5. Interfaces

The core imports no adapter package. Adapters and clients register under a name, and that name is used only as a key and a path segment.

### 5.1 The control API

Everything that can be asked of the core is one interface. In-process clients call it directly; other processes (the `hataori` command, the supervisor's tools, the dashboard) reach the same operations over a Unix socket.

| Operation | What it does |
|---|---|
| `Threads` | list the threads the caller may see |
| `Thread` | one thread's state, open requests and last output, capped |
| `Start` | start a thread in a scope; safe to repeat with the same key |
| `Send` | send input; says what to do if a turn is running: refuse, deliver, interrupt |
| `Answer` | answer a question or an approval |
| `Stop` | interrupt the running turn |
| `Events` | events after a position, then live ones |

Every call is made as a `Caller`: who it is, which scopes it may see, and which operations it may use. The core checks each call against it. A scope has one owner, the client that gives it its threads; when another client starts a thread there, the core asks the owner for the thread's key, and a chat front end answers by opening a chat thread. Appendix D has the Go declarations.

Sleep, close and remove join the interface with the phase that needs them. There is no operation to reset or merge anything, for any caller.

### 5.2 Chat front end and surface driver

Showing a thread in chat is one client, shared by all chat surfaces. It turns inbound chat into `Start`, `Send`, `Answer` and `Stop`; it reads `Events` and decides what to show; it owns the outbox. Underneath it, a surface driver knows one chat service and nothing about agents or threads of work.

The driver's calls are: run and deliver inbound messages, button presses and commands; fetch messages since a cursor; open a thread; post, edit and delete a message; set a status on a message; set a thread's title and archived flag; report its limits. A message to post says what to show (text, files, choices, reply target, who to notify), not how. A driver can also open a direct conversation with one person; in phase 1 that is used for the owner only (section 4.7).

The driver splits long text, picks buttons or a menu, and maps a status to whatever the surface has (a reaction, an edit). Its limits let the front end avoid asking for what a surface cannot do.

### 5.3 Agent adapter

An agent adapter has two parts. The first says how to start the agent for a session, new or resumed: the command, whether it needs a terminal or pipes, and any files to put in the session's state directory first (for Claude Code, the settings file that registers its hooks). It also says whether the agent is installed and logged in, and, if the agent can be logged in again from chat, how to run its login command and how to read the link it prints. The second is a **codec**: it turns one reported line into events, whether the line came from the agent's output, from a hook call or from a file the codec asked to follow; and it turns a user message, an answer or an interrupt into bytes for the agent's input or into the reply to a waiting hook. The codec does no I/O.

Splitting the agent adapter into a launch description and a pure codec is what makes the repository's test rule practical: a capture of real output goes in `testdata/`, and the codec must turn it into the expected events. The session host and the core never see agent-specific bytes. The same interface fits an agent on pipes that speaks a protocol and an agent in a terminal that reports through hooks, which is how the Claude Code adapter could be changed without touching the core.

`TurnEnded` carries a status (`completed`, `interrupted`, `failed`) and a reason (`limit`, `login`, `api_error`, `crashed`, `blocked at start`, `not delivered`). `SessionEnded` carries a reason too; `cannot resume` means the agent no longer has the conversation, and the core starts a new session in the same workspace. `TurnStarted` says who started it (a caller, the agent itself after a background task, or a person at the terminal). These two fields replace most of the "guessing turn state" group.

### 5.4 Did the first draft allow chat, supervisor and dashboard on one API?

No, not as written. The agent side is unchanged. The surface side was a plug-in that the core called: the core itself received every inbound chat event (the `Sink` interface), owned the outbox, and pushed finished messages to one surface per thread. Nothing in it could list threads, read a thread's state, or follow events, which is all a dashboard does and most of what a supervisor does. And a thread was identified by `(surface, channel, thread)`, which the core had to understand.

What changed: the control API (5.1) now sits between. The methods of the old surface interface survive as the driver (5.2), but `Sink` is implemented by the chat front end, the outbox moved to it, and the core identifies a thread by two opaque strings. The supervisor's tools and the dashboard's server are two more clients, and so is the `hataori` command.

## 6. Storage and layout

### 6.1 One root per instance

```
<root>/
├── bin/          one binary per version, and a link to the one in use
├── config/       this instance's config and secrets (private; never linked from another instance)
├── state/
│   ├── hataori.db            SQLite, WAL, one writer (the daemon)
│   ├── log/
│   └── sessions/<id>/        events.ndjson, terminal log, host socket, agent settings, attachments
├── workspaces/<surface>/<channel>/<thread>/   the thread's scope and key, as path segments
├── mirrors/      one bare mirror per remote
└── trash/        removed workspaces, kept for the retention period
```

An instance reads and writes only under its own root. A staging instance is another root with another bot identity. At start the daemon takes a lock in `state/`, checks that the identity it logged in as is the one in its config, and exits if either fails (c-lord#323, c-lord#325).

Unix socket paths are limited to about 100 bytes, so the socket path must stay short however deep the root is; if `state/sessions/<id>/` is too long the host uses a short path under the instance's runtime directory and records it.

### 6.2 SQLite

One database file. A schema version table and forward-only migrations (c-lord has no schema version). Main tables:

| Table | Holds |
|---|---|
| `threads` | `(scope, key)`, short number, title, who started it, state |
| `workspaces` | path, remote, state |
| `sessions` | agent name, agent session id, state, model, host pid, log offset, last sequence |
| `turns` | who started it, the message that triggered it, start, end, status, reason |
| `inbox` | accepted calls by their key; for chat, the catch-up cursor per conversation |
| `events` | the numbered event log that clients read from a position |
| `outbox` | the chat front end's pending and sent posts by `(event, part)`, with the surface message id |
| `requests` | open questions and approvals, their message, their answer |
| `bindings` | channel to repository, and per-thread overrides |
| `settings` | model and other choices per scope |

Who is allowed is in the config file, not in the database.

### 6.3 Workspaces and mirrors

A workspace is a full clone per thread. To avoid downloading a repository for every thread, hataori keeps one bare mirror per remote, fetches it before a clone, and clones from it. The rule: **a workspace stays valid if its mirror is deleted.** So the clone takes its objects by hard link or copy, not by a reference to the mirror's object store, and its `origin` points at the real remote. (c-lord references the mirror's objects and protects them by never pruning the mirror.) A mirror that cannot be fetched in time is used as it is, or skipped; it never blocks a thread.

### 6.4 A second host

A second host (for example a macOS machine) is another instance: its own root, database, agent logins and bot identity. Hosts share nothing and do not coordinate. A channel belongs to one instance, by config. Which bot answers tells you which host the work is on. Moving a thread between hosts is not supported; push the branch and start a thread on the other host.

For macOS specifically: the service manager is launchd, not systemd; the host process must not depend on `/proc`; Claude Code keeps its login in the Keychain there, which a background service may not be able to read **(unverified)**; the default filesystem is case-insensitive, which matters for workspace paths. None of this was run.

## 7. Supervisor, dashboard and plugins

All three are wanted, all come after phase 1, and all are clients of the control API. The supervisor and the dashboard add no concept to the core; plugins add hook points (section 7.3).

### 7.1 Supervisor agent

A supervisor is one ordinary session, Claude Code or Codex, that looks after the threads of one scope (one channel). It is a thread like any other, with its own directory and no repository. It is not an orchestration system: no roles, no task graph, no memory of its own beyond its conversation.

**What it can do.** hataori gives it the control API as MCP tools, through a small MCP server (`hataori mcp`) that connects to the daemon as a `Caller` limited to that scope:

| Tool | Control API call |
|---|---|
| list threads | `Threads` |
| read one thread: state, open question, latest output | `Thread`, output capped |
| send to a thread | `Send` with `Refuse` when busy |
| start a thread | `Start` |

**What it cannot do.**

- Interrupt a busy thread. `Send` is refused with "busy" and the supervisor gets the turn-end event later. Interrupting is a separate tool, and each use of it is put to a person as an approval in the supervisor's own thread.
- Merge, delete or reset. The control API has no such operations, so there is nothing to grant.
- Touch other workspaces directly. Its session runs without a shell and with file tools confined to its own directory. Checked for Claude Code (in a `-p` run): with `--restricted --strict-mcp-config --mcp-config <file>` the session had no Bash and no WebFetch, loaded only the listed MCP servers, and a write outside its directory was refused. That mode still leaves subagent and wake-up tools, so the tool list is also narrowed with `--tools` **(not run)**. The Codex equivalent is its read-only or workspace-only sandbox **(not run)**.

One limit is not enforced by this: the supervisor can *ask* a worker thread to merge or delete. What a worker may do is decided by that worker's own rules. Messages from a supervisor are marked as such, so a worker's instructions can treat them differently from a person's.

**When it runs.** It never polls. The core wakes it by sending it one message when something happens in its scope: a turn ended, a question was asked, a turn failed, a usage limit was hit. Its own thread is excluded. Events that arrive while it is busy, or within a short quiet period after the first, are folded into one message.

**How its cost is bounded.**

- *No events, no turns.* An idle scope costs nothing.
- *Small input per wake.* The wake message is a digest: thread, how the turn ended, the last lines. Reading more is an explicit tool call with a capped answer.
- *A budget per scope:* supervisor turns per hour, sends and starts per hour, and tokens per day, counted from the usage the agent records for each turn. Over budget, the supervisor is not woken; a person is told once.
- *A loop guard.* A supervisor send that ends a worker turn that wakes the supervisor is a cycle. After a set number of cycles on one thread with no person's message in between, that thread stops waking the supervisor until a person acts.
- *The ordinary limits apply:* the cap on turns running at once, and sleep when idle.
- *A cheaper model by default.* Its job is reading digests and routing.

### 7.2 Dashboard

Every thread side by side with its live state and recent output, like tiled terminal panes. The unit is the thread, not the pane, so it does not depend on how hataori talks to the agent. A session that runs in a terminal, as Claude Code's do, can also show that terminal live in its tile.

- **Served by hataori**, as a client of the control API: `Threads` for the grid, `Thread` for a tile's content, `Events` from a position for live updates. A page that reconnects asks for events after the last one it saw, so it shows no gap and no repeat.
- **Read-only first.** The first version makes no call that changes anything. Sending, answering and stopping come later through the same API and the same checks.
- **A native window later** (Wails or Electron) wraps the same pages. Not designed here.

**Authentication is required, also on a private network.**

- No route, including the event stream, answers without a signed-in session.
- Sign-in is a short-lived, single-use link. A person asks for it in chat and gets it privately, or the operator prints one on the host. The dashboard session then belongs to that chat identity, so there are no extra passwords and no surface-specific login. Other ways (the chat service's own sign-in) can be added.
- A person sees a thread only if their `Caller` includes its scope. The channel decides who can read a thread in chat, and the dashboard must not widen that. The first version is owner-only.
- The server checks `Host` and `Origin` on every request, so a web page elsewhere cannot reach it through the user's browser. Cookies are not readable by scripts. Anything beyond loopback should be served over TLS.

### 7.3 Plugins

A plugin lets another developer add to what hataori does, in any language, without changing the core. The aim is that more people use hataori and that it grows by reusing what exists.

Its shape follows OpenClaw's plugins: a manifest, registration, and hooks. Hook names and timing follow OpenClaw's where the meaning is the same, so someone who wrote an OpenClaw plugin can write one for hataori, and an existing one is easy to port. One thing differs. An OpenClaw plugin is loaded into the Gateway's process and is trusted like core code; its documentation says it is not sandboxed. A hataori plugin is a separate program, for two reasons:

- Go cannot reliably load code into a running binary. The standard `plugin` package needs the same toolchain and the same version of every shared package as the binary.
- Only a separate program makes what a plugin declares enforceable. The core checks each of its calls, as for every client.

hataori does not depend on OpenClaw. An OpenClaw plugin that talks to hataori is an ordinary client of the control API.

**What is not a plugin.** Tools used inside a session (MCP servers, skills, Claude Code and Codex plugins) already work in a hataori session; hataori only hands them to a session by flags, per scope (section 4.6). hataori does not load OpenClaw's native plugins. The built-in surface drivers and agent adapters stay in the binary.

**How a plugin runs.**

- A plugin is an executable with a manifest, `hataori-plugin.json`, next to it. Any language that can open a Unix socket can be used.
- The operator lists it in the config with its path and the SHA-256 of the executable. hataori starts it as a child process, passes the socket path and a token for this start in its environment, and stops it when the daemon stops. An executable whose hash does not match is not started.
- A plugin that exits is started again after a backoff. After repeated failures it stays stopped, and the owner is told once. A plugin's failure never stops the daemon or a turn.
- A program that runs elsewhere, such as an OpenClaw plugin, is not started by hataori. It connects with a token from the config and gets the same checks.
- It speaks the control API's wire protocol on the same socket as the `hataori` command, and states its protocol version when it connects.
- It reads events from a position, like any client, and keeps the position itself, so a restart misses nothing.

**The manifest** says what the plugin is and what it wants. hataori reads it without running the plugin.

| Field | Meaning |
|---|---|
| `id`, `version` | the plugin's name and version |
| `protocol` | the protocol major version it speaks, and the lowest minor version it needs |
| `operations` | the control API operations it calls, for example `Threads` and `Send` |
| `events` | the event kinds it reads |
| `hooks` | the hook points it handles, each with an optional priority and timeout |
| `conversation` | whether it needs message text (below) |
| `config` | a JSON Schema for its own settings |

**What the operator grants.** The plugin's config entry names the scopes it may see and act in, and may narrow the operations, events and hooks below what the manifest asks for. The plugin's `Caller` is what the manifest asks for and the config grants, and nothing more. Any other call is refused by the core's one authorization function (section 4.4), and the refusal is logged.

Message text (input to the agent, the agent's text, outgoing posts) reaches a plugin only with an explicit `conversation` grant, as with OpenClaw's `allowConversationAccess`. Without it, the plugin gets events and hook calls with the text left out. No client can read the login relay's link and code (section 4.7), so no plugin can either.

**Hooks.** A hook is a call from hataori to a plugin at a fixed point, which the plugin answers. The kinds are OpenClaw's:

- *Observe*: the plugin is told and does not answer. In hataori these are events read from a position, with no separate mechanism.
- *Modify*: the plugin may change what passes the point.
- *Gate*: the plugin may stop it, with a reason that the thread shows.

The first hook points, and the events, next to the OpenClaw hook with the same meaning:

| hataori | Kind | Where | OpenClaw |
|---|---|---|---|
| `before_agent_run` | Gate | core: before input from `Start` or `Send` reaches the agent | `before_agent_run` |
| `before_prompt_build` | Modify | core: at the same point; may add text before or after the input | `before_prompt_build` |
| `message_sending` | Modify, Gate | chat front end: before an outbox row is posted; may rewrite the text or cancel the post | `message_sending` |
| thread created | Observe | core event | `session_start` |
| turn started | Observe | core event | none |
| tool progress | Observe | core event | `after_tool_call` (closest) |
| question asked | Observe | core event | none |
| turn ended | Observe | core event | `agent_end` |
| post sent | Observe | chat front end event | `message_sent` |
| plugin started, stopping | Observe | told to the plugin itself | `gateway_start`, `gateway_stop` |

The event names follow the session state machine (#16) and may still change. A cancelled post stays in the outbox with the plugin and the reason, and is visible through `hataori thread`. Nothing is posted in its place unless the plugin returned new text.

**Order, timeout and failure.** Handlers on one point run one after another, by priority (higher first) and then in config order, and each sees the result of the one before. The defaults follow OpenClaw. Each handler has 15 seconds. When a `before_agent_run` handler fails or times out, the input is blocked (fail closed), and the thread says which plugin blocked it. When a `before_prompt_build` or `message_sending` handler fails or times out, that handler is skipped and the other results are kept. The operator can change the timeout and the failure policy per plugin, for example to make a secret scanner on `message_sending` fail closed.

**Distribution.** At first a plugin is a path, a version and a hash in the config. There is no registry and no install command. A list of known plugins, and signatures, come when there are plugins to list.

**What phase 1 must allow.** Adding hook points later must not need a new major version of the protocol (#51):

- One connection carries requests in both directions, so the daemon can send a client a request and wait for the answer. A scope owner that is a separate program needs this already (`NewThread`, Appendix D), and hooks use the same message.
- A client that gets a request it does not know answers "not supported". For a hook, the daemon then applies that point's failure policy.
- Text fields are optional in every event, so leaving text out for a plugin without the `conversation` grant does not change the format.

**Later, not designed here.** *Claim* hooks, by which a plugin takes an inbound message instead of the normal path (OpenClaw's `inbound_claim` and `before_dispatch`); this is how routing an order to a thread would be done. A plugin that registers a surface driver or an agent adapter, as OpenClaw's `registerChannel` does: it would implement the driver or adapter calls of Appendix D over the socket. Commands that a plugin adds to chat. Confinement beyond a separate process; running a plugin as another OS user is a deployment choice.

## 8. Phases

Each criterion is met or not. "Core packages" means everything except drivers, agent adapters, clients and `cmd/`.

### Phase 1: replace c-lord for dispatched orders (Discord and Claude Code)

The first deployment's orders are sent by a managing agent, not typed by a person. It starts a thread by posting a text trigger with a repository and a prompt through a channel webhook, follows up through the same webhook, chooses model and effort per order, and learns that a turn ended from an event log. It reads results from the thread and tells a running turn from a stuck one by the progress and subagent lines. Phase 1 is exactly that, plus what running unattended needs (restart safety, sleep), plus the tests.

In phase 1:

- starting from a webhook post in an enabled channel, with `repo:`, `model:` and `effort:` on the trigger line; a default model and effort per channel;
- follow-ups into the thread through the webhook;
- only the configured webhooks and the owner are accepted; everything else is ignored and logged;
- progress text, final answer, the quiet-gap progress line, subagent start and end lines, long replies split, files the agent hands over;
- a question from the agent shown as text, with the next message as its answer; permission requests allowed automatically, as in c-lord;
- a new workspace starting with nobody at the terminal (the folder-trust dialog, section 10);
- turn-end events as in section 4.4;
- `hataori threads`, `hataori thread`, `hataori events`, `hataori stop` on the host. These are the first control API client;
- the session host, catch-up of missed messages, sleep and resume;
- one root per instance;
- the login alert and relay in the owner's direct messages (section 4.7).

Not in phase 1: slash commands, buttons (except the one on the login message), the Stop button, lamps, pings, the footer, attachments into the agent, attach by hand, thread naming beyond a title from the prompt, and every way of removing a workspace. Phase 1 never deletes a workspace.

Acceptance:

- [ ] The full test suite exists before the first real order: the codec against captures of real Claude Code hook calls and transcript lines for every scenario in Appendix B, and end-to-end tests with a fake driver and a fake agent for each criterion below that does not need the real services.
- [ ] A webhook post with the trigger, a repository and a prompt in an enabled channel creates a thread, a clone under `workspaces/discord/<channel>/<thread>/`, and a first reply. The full prompt is in the thread, and editing the channel message afterwards changes nothing.
- [ ] The same post in a channel that is not enabled, or from an account that is not allowed, starts nothing and writes one log line.
- [ ] A webhook post into the thread continues the same agent session id.
- [ ] A thread in a repository that was never opened on the host before starts with nobody at the terminal; a session that does not report its start within the timeout is `failed` with its terminal output attached.
- [ ] `model:` and `effort:` on the trigger line are the ones the agent reports for that thread, and the next thread without them gets the channel's defaults.
- [ ] A turn of 30 minutes ends with its final answer posted once.
- [ ] Every turn, including one the agent starts by itself after a background task, produces exactly one end event, with who started it and how many background tasks are still running.
- [ ] `hataori events --after <n>` returns exactly the events after `n`, also after the daemon was restarted.
- [ ] A question from the agent appears in the thread, produces a `blocked` event, and is answered by the next message.
- [ ] A usage limit, an expired login and a crashed agent each produce one line in the thread and one `failure` event with that reason. (Recorded from the real CLI when they occur; simulated until then.)
- [ ] While a tool runs for more than 90 seconds, the thread shows one progress line that is updated in place and removed at the end.
- [ ] `hataori thread <key>` tells a hataori thread from an ordinary one, and gives its state.
- [ ] `hataori stop <thread>` ends a running command within 10 seconds and leaves no process of the agent's behind.
- [ ] The daemon is killed with SIGKILL during a turn and started again: the turn finishes, every reply appears exactly once, and its end event appears once.
- [ ] Three posts into a thread while the daemon is down are run once each, in order, after it starts.
- [ ] After the idle period the agent process is gone; the next post resumes the same conversation. The same after all processes were killed, as in a host reboot.
- [ ] When turns fail because the agent is logged out, the owner gets one direct message for the outage, also across a daemon restart, and threads that stop later are added to it.
- [ ] The owner's reply with the code, and nothing else, reaches the login command; on success the message says so and each stopped thread gets one line; no message is resent.
- [ ] After a login through the relay, the sign-in link and the code are found nowhere in the database, the event log or the log files.
- [ ] No code path in the binary removes or resets a working directory.
- [ ] Two instances with different roots on one host each ignore the other's channels.
- [ ] No core package imports a driver, an agent adapter or a client, or contains the name of a surface or an agent (checked in CI).
- [ ] For one week, every order the deployment's manager sends runs on hataori and none on c-lord.

### After phase 1

The order below is a recommendation (section 10). Each later phase also has this criterion: the change touches no core package, except to add an operation to the control API or to register an adapter; the plugin phase may also add hook points. Appendix E has the acceptance criteria of each.

**People in chat, and cleanup.** Slash commands, buttons for questions, lamps, pings, attach by hand, and removing workspaces behind the proof in section 4.6. This is what lets c-lord be switched off.

**Dashboard, read-only.** Section 7.2.

**Supervisor.** Section 7.1.

**Codex.** The second agent adapter (section 3.4).

**Slack.** Go has no official Slack SDK (official ones are JavaScript, Python and Java); a community library with Socket Mode is assumed **(unverified)**. Socket Mode needs no inbound endpoint.

**Microsoft Teams.** Microsoft's bot SDKs are C#, TypeScript and Python; the older Bot Framework SDK is archived. There is nothing for Go, so the adapter is written against the HTTP protocol. By hand **(the list is from memory, unverified)**:

- an HTTPS endpoint that receives activities, and validation of the signed token on each one;
- getting and refreshing the service token for outbound calls;
- the activity types hataori needs: message, message update, invoke (button press), conversation update;
- Adaptive Cards for choices;
- conversation references, so hataori can post without an inbound activity;
- file upload and download;
- pacing and retry.

Unlike Discord and Slack Socket Mode, Teams calls hataori. The host needs a public HTTPS address, which is a deployment decision.

**Plugins.** Section 7.3. Where this goes in the order is open (section 10).

## 9. Future work, not designed here

**Surfaces without threads.** The core takes a thread as an opaque key, so a surface with no native threads only needs a front end that makes the keys up. The intended rules: a reply to a hataori message continues that message's thread; any other message goes to the thread used last; `/new` starts one; every hataori message begins with the thread's name. If the bot cannot see what a message replies to, one chat is one thread. A surface whose threads expire (LINE OpenChat: 24 hours or two weeks) counts as having none.

## 10. Decisions

### Open

1. **How does a new workspace get past the folder-trust dialog with nobody at the terminal?** No hook runs before it is answered (section 3.5). The choices:
   - *Answer it by keys.* Before `SessionStart`, and only then, look at the terminal output for this one dialog, move to the line that says yes, and confirm. Success is `SessionStart` arriving; anything unrecognised is a failed start and no key is sent. This is the one place hataori would read the screen, and the dialog is behind nine c-lord issues.
   - *Write the trust record.* Mark the workspace as trusted in Claude Code's own state file before starting. No screen, but the file is not a documented interface, Claude Code rewrites it all the time, and it is the file that also holds the account.
   - *A person accepts it* once per workspace, through `hataori attach`. Clean, but an order then waits for a person.

   *Recommendation:* answer it by keys, as the single bounded exception, with the failed-start rule behind it. Writing into the CLI's state file is closer to touching login than reading one known dialog is. The adapter issue for start-up records what is tried first: whether Claude Code offers a supported way to trust a directory in advance.

2. **Where do plugins go in the order after phase 1?** *Recommendation:* right after the supervisor. The supervisor is the first program outside the daemon with a limited `Caller` that reads events from a position; plugins add the manifest, the grant and hooks on top of that.

### Decided

- **Claude Code is not driven through `claude -p`.** Decided by the maintainer; section 3.4. Live attach comes back with the terminal, so the earlier question about take-over between turns is closed.
- **A message that arrives while a turn runs is delivered into the turn**; stopping is the explicit way to interrupt. On the Claude Code path it is held until the turn ends, until delivery during a turn has been checked (section 3.5).
- **Permission requests are allowed automatically** by default, so unattended orders do not stall. "Ask in chat" becomes a per-channel setting with the buttons.
- **Phase 1 is only the dispatched orders.** Nothing a person types into chat directly is in it. c-lord stays for hands-on threads until the next phase.
- **Dropped:** the HTTP API (replaced by the `hataori` command, the control API and webhook triggers), scheduled tasks and notifications, docker container tracking, table images, the status board (replaced by the dashboard). Scheduled tasks come back first if they are missed.
- **One bot identity per instance**, including a second host.
- **Order after phase 1:** people in chat and cleanup, then the dashboard, then the supervisor, then Codex, Slack, Teams.
- **A supervisor has no shell:** control API tools and files in its own directory only.
- **Dashboard sign-in is a link issued in chat**, owner-only at first.
- **Existing c-lord threads do not move.** Both run side by side on different channels.
- **The login alert and relay are in phase 1** (section 4.7), for Claude Code. They are the only use of direct messages and the only button in phase 1.
- **Plugins are separate programs shaped like OpenClaw's plugins:** a manifest, registration and hooks, with OpenClaw's hook names and timing where the meaning is the same. hataori does not depend on OpenClaw. Tools used inside a session are not hataori plugins. Section 7.3.

## Appendix A: c-lord feature inventory

"Keep" does not mean phase 1; section 8 says what is in phase 1. Compiled from c-lord's README, `docs/`, `docs/specs/` and code. A reading agent collected the facts; the bridge facts in section 3.1 were re-checked in code, the rest of the rows were not re-checked one by one. Features that c-lord documents but does not run (tool-call embeds, permission buttons, starting a session by posting in the main channel, automatic resume after restart) are left out.

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
| Attach a thread to a hand-made tmux window | drop | No tmux; hataori starts every session's terminal itself. |
| Programmatic spawn over HTTP | change | The control API: `hataori spawn` on the host, the supervisor's tool, or the text trigger through a webhook. |
| CI webhook trigger with a fixed server-side prompt | drop | Not wired in c-lord's default setup; the text trigger covers it. |

### Input and turns

| Feature | Verdict | Why |
|---|---|---|
| A message in the thread is the next prompt | keep | |
| A new message interrupts the running turn | change | Delivered into the running turn instead (section 10). |
| Stop button and stop command | keep | Now an interrupt through the adapter plus ending the turn's process group. |
| Messages missed during an outage or restart are run once | keep | Built into the inbox instead of added later. |
| Limit on turns running at once | keep | Needed on a shared host. |
| Trusted bots and webhooks may post into existing threads | keep | Agents talk to agents this way. |
| Pasted chat links and replies are expanded into context | keep | Small, used daily. |
| Input checked on screen after typing; vim mode, paste folding, long-input splitting | change | One paste, confirmed by the agent's own hook instead of by reading the screen. |
| Post into a thread over HTTP | change | `hataori send`. |

### Output

| Feature | Verdict | Why |
|---|---|---|
| Progress text posted quietly, final answer as a notifying reply | keep | The final answer is the one the agent reports at the turn's end. |
| Long replies split without breaking code blocks | keep | Done by the surface adapter, which knows the limit. |
| `progress.txt` with the turn's tool calls attached to the answer | keep | Built from tool events. |
| Text typed by a person in the pane is shown in the thread | keep | Reported by the same hook as any other input, during `hataori attach`. |
| Hiding of internal harness text (reminders, notifications, slash-command records) | change | Chosen by a transcript line's type and fields, not by scrubbing text. |
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
| Agent questions as buttons or a menu, with free-text answer | keep | From the agent's request (a hook call or a protocol message), not from a menu on screen. |
| Multi-select questions | keep | Same request. |
| Plan approval as a menu | keep | Same mechanism **(not checked against the CLI)**. |
| Ping when input is needed | keep | |
| Buttons work after a restart; old cards are disarmed | keep | Requests are rows; nothing to recover. |
| Typing a sentence instead of pressing a button | keep | An open request takes the next message as its answer. |
| Tool permission prompts auto-accepted | change | Default stays bypass; "ask in chat" becomes possible (section 10). |
| Folder-trust dialog handling | change | Still needed: the interactive CLI shows it and no hook runs before it (section 10). |
| Unknown-prompt safety net, menu watchdog, dead-pane guard | drop | The screen is not read. A session that does not report its start fails with its terminal output attached. |
| Usage-limit menu answered with "wait" | change | The limit arrives as a hook call; whether the menu on screen still needs an answer is unverified. |

### Status and visibility

| Feature | Verdict | Why |
|---|---|---|
| Lamp on the triggering message (working, done, error) | keep | Derived from the state machine. |
| Stall lamps after 10 s and 30 s of silence | change | Driven by tool start and end reports and by process liveness, not by file silence. |
| Quiet-gap progress line, edited in place | keep | From tool events. |
| Turn footer: context use, model, versions | keep | From the usage the agent records per turn (source on the Claude Code path unverified). |
| Thread name `W<N> │ #issue topic` | change | The tmux window number becomes a short thread number that `hataori attach` accepts. |
| Issue or pull request number in the thread name; manual rename wins | keep | |
| Thread-name lamp, automatic topic, re-title | drop | Opt-in in c-lord; a source of rate limiting. |
| Status command with a table of sessions | keep | From the database. |
| Pinned status board | drop | Opt-in in c-lord; the dashboard takes its place (section 10). |
| Pane screenshot, window list, resync | drop | `hataori attach` and `tail` show the terminal itself. |
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
| Clear and compact the conversation | keep | Compact is given to the agent as its own command (unverified); clear is a new session. |
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
| Docker containers per thread tracked and listed | drop | Deployment-specific (section 10). |

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
| Scheduled tasks | drop | Section 10. |
| Scheduled and immediate notifications, "lounge" notes | drop | A second product inside the bot. |

### Models, limits, login

| Feature | Verdict | Why |
|---|---|---|
| Model and effort per thread, channel and instance | change | Per order on the trigger line and a default per channel, instead of setting the channel before each order and back after. |
| Usage limit shown with its reset time | keep | From the failure the agent reports; where the reset time comes from on the Claude Code path is unverified. |
| Messages during a limit are accepted | keep | |
| Owner told by direct message when the agent is logged out: one message per outage, also across a restart | keep | From the turn's failure reason. Phase 1 (section 4.7). |
| Logging in again from that direct message: sign-in link out, the owner's code in, a button for a fresh link | keep | Runs the CLI's own login command and stores neither link nor code. Phase 1 (section 4.7). An earlier version of this table dropped it. |

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
| `--restricted --strict-mcp-config --mcp-config <file>` | The tool list had no Bash and no WebFetch (21 tools instead of 48), only the listed MCP servers were loaded, and a write to a path outside the working directory was refused. Subagent, skill, web search and wake-up tools were still listed. |

Not checked: turns of hours; what the stream shows at a real usage limit or an expired login; `/compact` in `-p`; plan approval; two processes on one session at once; MCP tools given with `--mcp-config` and their approval requests; `--tools`; events for background subagents.

### Claude Code, interactive in a terminal, with hooks

`claude --settings <file> --session-id <uuid> --permission-mode default`, started in a terminal by a script. The settings file registered one command for 17 hook events; the command appended each payload to a file and, for two events, printed a decision. Input was pasted with bracketed paste and confirmed with Enter. The screen was looked at only to write this table.

| Check | Observed |
|---|---|
| Hooks from `--settings` | Ran, with nothing written to the working tree or the user's settings. |
| Start | `SessionStart` with `source: "startup"`, the model, `transcript_path`, and a `session_id` equal to the one given with `--session-id`. |
| Paste | A prompt of 6,188 characters in 64 lines (leading `-`, trailing `;`, Japanese, backticks, `$HOME`, quotes) arrived in `UserPromptSubmit` wrapped in a `pasted_content` tag, with the text inside identical. A short prompt arrived as it was. |
| Turn end | `Stop` with `last_assistant_message` (the final answer), `background_tasks`, `session_crons`, `stop_hook_active`. |
| Permission request | In the default permission mode a file write produced `PreToolUse`, then `PermissionRequest` with the tool and its input. The hook printed `behavior: "allow"` and the file was written. No key was sent. |
| Question | `PreToolUse` for `AskUserQuestion` carried the questions and options. The hook printed `permissionDecision: "allow"` with `updatedInput` holding an `answers` map, and Claude's reply named the chosen option. No key was sent. |
| Interrupt | Escape during a running command: the command was gone. No `Stop` and no `PostToolUse` followed. The transcript got a `user` line with the text `[Request interrupted by user for tool use]`. |
| Turn started by the agent | After a background command was started, `Stop` listed it under `background_tasks` as running. When it ended, `UserPromptSubmit` fired with a `<task-notification>` prompt, then `Stop` with an empty list. |
| Subagent end | `SubagentStop` with `agent_id` and `agent_transcript_path`; one fired with an empty `agent_type` for an agent Claude Code runs for itself. |
| Exit | `/exit` produced `SessionEnd` with `reason: "prompt_input_exit"`. |
| A repository never opened before | The folder-trust dialog, with the cursor on "No, exit". No hook call in 12 seconds. |
| Where trust is kept | After the dialog was accepted for one directory in an earlier check, that directory's entry in Claude Code's state file had `hasTrustDialogAccepted: true`; directories used only with `-p` had no such field. No documented switch to trust a directory in advance was found. |

### Claude Code: the login command

`claude auth login`, run with `CLAUDE_CONFIG_DIR` set to an empty directory, for which `claude auth status` reported `loggedIn: false`. The existing login was checked before and after and was unchanged. No login was completed.

| Check | Observed |
|---|---|
| On pipes, no terminal | Printed `Opening browser to sign in…`, then one line `If the browser didn't open, visit: <link>`, then `Paste code here if prompted > `, all on stdout. The link was on one line. |
| On a terminal | The same text; the link was also wrapped in a terminal hyperlink sequence and coloured. |
| A wrong code | Written to stdin on pipes, or typed on the terminal: `Invalid code. Please make sure the full code was copied.` (on stderr when on pipes). The command did not exit and went on waiting. |
| Status | `claude auth status` prints JSON with `loggedIn` and `authMethod`. |
| Options | `--claudeai` (default), `--console`, `--sso`, `--email`. |

Not checked: a correct code, the text and exit status of a successful login, and how long the link stays valid.

Not checked on the interactive path: `StopFailure` (no API error occurred); a hook waiting longer than its default timeout; input while a turn is running; the user's vim mode; a second terminal attached to the session; what the terminal shows at a usage limit.

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
- Hooks reference: the list of hook events and their payloads; `Stop` "does not run if the stoppage occurred due to a user interrupt"; `StopFailure` runs instead when a turn ends with an API error and names the error type; command hooks time out after 600 seconds by default; in an interactive session no hook runs until the folder-trust dialog is accepted.
- Permissions: trust is keyed on the repository root, and trusting a parent directory does not extend to a repository nested in it.
- Channels reference: a channel is an MCP server started by Claude Code; `notifications/claude/channel` pushes an event into the session; replies go through a tool the channel offers; permission requests can be relayed; custom channels need `--dangerously-load-development-channels`, which shows a warning dialog and is ignored with `-p`; notifications are not acknowledged.
- Agent view: `claude --bg` sessions run under a supervisor; `claude agents --json` reports their state; no way to send input to a running session other than interactively is described.

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

## Appendix D: interfaces in Go

Declared in `internal/control`, `internal/surface` and `internal/agent`; the packages hold the types this appendix leaves out. Names will move during phase 1; the split between the three packages should not. A pull request that changes one of the packages changes this appendix with it.

### Control API

```go
// Package control: everything a client can ask of the core.

type API interface {
	Threads(ctx context.Context, f Filter) ([]ThreadInfo, error)
	// Thread returns state, open requests and the last output, capped at recent bytes.
	Thread(ctx context.Context, id ThreadID, recent int) (ThreadDetail, error)

	Start(ctx context.Context, r StartRequest) (ThreadID, error)
	// key makes Send safe to repeat; for chat, the surface's message id.
	Send(ctx context.Context, id ThreadID, key string, in Input, busy Busy) error
	Answer(ctx context.Context, req RequestID, a Answer) error
	Stop(ctx context.Context, id ThreadID) error

	// Events delivers events after a position, then live ones.
	Events(ctx context.Context, f Filter, after uint64) (<-chan Event, error)
}

// Caller is who is asking. An API value is bound to one Caller,
// and the core checks every call against it.
type Caller struct {
	Principal Principal // a person, a webhook, a supervisor, a dashboard user
	Scopes    []string  // the scopes it may see and act in
	May       Ops       // any of OpRead, OpStart, OpSend, OpAnswer, OpStop
}

type StartRequest struct {
	Key    string // safe to repeat: the same key starts nothing twice
	Scope  string // opaque; for chat, a channel
	Thread string // opaque key, or empty to let the scope's owner make one
	Repo   string
	Input  Input
	Model  string
	Effort string
}

// Busy says what Send does when a turn is running.
type Busy int // Refuse, Deliver, Interrupt

// ScopeOwner is the client that gives a scope its threads. When another client
// starts a thread there, the core asks the owner for the key; a chat front end
// opens a chat thread and returns its id.
type ScopeOwner interface {
	NewThread(ctx context.Context, scope, title string) (key string, err error)
}
```

Sleep, close and remove join the interface with the phase that needs them.

### Surface driver

```go
// Package surface: a chat service.

type Driver interface {
	// Run connects and delivers inbound events to sink until ctx ends.
	// It reconnects by itself and calls sink in order per conversation.
	Run(ctx context.Context, sink Sink) error

	// Since returns the messages in a conversation after cursor,
	// for catching up after an outage.
	Since(ctx context.Context, conv Conversation, cursor string) ([]Inbound, error)

	OpenThread(ctx context.Context, parent Conversation, title string) (Conversation, error)
	// OpenDirect returns the private conversation with one person, given
	// by the person's id on the service.
	OpenDirect(ctx context.Context, who string) (Conversation, error)
	Post(ctx context.Context, conv Conversation, key string, m Message) (MessageRef, error)
	Edit(ctx context.Context, ref MessageRef, m Message) error
	Delete(ctx context.Context, ref MessageRef) error
	SetStatus(ctx context.Context, ref MessageRef, s Status) error        // lamp on a message
	SetThread(ctx context.Context, conv Conversation, t ThreadMeta) error // title, archived

	Limits() Limits // message length, choices per message, which optional calls work
}

// Factory makes a Driver from its opaque section of the instance's config.
// Register(name, f) fails when name is taken; Lookup(name) finds it.
type Factory func(settings []byte) (Driver, error)

// Sink is implemented by the chat front end, not by the core.
type Sink interface {
	Message(Inbound)  // text, files, author, ids
	Choice(Selection) // a button or menu was used
	Command(Command)  // a slash or text command
}

// Inbound is a received message. Direct says that its conversation is a
// private one with the author.
type Inbound struct {
	Ref     MessageRef
	Author  Author
	Text    string
	Files   []File
	ReplyTo *MessageRef
	Direct  bool
	Time    time.Time
	Cursor  string // pass to Since to get the messages after this one
}

type Message struct {
	Text    string
	Files   []File
	Choices []Choice    // rendered as buttons or a menu; each carries an opaque token
	ReplyTo *MessageRef // quote-reply
	Notify  []string    // ids of people to ping
	Quiet   bool        // no notification
}
```

### Agent adapter

```go
// Package agent: a coding agent.

type Adapter interface {
	// Probe reports whether the agent is installed and logged in.
	Probe(ctx context.Context) (Health, error)

	// Launch returns how to start the agent for spec: a new session,
	// or a resumed one when spec.ResumeID is set.
	Launch(spec Spec) (Launch, error)

	// NewCodec returns the translator for one running session.
	NewCodec(spec Spec) Codec
}

// LoginFlow is implemented by an adapter whose agent can be logged in again
// from chat. hataori runs the agent's own login command and passes on what it
// prints and the owner's code; it keeps neither.
type LoginFlow interface {
	// LoginLaunch returns the login command, with the same config directory
	// as sessions started from spec.
	LoginLaunch(spec Spec) (Launch, error)
	// LoginDecode reads one line the login command printed.
	LoginDecode(line []byte) LoginStep
	// LoginCode returns the bytes that hand a code to the login command.
	LoginCode(code string) ([]byte, error)
}

// LoginStep is what a line of the login command means: a link to show
// (with a code to show beside it, if the agent gives one), a request for a
// code, a refusal with its text, or nothing.
type LoginStep struct {
	Link      string
	ShowCode  string
	WantsCode bool
	Refused   string
}

// Launch is everything the session host needs to start an agent.
type Launch struct {
	Argv     []string
	Env      []string
	Terminal bool              // run on a pseudo-terminal instead of pipes
	Files    map[string][]byte // written into the session's state directory first
}

// Source says where a line came from: Output (the agent's output on pipes),
// Hook (a call from the agent's hook command) or File (a file the codec asked
// to follow).
type Source int

// Codec turns what an agent reports into events, and requests into what the
// agent accepts. It does no I/O, so it is tested against captured output.
type Codec interface {
	Open() Actions                          // what to do first, if anything
	Decode(src Source, line []byte) Actions // one reported line
	Input(in Input) (Actions, error)        // a user message
	Answer(a Answer) (Actions, error)       // to an approval or a question
	Interrupt() Actions
	// Tick gives the codec the time for its deadlines (input not reported
	// as delivered); the codec has no clock of its own.
	Tick(now time.Time) Actions
}

// Actions is what the caller of a Codec does next.
type Actions struct {
	Events []Event     // for the core
	Write  [][]byte    // to the agent's input (terminal or stdin), in order
	Reply  []HookReply // to hook calls that are waiting for an answer
	Follow []string    // files to start following, such as a transcript
}

type Spec struct {
	Dir          string // workspace
	StateDir     string // settings, attachments, extra instructions; outside the workspace
	ResumeID     string
	Model        string
	Effort       string
	Instructions string // appended to the agent's own
	Approvals    Policy // allow everything, or ask in chat
}

// Event is one thing the agent did. Kind is one of SessionReady, TurnStarted,
// Text, ToolStarted, ToolEnded, SubagentStarted, SubagentEnded, Request,
// RequestWithdrawn, FileOut, Usage, TurnEnded, SessionEnded, Unknown.
type Event struct {
	Seq  uint64
	Kind Kind
	// Plus the fields each kind needs: the text and whether it is final,
	// who started a turn, the request id and its questions,
	// the end status and reason, the background tasks still running.
	// SessionEnded has a reason; CannotResume ("cannot resume") when a
	// resumed session's conversation is gone.
}

// Factory makes an Adapter from its opaque section of the instance's config.
// Register(name, f) fails when name is taken; Lookup(name) finds it.
type Factory func(settings []byte) (Adapter, error)
```

## Appendix E: acceptance criteria after phase 1

Every phase here also has this criterion: the change touches no core package, except to add an operation to the control API or to register an adapter; the plugin phase may also add hook points.

### People in chat, and cleanup

- [ ] Slash commands start a thread, stop a turn and show status. A message, a command and a button press from a user who is not allowed each do nothing and are logged.
- [ ] A question appears with buttons; pressing one delivers the answer, also when the daemon was restarted in between.
- [ ] The sender's message carries a lamp for working, done and error, and the sender is pinged once when the turn ends.
- [ ] `hataori attach` opens the conversation in the agent's terminal UI, and a turn typed there is known to the next turn from chat.
- [ ] Removal refuses, with the reason, for each of: modified file, untracked file, ignored file not listed as disposable, unpushed commit, stash, commit only in the reflog, submodule with an unpushed commit, nested repository, remote that is another workspace, git error. It removes a clean, pushed workspace. Each test fails when its check is removed.

### Dashboard, read-only

- [ ] Without a signed-in session every route, including the event stream, answers 401. A request with an unknown `Host` is refused.
- [ ] Each thread in the user's scopes is a tile whose state and last output follow the events within 2 seconds. A thread outside those scopes is never sent to the browser.
- [ ] After a daemon restart the open page catches up with no gap and no repeat.
- [ ] Every request that is not a read is refused.

### Supervisor

- [ ] With every thread in its scope idle for an hour, the supervisor's session makes no model request.
- [ ] One turn end in its scope starts one supervisor turn; ten within the quiet period start one.
- [ ] Sending to a busy thread is refused. Interrupting happens only after a person approved that call.
- [ ] Attempts by the supervisor's session to write outside its directory, to run a command, and to remove a workspace each fail (tested).
- [ ] When the budget is used up no supervisor turn starts, and a person is told once. The loop guard stops a two-thread ping-pong at the set count.

### Codex

- [ ] A channel or thread can be set to Codex; start, continue, sleep and wake work as in phase 1.
- [ ] Approvals and questions from Codex appear in the thread and can be answered.
- [ ] After an interrupt, no command started by that turn is still running.
- [ ] A turn of 30 minutes completes.
- [ ] Captures of real `app-server` output are in `testdata/`, and the codec passes on them.

### Slack

- [ ] Phase 1's criteria for start, continue, questions, restart and catch-up pass on Slack.
- [ ] Interactions are acknowledged within Slack's deadline while the work continues.

### Microsoft Teams

- [ ] Phase 1's criteria for start, continue, questions, restart and catch-up pass on Teams.
- [ ] An activity with an invalid or missing token is rejected.

### Plugins

- [ ] An example plugin written in a language other than Go is in the repository, and the tests below use it.
- [ ] The plugin connects, reads events from a position and calls `Threads`. A call to an operation its grant lacks is refused and logged.
- [ ] A plugin is not started when the hash of its executable differs from the config, and it is refused when its protocol major version differs. Each log line names both values.
- [ ] A plugin killed during a turn is started again, the turn finishes, and the plugin reads each event it missed exactly once.
- [ ] A `message_sending` handler rewrites one post and cancels another. A `before_agent_run` handler stops an input before it reaches the agent, and the thread names the plugin and the reason.
- [ ] Two handlers on one point run by priority, then in config order. A handler that times out gets the point's default failure policy, and a per-plugin override in the config replaces it (each tested).
- [ ] Without the `conversation` grant, nothing sent to the plugin contains message text (tested with a marker string in the input, the agent's text and a post).
