# hataori design overview

Status: proposal for review. Nothing here is implemented yet.

hataori succeeds [c-lord](https://github.com/yousan/c-lord). It keeps c-lord's model: one chat thread is one agent session, and one agent session owns one working directory. This document says what hataori takes from c-lord, what it changes, and how it is built.

How to read it:

- Issue numbers written `c-lord#123` are issues of `yousan/c-lord`.
- c-lord facts come from its docs and code at v1.4.420.
- Claims about the agent CLIs were checked on 2026-11-12 against Claude Code 2.1.322 and codex-cli 0.163.0 on Linux, by driving the real programs, and the supervisor's confinement (section 7.1) on 2026-11-18 against Claude Code 2.1.327 and codex-cli 0.163.0. Appendix B lists each check.
- Section 3 first recommended `claude -p`. The maintainer decided against it; sections 3.4 and 3.5 describe the path chosen instead, and the earlier checks stay in the document as the record.
- OpenClaw facts (section 7.3) come from its documentation for version 2026.10.6, read on 2026-11-12.
- Slack and Microsoft Teams facts in section 8, and the source notes in Appendix B's Codex rows, come from documentation and source code read on 2026-11-18 (Appendix B); they were not run.
- The tmux facts behind typing into a host pane (section 7.2) were checked on 2026-11-19 against tmux 3.4 on Linux (Appendix B). The SSH and browser facts there (OpenSSH's connection sharing and forwarding, `localhost` as a secure origin, cookies shared across ports) were not run or read again for it, and count as **(unverified)** until the issues that use them check them.
- The facts behind terminal holders and taking in sessions people started (sections 4.3 and 4.8) were checked on 2026-11-19 against Claude Code 2.1.328 and tmux 3.4 on Linux, with a throwaway configuration directory and no login (Appendix B). cmux and iTerm2 run only on macOS: their facts come from their documentation, read the same day, and are **(unverified)**.
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
9. **Two things have no hook:** the folder-trust dialog at a workspace's first start, and the end of an interrupted turn. Section 3.5 says how each is handled; the first is answered by keys before the session starts (decided, section 10).
10. **hataori is added to the terminals people already use, not a replacement for them.** Its own session host stays the default, but holding a terminal is a replaceable part, so tmux, and on macOS cmux and iTerm2, can hold a session too. With a Claude Code plugin the person installs, a session they started in their own window reports through the same hooks and can be taken in as a thread, explicitly, by its owner (sections 4.3 and 4.8; decided 2026-11-19, section 10).

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
| tmux window bookkeeping: which window is which thread, twins, reaping | 32 | c-lord#649, c-lord#701 | hataori's own session host by default: one process per session, tracked by the database. A tmux pane holds a session only by the pane id that the session's own hook reports, never matched by name or screen (sections 4.3 and 4.8). |
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
| Input | One bracketed paste into the terminal, then Enter. Delivered means the `UserPromptSubmit` hook reported the same text. | Yes: 6,188 characters in 64 lines, with a leading `-`, a trailing `;`, Japanese, backticks and quotes, arrived unchanged (inside a `pasted_content` tag). A tab arrives as four spaces (2.1.328, #393), so tabs count as four spaces when the texts are compared. |
| Interrupt | The Escape key. | Yes: the running command was gone |

Each hook is the command `hataori hook`. It hands its payload to the session host and, for the two hooks that decide something, waits for the reply. The screen is never read to learn state.

**Permission requests in phase 1** are answered "allow" by the hook. This gives c-lord's behaviour without starting Claude Code in its bypass mode, which shows a warning dialog of its own (c-lord#695; not re-checked on this version). "Ask in chat" is later the same path with a person behind the reply.

**Codex:** `codex app-server` over pipes, one process per session, `thread/start` with `cwd`, then `turn/start`, `turn/steer`, `turn/interrupt`, `thread/resume`. Unchanged.

**Attach by hand.** A Claude Code session is a real terminal program, so a person can be connected to it: `hataori attach <thread>` relays their terminal to the session's through the host, to watch and to type while it runs. What they type is reported by the same hooks and shows in the thread. `hataori tail <thread>` is the read-only form. The host owns the terminal directly; tmux is not needed, and none of c-lord's window bookkeeping comes back. Attach is after phase 1 **(not built, not run)**.

**Two alternatives are kept as spikes**, separate from the adapter, because either could replace paste as the way in:

- *Channels.* A channel is an MCP server that Claude Code starts; it pushes messages into the session and offers a reply tool, and can relay permission requests. It is a research preview: a custom channel needs `--dangerously-load-development-channels`, which shows a warning dialog at start, and delivery is not acknowledged (from the docs; not run).
- *Background sessions.* `claude --bg` runs a session under Claude Code's own supervisor, `claude agents --json` reports its state, and `claude attach` opens it. The docs name no way to send input to a running one without a terminal (from the docs; not run).

### 3.5 What hooks do not cover, and other risks

- **The folder-trust dialog.** The first time the interactive CLI starts in a repository, it asks whether the folder is trusted, with the cursor on "No, exit". Until that is answered no hook runs at all (documented, and seen: zero hook calls in 12 seconds). Trust is recorded per repository root, and trusting a parent directory does not cover a clone inside it (documented). So every new workspace meets the dialog, and no hook can report it. Claude Code 2.1.326 offers no supported way to trust a directory in advance (#56). **Decided:** hataori answers this one dialog by keys, before `SessionStart` only (section 10). The adapter asks to watch the screen until the session starts. It recognises the captured dialog by its text, for the workspace's own path, and waits until the screen has settled: right after drawing it, the CLI asks the terminal what it supports, and a key sent then was lost. Then it moves the cursor one line at a time toward the line that says yes, found by its text, and confirms once the screen shows the cursor there. Any other screen gets no key, and the start fails as below. When the folder holds files that trusting it loads, Claude Code 2.1.328 names them on one more line, such as `Trusting this folder loads .claude/settings.json.` or `... .claude/settings.local.json (runs commands and sets environment variables).` (#423). In a confined session the adapter answers that dialog whatever it names, since the launch ignores those files. In an ordinary thread it answers only when the scope's `trust_files` lists every file named; otherwise it sends no key, and the start fails at once as `blocked at start`, with the files named in the state's detail.
- **Any other start-up dialog** (a login prompt, a consent prompt for a new MCP server) has the same shape: nothing is reported. The rule: a session that has not reported `SessionStart` within the start timeout is `failed` with reason `blocked at start`, with its terminal output attached for a person to read. hataori does not guess and does not type.
- **An interrupted turn fires no hook** (documented, and seen). Its end is read from the transcript, which gets a line saying the request was interrupted. This is the one piece of turn state that rests on the transcript.
- **The transcript is not a documented format** as far as we found **(unverified)**. Hooks are: their payloads are specified field by field. So the split matters: turn state, questions, failures and the final answer come from hooks; only text before the final answer, and the interrupt line, come from the transcript. If the transcript changes, progress text degrades and nothing else.
- **A hook that waits for a person.** Command hooks time out after 600 seconds by default (documented). A question may wait for hours, so the hook's timeout must be raised, and what happens when it still expires must be known **(unverified)**.
- **Input while a turn is running** was not checked on this path **(unverified)**. Until it is, hataori holds such input and pastes it when the turn ends.
- **Paste depends on the terminal UI's input box.** It was checked in the default editing mode only; the user's own settings (vim mode) are **unverified**.
- **A tab in a paste arrives as four spaces.** Claude Code 2.1.328's input box turned each tab of a bracketed paste into four spaces, on its own pseudo-terminal and through tmux alike (Appendix B). The codec compares the pasted text with the prompt the hook reports after taking a long paste out of its `pasted_content` tag (section 3.4), trimming, line-ending changes and each tab as four spaces, so such an input is matched and delivered (#393, `testdata/2.1.328/paste_tabs`). A prompt that differs from the paste in any other way is still not counted as the input.
- **Whether the usage-limit menu still needs an answer** on screen when a limit is hit is **unverified**; c-lord answers it by keys.
- **Codex `app-server` is marked experimental** in its own help text. Captures per version, unknown messages skipped.
- **Codex interrupt leaves the command running** (observed). hataori ends it with `thread/backgroundTerminals/clean` (#317 `interrupt`). This ends every background terminal of the thread, including commands moved to the background in earlier turns.

For every version of an agent CLI: decode against captures of that version's real output, skip what is unknown, and run the capture suite before the version is used for real work.

## 4. Core model and lifecycle

### 4.1 Three objects

- **Thread:** one line of work. Identified by its **number**, which hataori gives it (`threads.id`), unique in the instance. It belongs to exactly one scope, an opaque string from the config; the scope decides who may act in the thread, its default model and where it may be shown. It is shown in chat through any number of **links** (section 5.2). The core never looks inside a scope or a link.
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

### 4.3 The session host, and terminal holders

Each agent is started by a small host process (`hataori host`, the same binary). The host:

- starts the agent in the workspace, in its own process group, on a pseudo-terminal or on pipes, whichever the adapter asks for;
- takes what the agent reports and appends it, one line each, to `events.ndjson` in the session's state directory. For an agent on pipes that is its output. For an agent in a terminal it is the calls its hooks make (`hataori hook`), and when a hook waits for an answer, the host holds the call open and passes the answer back;
- accepts input on a Unix socket and writes it to the agent's terminal or stdin;
- keeps the terminal's raw output in a bounded log, for a person to look at and for `attach`. Nothing reads it to decide anything;
- records the exit status when the agent ends.

**Attach.** `hataori attach <thread>` connects the owner's own terminal on the host to the agent's: the control API's `AttachTerminal` (owner only) wakes an asleep thread with no input and names the host's socket, and the command then talks to the host directly, so a daemon restart ends neither the agent nor the attached terminal. The host sends the end of its terminal log, then the output as it comes, and writes the person's keys to the terminal; Ctrl-\ detaches and is not passed on. The host counts attached terminals and reports when the last one went, and the runner does not put a session to sleep while one is attached, nor within the idle period after it went. A turn typed there is reported by the agent's hooks like any other, with `by` `terminal`; chat posts its final answer after a line "(typed at the terminal)", and chat input while someone is attached is delivered as usual (section 10). Both reach the agent's one input line, so a chat message that arrives while a person has typed part of a line is mixed with it, and the agent may get both as one prompt **(unverified with Claude Code)**. The screen shown first is the end of the raw log, replayed; it is exact once the agent draws again, since the host interprets nothing. The agent's terminal keeps its own size.

The host does not parse or interpret anything, so it rarely needs to change. It is detached from the daemon. When the daemon restarts, running turns continue; the daemon reconnects to the socket and reads the log from the byte offset it last stored. A hook that is waiting for an answer keeps waiting across the restart.

Checked: a `claude -p` process with stdin on a FIFO and stdout on a file finished its turn after the process that launched it had exited, and accepted a second message afterwards. The interactive CLI was run in a terminal with its hooks writing to a file (Appendix B). The host itself is new code and is the first thing phase 1 must prove.

**Terminal holders.** The host does two jobs. It holds the agent's terminal, and it relays what the agent reports: the log, and the hook calls that wait for an answer. Relaying stays with hataori's host whatever holds the terminal, so turn state, questions and permission requests keep one path and survive a daemon restart as described above. A host can therefore also run with no command, as a relay only, for a terminal that something else holds. Holding the terminal is a replaceable part, the *terminal holder*. hataori's own host is the default holder for every session hataori starts, and the reference that every other holder is measured against (decided 2026-11-19, section 10). One holder is added at a time, and each one's input path is checked against the real agent before it ships.

A holder has five operations. The right-hand column is what hataori's own host does today.

| Operation | What it does | hataori's own host |
|---|---|---|
| Start | starts the agent in the workspace, with exactly the arguments, environment and terminal size given, in a process group of its own; returns a reference to the window, which the core stores with the session | opens a pseudo-terminal with `creack/pty` and starts the command in a new session on it |
| Input | delivers pastes and named keys, in order | a paste is `ESC [200~`, the text, `ESC [201~`; a key comes from a fixed table: Enter `\r`, Escape `ESC`, Up `ESC [A`, Down `ESC [B` |
| Screen | shows what the agent draws, as raw output with byte positions or as rendered snapshots | raw output, positions counted from the start of the host's run (section 7.2) |
| Stop | ends the agent's process group, interrupting first, and leaves no process behind | SIGINT, then SIGTERM, then SIGKILL to the group |
| Alive | says whether the agent runs, has ended (with its exit status when the holder knows it), or is gone, or that the holder cannot tell | the host's socket and `status.json` |

What every holder must provide:

- *Input.* A paste reaches the agent as one bracketed paste when the agent has asked for bracketed paste, with its bytes unchanged: no line feed turned into a carriage return, nothing re-encoded. A named key reaches the agent encoded for the terminal's current mode. Text never appears on a command line, where other users of the machine could read it in the process table. Nothing is sent that the codec did not ask for. An input counts as handed over once the holder has given all of it to the terminal; whether the agent took it is still decided by the agent's hook (section 3.4), never by the screen.
- *Screen.* Either raw output from the agent's first byte, with positions that survive trimming the log, or rendered snapshots. Raw output is what the start-up dialog watcher reads (section 3.5), and how the codec sees that the agent has turned bracketed paste on. A holder that has only snapshots therefore cannot start sessions; it holds only sessions that are past their start (section 4.8). Whatever the holder, nothing reads the screen to decide anything else.
- *Stop.* Only a holder that started the agent stops it. A window hataori did not start is never stopped by hataori; it can only be let go (section 4.8).
- *Alive.* The answer comes from the holder's own records, never from the screen. "Cannot tell" is not "gone": a session is put asleep or marked as exited only on a definite answer.
- *Identity.* A window reference is never reused for another window, even after the holder itself restarts.

The holders, in the order they come:

| Holder | Start | Input | Screen | Stop | Alive | Checked |
|---|---|---|---|---|---|---|
| hataori's own host | yes | bytes to its pseudo-terminal | raw | yes | socket and status file | yes |
| tmux, a window a person started (section 4.8) | no | `load-buffer -b <own> -`, then `paste-buffer -p -r -d -b <own>`, with `-S` from tmux 3.7 on; `send-keys` with tmux's key names | snapshots (`capture-pane`) | no | `list-panes`, and the process the hooks named | yes, Appendix B; built (#399) |
| tmux, a window hataori starts | yes | as above | raw, through `pipe-pane -O` | yes | `remain-on-exit` and `#{pane_dead_status}` | the parts, Appendix B; the whole is not built |
| cmux (macOS) | **(unverified)** | `cmux paste`, `cmux send-key` | `cmux read-screen`, text | `cmux close-surface` | `cmux surface-health` | documentation only |
| iTerm2 (macOS) | **(unverified)** | Python API `Session.async_send_text` | `Session.async_get_screen_contents` | **(unverified)** | **(unverified)** | documentation only |

*tmux.* A window is named by the server and the pane: `TMUX` holds the server's socket path and process id, and `TMUX_PANE` the pane id. The server's process id is part of the reference because pane ids start again at `%0` on a new server (Appendix B). Input follows the path chosen for host panes in section 7.2, with two changes: `paste-buffer` replaces every line feed with a carriage return unless it is given `-r`, so a paste for an agent always uses `-r`; and from tmux 3.7 on it shows a pasted control character as `^X` unless it is given `-S`, so a paste for an agent uses `-S` where the server knows it. `display-message` on a pane that no longer exists exits 0 with empty fields, so whether a pane exists is read from `list-panes`. The process tmux starts in a pane leads its own process group, so Stop signals that group as the host does. For a window hataori starts:

1. A relay host with no command starts first, so the first hook call finds its socket, as today.
2. The pane is created in a tmux session of hataori's own name, on the person's default tmux server, with a placeholder command. The person sees the window among their own and can switch to it; what they type there is reported by the hooks as typed at the terminal (Attach, above).
3. `remain-on-exit` is set on the pane, so that its exit status can be read after the agent ends, and `pipe-pane -O` sends the pane's output to a `hataori` process that writes the bounded raw log the host writes today.
4. `respawn-pane -k` replaces the placeholder with the real command. The pipe stays on the pane and gets the first byte (Appendix B). The command is a small `hataori` command that reads the spec from the session's state directory and executes the agent with exactly that environment. Without it the pane would inherit the tmux server's environment, and passing values with `-e` would put them on a command line.

Choosing tmux for the sessions hataori starts is a config option, later (section 10).

*cmux*, from its documentation (Appendix B; nothing run). Each terminal, a "surface", has `CMUX_SURFACE_ID` in its environment, which stays the same for the terminal's whole life, across a restore of the app, and `CMUX_SOCKET_PATH`. `cmux paste` sends text unchanged through the app's paste path; the terminal brackets it when the program has asked for bracketed paste, and "replaces unsafe control bytes with spaces". `cmux send-key` sends one key. Both refuse to write over a prompt that a person is typing, or into an open dialog, unless forced. `cmux read-screen` returns text. cmux also starts `claude` through a wrapper of its own, which registers hooks with `--settings` and shows permission requests in cmux's own feed. So in a cmux window two programs could answer one permission request, and the cmux issue decides which one does before anything ships. It is not known whether cmux can give raw output from the first byte, so cmux starts as a holder for windows people started only.

*iTerm2*, from its documentation (Appendix B; nothing run). The Python API, which a person has to turn on in iTerm2's settings **(unverified)**, has `Session.async_send_text`, documented as "Send text as though the user had typed it", and `Session.async_get_screen_contents`. No paste method is documented. A session's identifier is `Session.session_id`; the environment variable `ITERM_SESSION_ID`, of the form `w0t0p0:<UUID>`, is known from third-party code only. The first thing its issue checks is whether text sent this way can carry the bracketed-paste marks, so that a prompt of several lines reaches Claude Code as one paste and not as lines typed one by one.

Orca is not a holder. It is an orchestrator at the same level as hataori, and its terminals are listed like any other terminal (section 10).

**The first step changes nothing a session does.** Today the runner calls the host package directly. The first step puts those calls behind the holder operations, with hataori's own host as the only holder, and changes the codec of an agent in a terminal to return pastes and named keys instead of bytes. The own host writes exactly the bytes written today. An agent on pipes, Codex, keeps writing its bytes, and hataori's own host stays its only holder. Done in #395; Appendix D has the interface.

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
2. Every commit reachable from any ref, from `HEAD`, from the stash and from the reflog is on a remote. The remote is asked at that moment; remote-tracking refs are not trusted (c-lord#1245). A remote that is itself a workspace, or lies under any hataori root, does not count (c-lord#1265, c-lord#1289). A local address is resolved the way git resolves it: a relative path from the repository, `~/` from `$HOME`, symlinks before `..`, the `.git` names upload-pack also tries, the git directory a `.git` file names and the common directory a linked worktree's git directory names. An address that cannot be resolved that way, or that `url.<base>.insteadOf` would rewrite again when it is fetched, does not count either. A local remote's objects directory and its `objects/info/alternates` are followed as git follows them (relative to the objects directory, `#` comments, quoted paths, depth first, six levels deep), because git serves their objects as the remote's: one that lies in a root, or passes through one on the way, makes the remote not count, and so does one that cannot be resolved, is not a directory, or lies deeper than git reads. Below each of those objects directories, the links git reads through are checked the same way: everything below `pack/` (a multi-pack-index may name a pack below it) and `info/`, and the lower-case fan-out directories with their loose objects. A link there into a root, through one, or that cannot be resolved makes the remote not count, and so does more than about a million entries in all.
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

- **Only the owner's direct message is ever passed to the login command, and only while the command is waiting for a code.** A login that takes no code back is passed nothing. The owner is the person of section 5.5, reached at their id on each client; the relay resolves the author's `<client>:<id>` and compares the person. A direct message from anyone else, a guest included, does nothing. The owner's message at any other time gets "not waiting for a code". A reply that is not shaped like a code (one word, no spaces) is not passed on.
- **The link and the code exist only in the direct message and in memory.** They are not written to the database, the event log, any log file or any other conversation, and no client of the control API can read them. The log says that a code arrived and how long it was. For this reason the login command is a direct child of the daemon and does not run under the session host, which logs what it sees. A daemon restart ends a login that is in progress; the button starts another.
- **Nothing is resent.** A message that failed is sent again by whoever sent it.
- **If someone logs in on the host instead**, the next turn that completes closes the outage, the waiting login is stopped, and the message says the login is back.
- The relay can be switched off in the config; the alert stays.

This is the only input hataori takes from direct messages in phase 1, and the only button.

For Claude Code the command is `claude auth login`. It was run with a throwaway config directory, so the existing login was not touched: it works on pipes without a terminal, prints the link on one line and then asks for the code, and answered a wrong code with one line and kept waiting (Appendix B). What it prints and how it ends after a correct code was not run **(unverified)**. Codex runs `codex login --device-auth` with the sessions' `CODEX_HOME`; it prints a link and a code to enter on the page (captured in 0.163.0) and takes no code back. The owner's direct message shows both and asks for nothing. The command ends by itself; success is its clean end and `codex login status`, as for Claude Code. The end was not captured: Codex's source reports `Successfully logged in`, or `Error logging in: <error>` and exit 1, which the owner is shown.

### 4.8 Taking in a session a person started

People who already run many Claude Code sessions run them in their own terminal tools, and will not switch tools for hataori. So hataori is added to what they run, in three steps (decided 2026-11-19, section 10):

1. Without installing anything, terminals that hataori does not own are listed in the dashboard (section 7.2).
2. With a Claude Code plugin that the person installs, a session they started reports through hooks, as hataori's own sessions do, and can be taken in as a thread. From then on chat, the supervisor and the dashboard treat it like any other thread.
3. hataori can start a session inside the person's tool (section 4.3, tmux).

This section is step 2, and Claude Code only. c-lord could attach a thread to a window a person had made, and hataori dropped that (Appendix A), because c-lord matched windows to threads by name and read their screens. Neither comes back: the window is named by the environment of the session's own hook, and the state comes from the hooks.

**The plugin.** `hataori plugin` writes a Claude Code plugin into the instance's root, in a directory that is also a plugin marketplace, and prints the two commands that install it: `claude plugin marketplace add <directory>` and `claude plugin install <plugin>@<marketplace>`. The person runs them. hataori writes nothing into Claude Code's configuration itself, as for its own sessions (section 3.4). What installing does (Appendix B):

- It records the marketplace and the plugin in the user's settings file (`extraKnownMarketplaces`, `enabledPlugins`) and in the `plugins/` directory of that configuration directory.
- Installed at user scope, the plugin's hooks run in every session of that configuration directory: in every project, next to hooks given with `--settings`, and under `claude -p`. They do not run when `--setting-sources` leaves out `user`, nor under `--restricted`, which ignores the user's settings file and so the plugin's `enabledPlugins` entry in it (a supervisor's session, section 7.1), nor in a session that uses another configuration directory.
- A plugin's hooks run in a session that is not logged in, so all of this can be checked with a throwaway configuration directory.
- For a marketplace that is a local directory, the hooks ran from that directory, not from the copy Claude Code keeps in its cache. A plugin rewritten by a newer hataori then takes effect at the next session start, without reinstalling. Whether this holds in later Claude Code versions is **(unverified)**.

The plugin's files name the instance's root and the `hataori` binary by absolute path, so it has no option to set. (Claude Code plugins can declare options, which reach hooks as `CLAUDE_PLUGIN_OPTION_<NAME>`, Appendix B; this plugin needs none.) It registers the hook events of hataori's own settings file (section 3.4), `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `PostToolUseFailure`, `PermissionRequest`, `SubagentStart`, `SubagentStop`, `Stop`, `StopFailure`, `PreCompact`, `PostCompact` and `SessionEnd`, each with the command `hataori hook --window`. The two that decide something, `PermissionRequest` and `PreToolUse` for `AskUserQuestion`, also get `--wait` and the configured hook timeout. It registers nothing else: no status line, tool, skill or MCP server.

**What `hataori hook --window` does**, in this order:

1. In a session that hataori started, whose environment has `HATAORI_HOST_SOCKET`, it exits 0 at once and prints nothing. hataori's own hooks report that session; both sets run in it, and the plugin's hook sees the variable (Appendix B).
2. It names the window from its own environment: `TMUX` and `TMUX_PANE` for tmux; later `CMUX_SURFACE_ID` with `CMUX_SOCKET_PATH` for cmux, and `ITERM_SESSION_ID` for iTerm2. It finds the Claude Code process as its nearest ancestor that has a terminal: Claude Code runs a hook under a shell of its own that has none (Appendix B).
3. If that window has been taken in, by this Claude Code process, it does what `hataori hook` does today, with the thread's relay host (section 4.3). The take-in record that says so is a file in the instance's runtime directory, next to the relay's socket, so this works while the daemon is down.
4. Otherwise it sends the daemon a short report and exits 0 with nothing printed, within a second, whether the daemon answers or not. The report holds the window, the session id, the working directory, the configuration directory when one is set, the event's name and time, the model, and whether the session is interactive: `CLAUDE_CODE_ENTRYPOINT` was `cli` in an interactive session and `sdk-cli` under `-p` (observed; not documented). It holds no prompt, no message and no tool input.

So the hooks of a session that was not taken in do one thing: they put it in the dashboard's list. They never reach an agent, a chat, the supervisor or a plugin. Nothing of them goes into the event log or the database; the daemon keeps the list in memory and builds it again from the next reports after a restart. And they never hold a permission request or a question: the hook returns at once with no answer, so Claude Code shows its own dialog in the window, as it would without the plugin. That a hook which prints nothing and exits 0 leaves the decision to Claude Code's normal permission flow is documented; it was not run here **(unverified)**. The exit status is always 0, also when the daemon cannot be reached, since any other status puts a hook error notice in the person's window (Appendix B).

**The list.** In the dashboard, a tmux row whose pane has reported also shows the session id, the directory, the last event and its time (section 7.2). These are reported, not inferred. Only interactive sessions are listed, and a report for a window that is not among the host's terminals is dropped.

**Taking a window in** is explicit: one window at a time, by its owner, with the scope chosen at that moment. Hooks reach the daemon only through a socket that only the instance's OS user can open, so a window that reports is one of that user's, and its owner is the instance's owner (section 5.5). The owner takes a window in with `hataori take <window> --scope <scope>` on the host, and later from the window's row in the dashboard.

1. hataori asks the holder whether the window still exists, whether the terminal of the Claude Code process that the hooks named is the window's terminal, and whether that process is the window's foreground process (Appendix B). This check is needed because an environment is inherited: a `claude -p` that the session's own shell starts sees the same `TMUX_PANE`. If any answer is no or unknown, nothing changes.
2. In one transaction the core makes a thread in the chosen scope, as `Start` does, with its first link opened by the scope's owner (section 5.2). The thread's session is the window's session id, its holder is the window, and its workspace is the window's directory, recorded as not hataori's (below). Its title is the window's name, or the directory's name when the window has none.
3. A relay host with no command starts for the session (section 4.3), and the take-in record is written. From the next hook on, the window reports to the thread.

A thread taken in is like any other:

- Input from chat or the control API is pasted into the window by the holder, and counts as delivered when the hook reports it, as always. The person can still type in the window at any time. Such a turn is reported as typed at the terminal and appears in chat with that line, as with `hataori attach` (section 4.3). A chat message that arrives while the person has typed part of a line can be mixed with it, as with attach **(unverified)**.
- Questions and permission requests follow the rules of the thread's scope, from the next hook on. Today that means permission requests are allowed automatically (section 10). `hataori take` says so before it acts.
- The codec of a session taken in starts the way a codec rebuilt after a restart does: ready, and without watching the screen, since the session is past its start.
- Stop interrupts the running turn with Escape. Nothing hataori does ends the window's process.

**Letting go.** `hataori release <thread>` ends a take-in. The take-in record and the relay host go, the thread goes asleep, and the window's hooks go back to listing only. The window itself is not touched.

**When the window closes.** When the window's Claude Code ends, its `SessionEnd` hook reports it: Claude Code ran that hook with reason `other` when its pane was killed and when the whole tmux server was killed (Appendix B). A process killed so hard that no hook ran is found by the holder's Alive. Either way the thread goes asleep with reason `window closed`, and the relay host exits. The next input resumes the session under hataori's own host, in the same directory, with hataori's own launch (section 3.4): `--resume`, the model the session last reported, and the configuration directory the window used. The options the person started it with are not known, and are not reused.

If the person opens the same session again in a window, its `SessionStart` names a session id that a thread knows. The window is listed with that thread's number, and can be taken in again, to the same thread. It is never taken in automatically. Nothing prevents the person from opening the session in a window while hataori's own host runs it. What Claude Code does with two processes on one session is **(unverified)**, and the dashboard marks such a window.

**The directory is the person's.** The workspace of a thread taken in was not cloned by hataori, and may hold the only copy of someone's work. No removal path touches it: `hataori remove` of such a thread removes hataori's records and the session's state directory only, and the removal pass (section 4.6) skips the directory. Nothing is written into it, as for every workspace.

**One instance.** The plugin reports to the instance that wrote it. A person who runs two instances installs the plugin of the one that should see their windows. With both installed, both could take in the same window, and nothing coordinates two instances (section 6.4).

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

Every call is made as a `Caller`: who it is, which scopes it may see, and which operations it may use. The core checks each call against it. A scope has one owner, the client that gives it its threads; when a thread is started there, the core asks the owner for the thread's first link, and a chat front end answers by opening a chat thread. Appendix D has the Go declarations.

**A thread is named by its number** in every operation and event: `Thread`, `Send`, `Stop`, `Link` and the dashboard's operations take it, `Start` returns it, and every event carries it with the thread's scope. Chat ids appear only in links. A client that knows only a chat conversation finds the number through the link: `Threads` with a filter that names the link (client, channel, thread) returns the one thread linked there, or nothing. `Link` (section 5.2) adds a link to a thread.

**Whose input reaches the agent.** A client that relays input from a chat service says where it came from: `Caller.From`, an id of the form `<client>:<id>` (section 5.5). The core resolves that id to a person; the client does not. Then, for `Start`, `Send`, `Answer` and `Stop`, the core allows the call only when the person is the owner or is listed in the scope of the thread (for `Start`, the scope asked for; for `Answer`, the scope of the request's thread). An id that no person lists is a guest, and a guest is refused every one of the four. A refusal changes nothing and writes one log line with the operation, the scope, the client and the id, and the person when there is one. This is the only check of who may give input, for every client: a chat front end, a second surface, a driver in another process, the `hataori` command. A caller that the daemon authenticates itself, such as the OS user on the control socket, which is the owner, names the person directly and has no `From`. Reads (`Threads`, `Thread`, `Events`) are checked against the caller's scopes as before; they give the agent nothing. A caller with `Caller.ReadAsPerson` has its reads checked like its input, so that the chat front end's status command shows a guest nothing (#256): a read in a scope the person may not act in is refused and logged the same way, and a list of every scope leaves those scopes out.

Every record of input keeps both the person and the id it came from (section 6.2).

Sleep joins the interface with the phase that needs it. Close and remove joined it in the cleanup phase (#255): `Close` and `Remove`, checked like `Stop`, and given only to the owner's connection on the control socket, so they are reached through `hataori close` and `hataori remove` on the host; besides them, only the removal pass that the config's `cleanup.remove_closed_after` turns on (off by default) removes, and only threads closed longer than that. Nothing in chat closes or removes in that phase, and no thread is closed by its idle age. There is no operation to reset or merge anything, for any caller.

### 5.2 Chat front end and surface driver

Showing a thread in chat is one client, shared by all chat surfaces. It turns inbound chat into `Start`, `Send`, `Answer` and `Stop`, each with `From` set to `<client>:<author's id>`; it reads `Events` and decides what to show; it owns the outbox.

The chat front end makes no allow check of its own and keeps no list of people, owners or posters: whether a message reaches the agent is decided by the core (section 5.1). The front end still drops what is not input from anyone: its own posts, posts by bot accounts and messages the service writes itself; that compares no id with a list. A trigger's chat thread is opened only once the core accepted the `Start`: the front end leaves `StartRequest.Thread` empty, and the core, after its check, asks the scope owner for the key with the call's key, so the front end opens the thread from the trigger message. A guest's trigger therefore opens nothing. Underneath it, a surface driver knows one chat service and nothing about agents or threads of work.

The driver's calls are: run and deliver inbound messages, button presses and commands; fetch messages since a cursor; open a thread; post, edit and delete a message; set a status on a message; set a thread's title and archived flag; report its limits. A message to post says what to show (text, files, choices, reply target, who to notify), not how. A driver can also open a direct conversation with one person; in phase 1 that is used for the owner only (section 4.7), at the owner's id on that client. Whether a direct message is the owner's is decided by resolving its `<client>:<id>` with the people config, as the core does: the front end keeps no id of the owner of its own, and the relay resolves the id again before it passes a code on.

The driver splits long text, picks buttons or a menu, and maps a status to whatever the surface has (a reaction, an edit). Its limits let the front end avoid asking for what a surface cannot do.

**Links.** A link is where a thread is shown in chat: the client's name, the channel, and the thread on that service (the conversation), with who made it and when. A thread has any number of links, at most one per conversation, and a conversation links to at most one thread. An inbound message in a conversation is resolved to a thread through its link: the front end looks the link up (by `Threads`, or from what it keeps of its own links) and sends to that number. A message in a conversation with no link is not for hataori, as today.

- **The first link** is made when the thread starts: the scope's owner opens the chat thread, from the trigger message when there is one, and the core stores it with the thread in the same transaction.
- **A second link is attached by hand**, from the channel that should show the thread: `/attach <number>` there, or `<trigger> attach <number>` on a surface without commands. The front end calls `Link` with that channel and no conversation; once the core allows the link, it asks that client, as it asks a scope's owner at `Start`, to open a chat thread in the channel, titled like the thread, so a refused attach opens nothing. Used inside a chat thread, `/attach` links that chat thread. hataori never opens a link in another channel by itself. *Why:* the person who attaches is looking at the channel that will show the work, so nobody gets a copy of a conversation they did not ask for; an automatic link would post every thread of a scope into every channel allowed to show it, and would need a rule for which channel of several; attaching also works for threads that started before the channel was allowed.
- **A link in another channel needs the config.** A thread belongs to one scope, and a link outside the scope's own channel is made only when the scope's config lists that channel in `show_in`, as `<client>:<channel>`. Without the entry no link is made, nothing is posted there, and the refusal is logged with the thread, the client and the channel. So a thread is never copied into a channel with a wider audience by accident.
- **The same people.** `Link` is checked like `Send`: the person (section 5.5) must be allowed in the thread's scope; the channel the link is in does not widen or narrow that. Every input through any link goes through the same check.
- **Every link reads the same conversation.** Besides the agent's output, each link shows the input and answers given through the other links, from the `input` and `answer` events, which name the link and the person they came through: one line each, `<person>: <text>`. The link the input came through does not get it again. A message that did not reach the agent (a guest's, one refused, one in a closed thread) produces no event and is not copied to any link.

### 5.3 Agent adapter

An agent adapter has two parts. The first says how to start the agent for a session, new or resumed: the command, whether it needs a terminal or pipes, and any files to put in the session's state directory first (for Claude Code, the settings file that registers its hooks). It also says whether the agent is installed and logged in, and, if the agent can be logged in again from chat, how to run its login command and how to read the link it prints. The second is a **codec**: it turns one reported line into events, whether the line came from the agent's output, from a hook call or from a file the codec asked to follow; and it turns a user message, an answer or an interrupt into bytes for the agent's input or into the reply to a waiting hook. The codec does no I/O.

Splitting the agent adapter into a launch description and a pure codec is what makes the repository's test rule practical: a capture of real output goes in `testdata/`, and the codec must turn it into the expected events. The session host and the core never see agent-specific bytes. The same interface fits an agent on pipes that speaks a protocol and an agent in a terminal that reports through hooks, which is how the Claude Code adapter could be changed without touching the core.

`TurnEnded` carries a status (`completed`, `interrupted`, `failed`) and a reason (`limit`, `login`, `api_error`, `crashed`, `blocked at start`, `not delivered`). `SessionEnded` carries a reason too; `cannot resume` means the agent no longer has the conversation, and the core starts a new session in the same workspace. `TurnStarted` says who started it (a caller, the agent itself after a background task, or a person at the terminal). These two fields replace most of the "guessing turn state" group.

Each user message the core hands to a codec carries an id. The codec returns that id in the `TurnStarted` of the turn that reports the message, and leaves it empty when it does not know. The core keeps a message it accepted until a turn reports its id, and hands it to the agent again after a daemon restart until then, so a message is delivered exactly once even when the codec holds it until the agent can take it.

Whatever call of the codec produced it, the actions that write a message name its id. The core records the message as written just before it writes it. After a restart, a message written and not yet reported is handed to the new codec marked as written, before any new line is read. The codec records it and writes nothing, so the report that comes is matched to it. If no report comes, the codec ends its turn as failed, `not delivered`, naming the id. A daemon that dies between the record and the write leaves a turn that fails that way, never a message run twice or lost without a trace.

### 5.4 Did the first draft allow chat, supervisor and dashboard on one API?

No, not as written. The agent side is unchanged. The surface side was a plug-in that the core called: the core itself received every inbound chat event (the `Sink` interface), owned the outbox, and pushed finished messages to one surface per thread. Nothing in it could list threads, read a thread's state, or follow events, which is all a dashboard does and most of what a supervisor does. And a thread was identified by `(surface, channel, thread)`, which the core had to understand.

What changed: the control API (5.1) now sits between. The methods of the old surface interface survive as the driver (5.2), but `Sink` is implemented by the chat front end, the outbox moved to it, and the core identifies a thread by its own number; chat ids are only in its links. The supervisor's tools and the dashboard's server are two more clients, and so is the `hataori` command.

### 5.5 People

hataori manages people. The config lists them; each has a name and the ids by which chat services and automated posters know them:

```json
"people": [
  {"name": "ada",     "ids": ["discord:111", "slack:U222"]},
  {"name": "grace",   "ids": ["discord:333"]},
  {"name": "dispatch", "ids": ["discord:444"]}
],
"owner": "ada",
"scopes": [
  {"key": "<channel>", "client": "discord", "enabled": true, "people": ["grace", "dispatch"]}
]
```

- An **id** is `<client>:<id>`: the registered name of the client, a colon, and the service's own id of the author, as the driver reports it. The shape is the same as OpenClaw's `session.identityLinks`. A webhook or another automated poster is listed the same way, under a name of its own (`dispatch` above, with the webhook's id).
- A **name** is a word without a colon, so a name and an id are never confused. Each name and each id appears once; an id listed under two people is refused at start.
- `owner` names a person. The owner may act in every scope. A scope's `people` names the others who may act in it. Every name must be listed under `people`.
- An id that no person lists is a **guest**. A guest is shown and logged as its `<client>:<id>`. A guest can never start a thread, send input, answer or stop (section 5.1).
- **Ids are linked to people only in the config, by the operator.** Nothing said in chat links an id to a person or changes who a person is: no command, no message, no answer of the agent. A change takes effect when the daemon restarts with the new config.

**Moving from `owner` and `posters`.** Today the config's `owner` is a raw id, compared with the author's id by every chat front end and used as the control socket's principal, and each scope's `posters` are raw ids of that scope's client. A config without `people` is read in that form, unchanged, and converted when the daemon starts:

- the owner becomes a person named `owner` whose ids are `<driver>:<owner>` for every driver the config names;
- each poster `p` of a scope whose client is `c` becomes a person with the id `c:p`, allowed in that scope; one id that is a poster in several scopes is one person allowed in each.

So a running instance accepts exactly whom it accepts today, and the daemon logs one line saying the config uses the old form. A config with `people` names the owner by person, and a scope with `posters` in it is refused there, with a message to use `people`. Records made before the move keep the principal they were made with.

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
├── workspaces/<number>/   the thread's workspace; threads made before links keep their stored path
├── mirrors/      one bare mirror per remote
└── trash/        removed workspaces, kept for the retention period
```

An instance reads and writes only under its own root. A staging instance is another root with another bot identity. At start the daemon takes a lock in `state/`, checks that the identity it logged in as is the one in its config, and exits if either fails (c-lord#323, c-lord#325).

Unix socket paths are limited to about 100 bytes, so the socket path must stay short however deep the root is; if `state/sessions/<id>/` is too long the host uses a short path under the instance's runtime directory and records it.

### 6.2 SQLite

One database file. A schema version table and forward-only migrations (c-lord has no schema version). Main tables:

| Table | Holds |
|---|---|
| `threads` | number (`id`), scope, title, who started it, state, workspace path |
| `links` | thread number, client, channel, conversation thread, who made it, when; unique per conversation |
| `workspaces` | path, remote, state |
| `sessions` | agent name, agent session id, state, model, host pid, log offset, last sequence |
| `turns` | who started it, the message that triggered it, start, end, status, reason |
| `inbox` | accepted calls by their key; for chat, the catch-up cursor per conversation |
| `events` | the numbered event log that clients read from a position |
| `outbox` | the chat front end's pending and sent posts by `(event, link, part)`, with the surface message id |
| `chat_senders` | per key of input a chat front end gave, the message that gets the turn's lamp and who to ping at its end, as the service told it when the message came (#258) |
| `requests` | open questions and approvals, their message, their answer |
| `bindings` | channel to repository, and per-thread overrides |
| `settings` | model and other choices per scope |

**Moving to thread numbers.** One forward migration, in one transaction, recorded so it runs once. Part of it needs the config (which client owns each scope), so it runs when the daemon starts, after the schema migrations and before anything reads a thread.

- `threads`: `UNIQUE (scope, key)` is dropped and `key` is no longer an identity. Each thread's workspace path is stored in `threads`, copied from where its sessions recorded it.
- `links`: each existing thread gets one link, its current chat conversation: the scope's client from the config, the scope as the channel, the key as the thread. A thread whose scope is no longer in the config gets no link, is logged, and stays reachable by number.
- `events`: a `thread_id` column is added and filled from `(scope, thread)`; the scope stays for reads by scope. Events are not rewritten otherwise.
- `outbox`: each row gets the link of its conversation; rows keep their `(event, part)` and their surface message ids, so nothing is posted twice.
- Catch-up cursors stay per conversation; a cursor of a thread's conversation now belongs to its link.
- `inbox` keys and `chat_triggers` stay as they are; a trigger delivered again finds its thread through the link of the conversation it opened.
- **No workspace is moved.** A thread made before the migration keeps its directory; only new threads use `workspaces/<number>/`. The path is always read from the database, never computed from ids.

Who is allowed is in the config file, not in the database: the people, their ids, the owner and each scope's people (section 5.5). The database keeps who gave each input: `threads.started_by` and `turns.principal` hold the person, and `turns` gains `from_id`, the `<client>:<id>` the input came from. The events `thread_created`, `turn_started` and `turn_ended` carry both, as `principal` and `from`; `from` is absent for a caller with no `From`, such as the `hataori` command.

### 6.3 Workspaces and mirrors

A workspace is a full clone per thread. To avoid downloading a repository for every thread, hataori keeps one bare mirror per remote, fetches it before a clone, and clones from it. The rule: **a workspace stays valid if its mirror is deleted.** So the clone takes its objects by hard link or copy, not by a reference to the mirror's object store, and its `origin` points at the real remote. (c-lord references the mirror's objects and protects them by never pruning the mirror.) A mirror that cannot be fetched in time is used as it is, or skipped; it never blocks a thread.

A clone is made in a directory of its own and renamed into place, so a half-made workspace never appears. Right before the rename, the core records where the workspace goes and the device and inode of the clone, which the rename keeps. A daemon that died before it recorded the finished workspace takes the directory at that place only when its device and inode match. If nothing is there, it clones again; any other directory there is refused and left as it is. Nothing is written into the workspace to mark it.

### 6.4 A second host

A second host (for example a macOS machine) is another instance: its own root, database, agent logins and bot identity. Hosts share nothing and do not coordinate. A channel belongs to one instance, by config. Which bot answers tells you which host the work is on. Moving a thread between hosts is not supported; push the branch and start a thread on the other host.

For macOS specifically: the service manager is launchd, not systemd; the host process must not depend on `/proc`; Claude Code keeps its login in the Keychain there, which a background service may not be able to read **(unverified)**; the default filesystem is case-insensitive, which matters for workspace paths. None of this was run.

## 7. Supervisor, dashboard and plugins

All three are wanted and all are clients of the control API. The supervisor and plugins come after phase 1; the read-only dashboard is built alongside it (section 8). The supervisor and the dashboard add no concept to the core; plugins add hook points (section 7.3).

### 7.1 Supervisor agent

A supervisor is one ordinary session, Claude Code or Codex, that looks after the threads of one scope (one channel). It is a thread like any other, with its own directory and no repository. It is not an orchestration system: no roles, no task graph, no memory of its own beyond its conversation.

**What it can do.** hataori gives it the control API as MCP tools, through a small MCP server (`hataori mcp`) that connects to the daemon as a `Caller` limited to that scope:

| Tool | Control API call |
|---|---|
| list threads | `Threads` |
| read one thread: state, open question, latest output | `Thread`, output capped |
| send to a thread | `Send` with `Refuse` when busy |
| ask to interrupt a thread | `AskInterrupt`, approved by a person |
| start a thread | `Start` |

**How an interrupt is approved** (#272; decision 4 of #267). The supervisor asks with a reason and receives a request id immediately. The request is shown in the supervisor's chat thread with Approve and Refuse buttons; a person of the scope, other than the asker, answers it. Approval sends the marked reason as the supervisor, whose person must still be allowed in the scope, while interrupting only the session and turn that were running when asked. If that turn ended, nothing is sent or interrupted. One line in the chat thread says how it ended: interrupted, the turn had already ended, or refused. An approved interrupt ends the worker's turn, and that turn end wakes the supervisor as any other; a refusal or an approval after the turn ended is said in the supervisor's chat thread. The request survives a daemon restart.

**What it cannot do.**

- Interrupt a busy thread. `Send` is refused with "busy" and the supervisor gets the turn-end event later. Interrupting is a separate tool, and each use of it is put to a person as an approval in the supervisor's own thread.
- Merge, delete or reset. The control API has no such operations, so there is nothing to grant.
- Touch other workspaces directly. Its session runs without a shell and with file tools confined to its own directory. Checked against the real CLIs in their interactive or app-server mode (Appendix B, "Confining a supervisor session"):
  - *Claude Code 2.1.327:* `--restricted --strict-mcp-config --mcp-config <file> --tools "Read,Write,Edit,Glob,Grep"`. A shell command, a web fetch, a subagent and a wake-up were not available. A write and a read outside its directory were refused. A write inside it and a call to the listed MCP server were allowed. Under `--tools ""` only the MCP tools are left, so no built-in tool can write a file. The confinement held across `--resume` with the same flags and across a model switch. Project settings, `--settings`, the user's own settings, skills and plugins, and `--plugin-dir` added no tool back. Two limits: the flags must be given at every launch, since a `--resume` without them had a shell again; and `/model` typed in the session wrote the model into the user's settings file. hataori therefore changes a supervisor's model by relaunching with `--model`, never with `/model`.
  - *Codex 0.163.0:* neither sandbox removes the shell, the subagent tool or web search, and neither confines reads: `read-only` and `workspace-write` confine writes and the network only. The setting under which every attempt but the MCP call and the write inside was refused or not available is `sandbox: "workspace-write"` with `approvalPolicy: "never"` and `-c sandbox_workspace_write.exclude_slash_tmp=true -c sandbox_workspace_write.exclude_tmpdir_env_var=true --disable shell_tool --disable unified_exec --disable multi_agent --disable goals --disable sleep_tool --disable apps --disable plugins --disable hooks -c web_search="disabled" -c agents.max_depth=0`, with the MCP server's `default_tools_approval_mode = "approve"`. Without the two `exclude_*` settings a write to `/tmp` is allowed. Under this setting no built-in tool reads a file. In both CLIs, what the session can still do outside its directory is then only what the listed MCP servers' tools do, so those tools must be limited too: the check used one tool that only answers. Still open for the Codex phase: which of the `--disable` flags are each needed, a resume and a model switch, and whether a project's `.codex/config.toml` can add tools when a parent directory is not trusted. That file did load an MCP server in the check, and `codex app-server` recorded the directory as trusted in the user's configuration. The scripts now run the `/model` and project-configuration scenarios only with a configuration directory of their own (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`), since both write the user's configuration.

**Who its tools are** (#269; decisions 1 and 2 of #267). `hataori mcp --scope <scope>` connects to the control socket with a token in its hello (`$HATAORI_TOKEN`), which the daemon issues for each supervisor session and puts in that session's MCP config, in its state directory, outside the workspace. The daemon binds the connection to the Caller it issued the token for: that scope only, `OpRead | OpStart | OpSend`, every `Send` treated as `Refuse` (`Caller.RefuseBusy`), and every input marked (`Caller.Mark`) except an answer to an open question. Nothing `hataori mcp` sends widens it. An unknown or revoked token is refused and logged without the token; a connection with no token is still the owner. The caller's id is `supervisor:<scope>`, and the operator makes it a person by listing that id under a person of its own and that person in the scope's `people`: then the core's one check (section 5.1) allows its `Start` and `Send`, and its events name that person. Without the entry it can read its scope but gives no input, and removing the entry turns the supervisor off for that scope. Each input it gives, other than an answer to an open question, starts with the line `From the supervisor of this channel:`, added by the core from the caller, never by `hataori mcp`; a thread's title is made from the text without it. A send that answers a worker's open question is not marked, since the question tool takes the answer line by line or as a choice's label; the `answer` event names the supervisor's person. `read_thread` returns at most `--output-max` bytes of output (4096 when not given), with a line saying it was cut. A tool's refusal is a tool error that starts with the reason: `busy`, `not in this scope` or `not allowed`.

**Its thread and its session** (#270; decision 3 of #267). A scope names its supervisor in the config (`scopes[].supervisor`: the agent, the model, the effort and instructions). The daemon starts its thread once, as the owner, in the scope's own channel, opened by the scope's front end like any thread, under the key `hataori-supervisor`. After a restart the daemon takes the scope's newest confined thread as the supervisor's, so nothing starts twice; a failed one is kept, and one the owner closed is replaced by a new one, under the key `hataori-supervisor-after-<number>` (closing a failed supervisor is how a person asks for a new one). A Start key is kept per scope, so a daemon that died during that Start gets the same thread back. The thread is started confined: `core.Core.StartConfined`, which is no operation of the control API, records it on the thread, and its directory is `<root>/confined/<number>`, made empty by the workspace maker instead of a clone and outside `workspaces/`, which is all the remover removes. Its model is the adapter's cheapest (`agent.CheapModel`; Claude Code's `haiku`) when the config names none. Every launch of its sessions, new, resumed after sleep, recovered after a daemon died or begun again for a lost conversation, reads the thread's confinement in the runner, so none is ever unconfined: the spec gets `Confine` with the MCP servers the daemon gives that launch, and an adapter whose launch does not say `Confined` is refused before any process starts, which fails the thread with that reason. Claude Code's launch adds `--restricted --strict-mcp-config --mcp-config <state>/claude-mcp.json --tools "Read,Write,Edit,Glob,Grep"`, the flags checked above; that check ran them with `--setting-sources project,local`, which the launch does not pass, and it also found that the user's own settings, skills and plugins add no tool (under `--restricted` the user's settings file is not loaded at all, see below). The check tried only Read and Write outside the directory. The session may write its own directory, so the launch also keeps out what it could plant there for its next start: it adds `--setting-sources user`, so that the project's and the local settings files, which lie in that directory, stay out by that flag as well as by `--restricted` (which, as its help says and as Claude Code 2.1.328 showed, ignores the user's settings file too, whatever `--setting-sources` names: under it the init event lists none of the user's plugins and not the user's model, so a supervisor gets no `env`, hooks or plugins from the user's settings, only hataori's `--settings`, managed settings and the variables the adapter passes); it shows hataori's own status line alone, since hataori would otherwise copy a project's status line command into its `--settings`, and even the user's own runs in that directory, often running git; and it sets `CLAUDE_CODE_DISABLE_CLAUDE_MDS=1`, `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`, `ENABLE_CLAUDEAI_MCP_SERVERS=false` and `GIT_DIR=/dev/null`, so that a CLAUDE.md there with imports of files outside is not loaded, no memory is kept outside the directory, no claude.ai connector joins the listed MCP server, and a `.git` there, whose config could name a command (such as `core.fsmonitor`) that the CLI's own look at the directory would make git run, is no repository. Nor may it write such files: its `PermissionRequest` hook, which `--restricted` leaves the writes to settings, git and tool configuration files to, refuses a write to any path in its directory with a part that starts with a dot (`.claude/`, `.mcp.json`, `.git/`, `.vscode/`, ...) and to any path outside it (#423). That the CLI honours the hook's refusal is documented, not yet checked against the real CLI **(unverified)**: the check needs a logged-in turn. These additions were not part of #268's check; the real supervisor check (`docs/testing.md`) plants each of them. Input that reaches the session can itself be a command: the CLI reads one that starts with `!` as a command for its shell mode, and runs it under `--restricted` too, and one that starts with `/` as one of its own commands (`/add-dir` widens the directories the file tools may use). So in a confined session, an input whose first character, past spaces and format characters, is `!` or `/` is pasted after `(message) `, and the CLI takes it as a prompt (#426; Claude Code 2.1.328, also after the CLI put an interrupted prompt back in the box). The MCP config names one server, `hataori mcp --scope <scope> --root <root>`, with the session's token in `HATAORI_TOKEN`. A session keeps one token for its life (decision 1): the daemon issues it at the session's first launch and keeps it in the session's state directory, gives it to each launch, and binds it again at a restart, before the socket serves, so an agent that kept running keeps its tools; the token of an older session of the thread is revoked when a newer one starts. The stored spec keeps the confinement without the MCP servers, so no token is in the store. An ordinary thread's launch is unchanged, byte for byte. Codex cannot start a confined session yet, so a Codex supervisor fails until the Codex adapter confines.

One limit is not enforced by this: the supervisor can *ask* a worker thread to merge or delete. What a worker may do is decided by that worker's own rules. Messages from a supervisor are marked as such, so a worker's instructions can treat them differently from a person's; an answer to a worker's question is not marked, and its `answer` event names the supervisor.

**When it runs.** It never polls. hataori wakes it by sending it one message when something happens in its scope: a turn ended, a question was asked, a turn failed, a usage limit was hit. Its own thread is excluded. Events that arrive while it is busy, or within a short quiet period after the first, are folded into one message.

**How it is woken** (#271; decision 5 of #267). Each scope that names a supervisor has one waker in the daemon (`supervisor.Waker`). It reads the scope's events from its position, kept in the store (`supervisor_wakes`); a scope seen for the first time starts after the events already stored when the daemon starts, before it reconnects or recovers any session (`supervisor.Begin`), so that a turn end it writes then, as of a worker whose turn ended while it was down, wakes the supervisor (#445). What wakes is a `turn_ended` of a thread of the scope that is no supervisor's thread, neither the current one nor one a person closed and the daemon replaced: completed, blocked on a question, or failed, a usage limit included (`reason` `limit`). Other kinds, and other scopes, wake nothing. The first such event opens the quiet period (`scopes[].supervisor.quiet`, 30 s); the turn ends until it is over, and those while the supervisor's turn runs, are folded, each thread once with its latest turn end. The waker then gives the supervisor's thread one message, the digest, with `Send` and `Refuse`, as the owner. The digest names each thread, by number: its title, how its turn ended with the reason, its background count and its last output, cut to `output_max` bytes (1 KB) at a character and quoted line by line; at most `threads_max` threads (20), then one line counting the others. Its format is fixed by a golden file. A wake is sent only to a supervisor thread that is idle or asleep, looked up before the digest is made and again after: one that is running, starting, waiting on a question, failed, closed or not there yet is not sent to, and the events wait for its next turn end or change of state, which makes the digest again with what came since. So the turn ends during one supervisor turn start exactly one more. A running one is not sent to, though `Send` would refuse it, because `Send` refuses only a running thread: a turn that asks a question between the look-up and the send would take the digest as its answer. For that moment, the waker's caller has `NoAnswer`: its `Send` to a thread waiting on a question is refused as busy, in the transaction that would take the input, and the wake waits as above (#441). If the thread starts a turn in that moment anyway, `Send` refuses the wake as busy, it is not accepted, and it waits the same way. The wake is stored, with its text and its key (`hataori-wake-<position>-<thread>`: the core takes a send's key once per scope, so the key names the supervisor's thread too, and a stored wake that goes to a thread replacing the one it was made for is sent under that thread's own key, since a thread whose resume failed took the key without reading the digest, #443; a key from before, with the position only, is sent again under its own form only to the thread the core says took it, `core.Core.SentTo`), before it is sent, and the position once it was accepted: a daemon that died in between sends the stored text under the same key again, which the core ignores if it was accepted, and events up to the stored position are not folded again; so each event wakes the supervisor once across restarts, not twice and not never. The one exception: a supervisor thread that took a wake and was closed before the waker stored its position may have read it or not (its resume may have failed), and its replacement is given the wake again, under its own key. Before a wake is made, one `supervisor.Gate` is given its threads, each with its `turn_ended` event, and returns those that may wake it; nothing is sent when it returns none. It is where the budget (#273) and the loop guard (#274) plug in. A wake refused as busy after the gate admitted it is made again later, and the gate is asked again, so wakes are counted from the supervisor's `turn_started` events with the wake's key, not from the gate's calls. A wake's input names the owner as its principal; its `turn_started` has the wake's key, which tells it from a person's input.

**Where this runs.** Waking, the budget and the loop guard below are one client of the control API inside the daemon, as the chat front end is: it reads events from a position, keeps its position, and wakes the supervisor with `Send` to the supervisor's thread. The core has no supervisor code; to the core the supervisor's thread is an ordinary thread and its tools are an ordinary limited `Caller`.

**How its cost is bounded.**

- *No events, no turns.* An idle scope costs nothing.
- *Small input per wake.* The wake message is a digest: thread, how the turn ended, the last lines. Reading more is an explicit tool call with a capped answer.
- *A budget per scope:* supervisor turns per hour, sends and starts per hour, and tokens per day, counted from the usage the agent records for each turn. Over budget, the supervisor is not woken; a person is told once.

  How (#273; decision 5 of #267): `scopes[].supervisor.turns_per_hour` (12), `sends_per_hour` (20) and `tokens_per_day` (2,000,000), each turned off by 0. `supervisor.Budget` counts from the event log, so a restart keeps the counts. Turns are the `turn_started` events of the scope's supervisor threads in the current hour, whoever started the turn. Sends and starts are the `input` and `answer` events whose `from` is the supervisor's id in the current hour, a start counting once through its first input; and a `thread_created` of its own whose thread has no input of its yet, as a start that failed before its first input. Tokens follow `Tokens.Today`'s rule over the `usage` events of its threads: each report adds what it raises its turn's highest total by, counted in the day it came. Hours and days are those of the daemon's clock and time zone; the core stamps `thread_created` with that clock too, as every other event of a thread. The budget is used up when any limit is reached. It is the waker's gate: a wake is then held, not dropped, until the windows of every limit reached have ended, and the turn ends until then are folded into the one wake that follows. Once the sends limit is reached, the daemon also refuses the supervisor's `Start` and `Send` at the call, on its token's connection, with `ErrForbidden` and a message that says why and until when, so that one long turn cannot send without bound. In both cases a person is told in the supervisor's chat thread: one line saying what is used up and until when, once for each end of a hold and the limits reached by then, so a limit reached later in the same hold is told too. The line is the chat front end's notice, keyed by `hataori-budget-<end>-<limits>`, which is posted once also across restarts. Hours are taken from the instant, so the hour repeated when the clocks go back is one of its own, and a hold waits at most a minute on one timer and goes by the wall clock, which goes on while the host sleeps. The loop guard (#274) goes before the budget in the waker's gates.
- *A loop guard.* A supervisor send that ends a worker turn that wakes the supervisor is a cycle. After a set number of cycles on one thread with no person's message in between, that thread stops waking the supervisor until a person acts.

  How (#274; decision 5 of #267): `scopes[].supervisor.loop_cycles` (3; 0 turns it off). `supervisor.LoopGuard` is the waker's first gate, in front of the budget, so the threads it drops spend nothing. It counts from the event log, so a restart keeps the counts. A cycle on worker thread W is a wake (a turn of a supervisor thread whose key starts with `hataori-wake-`) that covers the end of a turn of W that the supervisor started (`from: supervisor:<scope>` in `turn_ended`), a turn that started after W's last reset: a person's message delivered into such a turn is a reset, and takes that turn out of the count (#454). Several such ends in one wake are one cycle. The count starts after the last reset. That is an input or answer in W from anyone but the supervisor, or a turn typed at W's terminal; or a message or answer of a person in a supervisor thread of the scope, or a turn typed at its terminal. Any input there counts but the first input of a turn whose key starts with `hataori-`, which are the wakes and the thread's first message. At the count, W's turn ends are dropped from wakes until a reset, whoever started their turns, and other threads keep waking the supervisor. A person is told in the supervisor's chat thread with one line naming W: once the wake that makes the last cycle was accepted (`supervisor.Woken`, a gate told of each accepted wake), or else at the first drop. A stored wake that was sent again after a restart is an example of the second case. It is keyed by W and the reset (`hataori-loop-<W>-<reset>`), so it is told once for each stop, also across restarts.
- *The ordinary limits apply:* the cap on turns running at once, and sleep when idle.
- *A cheaper model by default.* Its job is reading digests and routing.

### 7.2 Dashboard

A web page that shows every thread a person may see: its state, its numbers, and its terminal, live. It changes nothing in a thread. The one thing it may change is a tmux pane on the host that hataori does not own, which the owner can type into when the config allows it (below). The maintainer chose the layout and started the work on 2026-11-18, in parallel with the remaining phases, and chose how the dashboard is reached and who may type into host panes on 2026-11-19 (section 10).

The unit is the thread, not the terminal, so the page does not depend on how hataori talks to the agent. A Claude Code session runs in a terminal and shows it live; a Codex session has none and shows its output rebuilt from events.

**Layout.** One page in five parts.

- **Tree, on the left.** A filter box on top, matching title, number and terminal. Under it a *needs attention* block: the threads waiting on a question or failed, each with the question or the failure reason. Then the tree itself, which reads like the chat it mirrors (#405), driven from the keyboard as a tree view (up, down, left folds or goes up, right unfolds or goes in, Enter opens):
  1. *One node per scope* the person may see (`Scopes`), also one with no thread, named as the chat service names its channel (`#name` on Discord, through the control API), by the config's name when it has one, else by its key. It shows the number of its threads and one state dot per thread, and folds; the folds are kept in the browser. Under it its threads, the most recently active first. A thread's row shows the agent's mark coloured by state, its number and title, and its age. Its title is the chat thread's own name, following a rename, else the first line of its first prompt; an issue's or a pull request's address in it is shown short, as `repo#N`, with the whole title in the tooltip and the details panel (#406); a second line shows the model, the context use in percent and the last line of output, with a question or a failure highlighted.
  2. *Host terminals*, one node after the scopes, folded at first. In it, *tmux on the host*, sessions that hataori does not own: session, then window. A row shows the window, its process, its terminal device and its last activity. A pane whose foreground process is a shell and whose window has shown no activity for more than a day is hidden by default, by the same switch that hides asleep threads (#335), and counted with them.
     Then *other terminals on the host* that hataori does not own: terminal device and process.

  A footer counts the threads per state.
- **Tab bar, above the main area.** An "All threads" tab is pinned first; the items opened from the tree follow. A tab shows the agent's mark coloured by state, the title, the time since the turn started while one runs, a badge for an open question or a failure, and a close button. The tabs are flat; the active one has the terminal's background. At the right end: show or hide the tree, split, show or hide the details, light or dark. Split goes from one pane to two to three, side by side; the extra panes show the tabs viewed before.
- **Main panes.** The selected session's terminal, live and read-only. A Codex session: its output rebuilt from events. An asleep session: its last screen, marked "no process". A tmux pane on the host: its screen, read-only until the owner switches the tab to "operate" (below).
- **Details panel, on the right, shown on demand.** On top, the open question or the failure reason. Then:
  - *thread*: state, scope, who started it, when, number of turns;
  - *agent*: kind, model, effort, context use, tokens this turn and today, background tasks running;
  - *workspace*: repository, branch, lines added and removed, uncommitted files, unpushed commits, pull request, terminal device;
  - *recent events*, with their times.

  For a terminal hataori does not own: tmux session, window and pane, terminal device, process, directory, last activity, and a line saying that hataori does not judge its state.
- **Status bar, at the bottom.** On the left, the selected item: number, state, model, context use, terminal device. On the right: the remaining usage of each agent with its reset time, a live mark with the position of the last event read, and "read-only", or the pane being operated.

The **"All threads" tab** is a table with one row per thread, grouped by scope: state, number, title, agent and model, state with its age, context use as a bar, tokens today, branch, changes (lines added and removed, uncommitted files), unpushed commits, pull request, last output. Clicking a row opens the thread's tab.

Rows are dense by default; the normal density hides the second line of a tree row. On a narrow screen the same page becomes a list: needs attention first, then running, then the rest folded. There is no separate app.

Not in the dashboard: diffs, boards of cards, a browser inside the page. Other tools do these well.

**Read-only for threads.** No call changes a thread, and no view of a thread's terminal ever takes input: input to an agent goes through chat or the control API, never as keys into its terminal. A question's choices are shown, not offered as buttons. Sending, answering and stopping come later, through the same API and the same checks. Besides a host tmux pane that the owner operates, the dashboard changes only the person's own passkeys (below).

**Where the data comes from.** Everything goes through the control API, versioned as [docs/protocol.md](../protocol.md) says. The dashboard reads no database, no state directory and no workspace by itself.

| Shown | Source |
|---|---|
| threads, state, number, title, scope, who started it, when | `Threads` (exists) |
| open question, failure reason, last output | `Thread` (exists), the `request` and `turn_ended` events |
| live updates, the live mark | `Events` from a position (exists) |
| recent events of one thread | new optional argument of `Thread` |
| a thread's conversation, read back to its first input | new operation `ThreadEvents`: events before a position, with a limit (#336) |
| agent, model, effort, number of turns, last line, background tasks | new optional fields of `ThreadInfo` |
| tokens this turn and today, context use | new optional fields of the `usage` event, which the core sums into `ThreadInfo` |
| branch, lines added and removed against the branch point, uncommitted files, unpushed commits | new operation `Workspace` |
| the live terminal of a session | new operation `Terminal`, a read-only stream from the session host |
| terminals hataori does not own | new operations `HostTerminals` and `HostScreen` |
| typing into a tmux pane on the host | new operation `OperateHost` |
| a sign-in link for one of the listeners | new optional argument of `SignInLink` |
| pull request | no known source: research first |
| remaining usage per agent, reset time | new operation `Limits`: Claude Code's status line input, Codex's `account/rateLimits/read` (research #191) |

Appendix D lists the new operations and fields as a proposal. Each is a minor protocol change.

- **Model and effort** are what the agent reports when it reports them, and what was asked for until then. **Tokens today** are counted per calendar day in the instance's time zone.
- **Workspace state** is the same check that section 4.6 needs, built once and used by both. The dashboard's use only reads, and it answers from the remote refs as last fetched, with the time of that fetch, because it is asked often. Removal still asks the remote at that moment.
- **The live terminal.** The session host already keeps the terminal's raw output in a bounded log (section 4.3). It also counts every byte it writes, so a byte keeps its position when the log is trimmed. `Terminal` first sends the end of the log with the terminal's size and the position after it, then output as it comes. A viewer that reconnects asks from its last position; when that is no longer kept, it gets the end of the log again and redraws. The host still interprets nothing. The page draws the bytes with a terminal emulator and drops everything the emulator would send back, keys and answers to terminal queries alike: no byte goes from the page to a thread's terminal.
- **Pull request and remaining usage** each wait for a research issue. For remaining usage: what Claude Code and Codex (`codex app-server`) report about remaining usage and reset times, run against the real CLIs. For pull requests: whether a thread's pull request can be found without hataori calling a code host. A field with no source is left out, not guessed.

**Terminals hataori does not own.** This widens the dashboard beyond hataori's own threads. The rules:

- Display only, except a tmux pane that the owner operates (below). hataori never infers a state from them; the details panel says so. A terminal outside tmux never gets a key.
- Owner only, and off unless the instance's config turns it on.
- A pane counts as an idle shell when the command tmux reports for it is a shell (`sh`, `bash`, `zsh`, `fish`, `dash`, `ksh`, `mksh`, `tcsh`, `csh`, `nu`, a leading `-` ignored) and its window's last activity is more than a day old. tmux reports activity per window, not per pane, so a shell in a busy window is not idle. Such a pane is hidden by default (layout, above); a pane with an open tab keeps its tab.
- tmux panes are listed with tmux's own commands, which report session, window, pane, terminal device, current command, directory and last activity. A pane's screen is read with `capture-pane` while a tab shows it.
- Other terminals are found from the process table through `ps`, not `/proc`, so the same code runs on Linux and macOS (section 6.4). A program that does not hold a terminal cannot read its screen, so only metadata is shown for these.
- A terminal that a session host owns is shown once, as its thread.
- With the Claude Code plugin installed (section 4.8), a pane whose session has reported also shows the session id, the directory, the last hook event and its time. That is reported by the session's hooks, not inferred. A pane that has been taken in is shown once, as its thread.

**Three ways in, and who is at the other end.** A terminal app on one's own machine knows its user: whoever has the window. The dashboard is served by a daemon that may run on another machine, so before it shows anything, and above all before it lets anyone type, it must know who is at the other end. It is opened in one of three ways, and each has its own proof. The daemon tells the ways apart by the listener a connection arrived on, never by anything the request says.

1. **Same machine: a native window** (Electron, later; not designed here). It connects to a Unix socket in the instance's state directory that only the OS user running the daemon can open, like the control socket. That user is the owner (section 5.1), so a connection there is the owner's, with no sign-in.
2. **Another machine, over SSH.** This is the default for remote use: most people who run hataori on a Linux server can reach it by SSH, and far fewer can give it HTTPS and a domain name. The daemon listens on loopback only. The person's machine forwards a local port to it over SSH and opens `http://localhost:<port>`, which browsers treat as a secure origin, so no certificate and no name are needed. The sign-in link is fetched over the same SSH connection, by running `hataori signin` on the host. Logging in by SSH as the instance's OS user is the proof: that person can already do anything that user can, the control socket included. Another user of either machine can reach the port, but not the link. The same listener serves a browser on the host itself, signed in with `hataori signin` there.
3. **The web, over HTTPS with a stable name**, set up by the operator, for a phone or a machine without SSH. The first sign-in is the single-use link, sent in chat or printed on the host. After that the person can register a passkey and sign in with it (below). An address that is neither loopback nor served over HTTPS gets no dashboard: the daemon does not start with one.

The rules for every way:

- **Listeners.** The loopback listener (way 2 and a browser on the host) and the web listener (way 3) are configured apart: `dashboard.listen`, which must be loopback, and `dashboard.web`, with its address, the name people open, and TLS of its own or a reverse proxy in front. Either may be left out. A config in today's form, one listener, keeps meaning what it means: with TLS or a proxy it is the web listener, otherwise the loopback listener, and the daemon logs one line saying the config uses the old form. The native window's socket comes with the native window.
- No route answers without a session, including the event stream and the terminal streams, except the sign-in routes (opening a link, and the passkey sign-in ceremony) and the page's own files, which hold no data: the page reads its session from its own storage once it runs. On the native window's socket the connection is the session.
- A sign-in link is short-lived and single-use, and the session it opens belongs to the person who got it (through `SignInLink`, Appendix D): today the owner only, from `hataori signin` on the host or, with the "people in chat" phase, from chat (section 10). A link and the session it opens belong to one listener. `hataori signin` makes a link for the loopback listener, `hataori signin --web` one for the web listener, and a link asked for in chat is for the web listener when there is one. A link or a session presented on the other listener is refused.
- A session lasts seven days on the loopback listener and twelve hours on the web listener (the maintainer, 2026-11-19). On loopback the person reached the host by SSH as the instance's user, or is on the host, which is already the proof, and signing in again every twelve hours is the chore `hataori connect` is there to avoid.
- A person sees a thread only if their `Caller` includes its scope. The channel decides who can read a thread in chat, and the dashboard must not widen that.
- The server checks `Host` and `Origin` on every request, so a web page elsewhere cannot reach it through the person's browser. On the web listener the `Host` must be its configured name. On the loopback listener it may be `localhost`, `127.0.0.1` or `[::1]` with any port, since a forwarded port need not have the same number as the daemon's. An `Origin`, when sent, must be exactly the listener's scheme, the request's host and its port: on the web listener the configured address, on the loopback listener `http://` and the request's `Host`. A page under another name that resolves to loopback still sends its own name, and is refused.
- **No session cookie.** A browser sends a cookie to every port of the host that set it, so a service that someone else runs on another port of the same machine, or of the web listener's name, would receive the session whenever the person's browser was led there. A session is therefore a token that the page keeps in its own origin's storage, which browsers keep apart by scheme, host and port, and sends in a header with each request. The page reads its event and terminal streams with `fetch` instead of `EventSource`, which cannot send a header, so the streams carry it too; the operate WebSocket gets it in its first message, before which the server accepts nothing on it. The token never goes into an address, since a reverse proxy's access log keeps addresses. A link's token is in its address's fragment, which no server and no preview crawler sees: the page takes it out of the address and trades it for the session with a request of its own, so a chat app that fetches the link for a preview uses nothing up, and the session is in no address. Two instances forwarded to two ports of one machine keep their sessions apart for the same reason. Since a script in the page could read the token, the page keeps its strict content security policy: no script runs but its own.
- The loopback listener refuses a request that came through a reverse proxy (`Forwarded`, `X-Forwarded-*`, `X-Real-IP`, `Via`): SSH adds none, and a proxy that rewrote the `Host` to a loopback name would otherwise open it to whoever the proxy serves.
- Only `GET` and `HEAD` are served, except on a fixed list of routes: those of passkeys and the operate connection (below). A request to one of them, the WebSocket handshake included, must carry an `Origin` that matches as above; a missing `Origin`, or `null`, is refused.

**Over SSH without a chore.** Opening SSH by hand every time is not acceptable. `hataori connect [user@]host`, on the person's machine (Linux or macOS; the same binary), does it all:

- It runs the system's `ssh`, so the person's keys, agent and config apply. hataori stores no SSH key, and sets nothing beyond the forward and the connection sharing below.
- It opens one connection, shared through OpenSSH's control socket in a directory only the person can open. Through it it runs `hataori signin` on the host and forwards a socket in that directory to the loopback address the link names. It listens on a local port on `127.0.0.1` for its whole run and passes each connection on to that socket, and opens the browser at the link on `http://127.0.0.1:<local port>`. Holding the port means nobody else can take it while the connection is down, and `127.0.0.1` rather than `localhost`, which may resolve to `::1`, where it does not listen. The person authenticates once. The link is written to no file and no log. It does reach the browser through the system's launcher (`open`, `xdg-open`), whose command line other users of the same machine can read while it runs; the link is short-lived and works once, and a link someone else used first shows the person "already used" instead of signing in, so a theft is seen. On a machine shared with people one does not trust, the native window is the way.
- It keeps running and keeps the connection alive. When the connection drops it reconnects with a backoff, asks the host for the dashboard's address again, and an open page resumes on the same local port as after any reconnect: no gap, no repeat.
- It remembers `user@host`, the instance root on the host (`--root`) and the local port, so next time `hataori connect` alone is enough. Run again while one runs for the same host, it has the running one fetch a fresh link and open it: that is how one signs in again after the session ended.
- `--hataori <path>` names the binary on the host for a non-interactive SSH session whose `PATH` does not have it. A missing `ssh`, forwarding refused by the server, and a link that does not name a loopback address each stop it with a message that says which.
- By hand it is `ssh -L 127.0.0.1:<port>:127.0.0.1:<dashboard port> user@host` (the local address given, so that the person's `GatewayPorts` cannot open the port to the network), `hataori signin` there, and the link with the port changed. The native window later does the same from a saved `user@host`, as editors' remote-SSH modes do.

**Passkeys on the web.** WebAuthn, with `github.com/go-webauthn/webauthn`.

- The relying party is the web listener's name. A passkey is registered only with a proof made for that registration: right after opening a fresh sign-in link, in a ceremony bound to that use of the link and good for five minutes, or after a check of a passkey the person already has. A session alone, however new, cannot add one, so whoever takes a session cannot keep a way back in. User verification is required, and the passkey is discoverable, so signing in asks for no name.
- Signing in with a passkey opens a session like a link does, and the session records when the passkey was checked.
- The dashboard server keeps each person's passkeys in a file of its own in the state directory, next to its session key: credential id, public key, signature counter, when made and when last used. They decide who may open the dashboard and nothing in the core, so the control API does not change for them. A person's passkeys stop working when that person may no longer sign in. The dashboard lists the person's passkeys and removes one, from either listener; removing the file removes them all at the next start.
- Passkeys are for the web listener only. On the loopback listener and the native window, SSH or the OS user is already the proof.

**A VPN or tunnel** fits the web way without hataori depending on it. A product that gives the daemon an HTTPS address with a stable name inside a private network, such as a mesh VPN's built-in HTTPS proxy, makes way 3 work with nothing else to set up, passkeys included: its name is the web listener's name, and its address the reverse proxy. The configuration reference shows that as one example. Nothing in hataori requires or detects such a product.

**Typing into a tmux pane on the host.** A tab that shows a tmux pane hataori does not own can be switched to "operate"; the person's keys then go to that pane.

- Owner only, and off unless the config turns it on (`host_input`, which needs `host_terminals`).
- Only tmux panes on the host. A thread's terminal is never operated. A pane in which one of hataori's own processes runs, such as `hataori attach`, shows an agent and is refused. That is checked when the operation starts and before each batch of keys, by the command tmux reports for the pane, and at least once a second against the process table; a batch is not sent when the check fails or cannot be made. A program started in the pane between a check and the keys can still get them; the one typing is the owner, who could type there anyway.
- On the native window and on the loopback listener, the switch starts it at once. On the web listener it first asks for a passkey check; a person with no passkey registered needs a session signed in less than five minutes ago, and is otherwise asked to sign in again.
- One pane is operated from one tab at a time; a second is refused, with since when and by whom.
- It ends after five minutes without a key (configurable), when the tab is closed or the page left, when the person switches it off, when the pane goes away or becomes hataori's own, and when the session ends. The tab goes back to read-only and says why.
- Each start is one log line with the person, the time and the pane (id, session, window and index), and each end one with the same and the reason. hataori writes what was typed to no log, database or file, nor how much. Text passes through a tmux buffer of the operation's own for as long as one paste takes; the paste deletes it, and so does every failure, the end of the operation and the daemon's start, for any such buffer left over. What the program in the pane keeps, such as a shell's history, is that program's.
- While a pane is operated its screen is read at least five times a second instead of once, with the cursor position, so what is typed shows within a fifth of a second.

**How keys reach the pane: `tmux send-keys`, not an attached client.** Every key other than text goes by `send-keys` with tmux's own name for it (`Enter`, `Up`, `C-c`, `F5`), from a fixed table into which the page turns what its terminal emulator produces. Text and pastes go by `load-buffer -` into a buffer of the operation's own, then `paste-buffer -d`, with `-p` for a paste, so typed text is never on a command line where other users of the host could read it in the process table. Key names are on the command line, and a name with a character, such as `C-a` or `M-x`, shows that character; text is never sent that way. Why:

- tmux knows the pane's modes and encodes a named key for them. `Up` reached a program as `ESC [ A`, and as `ESC O A` once the program had turned on application cursor keys; `paste-buffer -p` wrapped a paste in bracketed-paste marks when the program had asked for them (Appendix B). The page cannot know those modes: it draws a screen captured with `capture-pane`, not the pane's byte stream, so bytes made by the page would be wrong in exactly the programs that change modes, such as editors and pagers.
- No tmux client is attached. After the keys the window had its size and the session was still not attached (Appendix B); the keys themselves change what the pane's program does, for everyone who watches it. An attached client, `tmux attach` in a terminal that hataori opens, would show a whole window instead of the pane, mark the session attached and run the client hooks, change the session's current window for every other client when it changed window, and under tmux's default `window-size latest` resize the window on the person's own terminal to the dashboard's size whenever the dashboard typed last. `attach -f ignore-size` avoids the size, and none of the rest.
- The cost: what is typed shows at the next screen read, up to that interval later; the mouse is not passed on; and the screen is drawn at the pane's own size, scaled to fill the dashboard's pane (#333), never resized.

The operate connection is a WebSocket of its own, opened when the tab switches to "operate" and closed when it ends. Closing the tab closes it, which ends the operation at once; and on plain HTTP a browser's few connections per host are already held by the event and terminal streams, which leaves no room for a request per key. The screen streams stay Server-Sent Events and keep refusing input.

**How it is built.**

- The server is part of the `hataori` binary and is a client of the control API with a `Caller` that reads, and that may also operate host tmux panes for the owner when host input is on.
- The page is TypeScript, React and xterm.js, built with Vite. The built files are committed and embedded with `go:embed`, so `go build` needs no Node. CI builds them again and fails when the result differs from what is committed.
- The UI is in English and Japanese. Every string is in a catalog; choose a language with `?lang=ja` or `?lang=en`, and the choice is kept in this browser; without a choice, the page follows the browser's language.
- A native window later (Electron) wraps the same pages; it is way 1 above, and not designed here.

**Tests.** As the repository's rules say, and one end-to-end test with a fake agent: the dashboard lists a thread, shows a turn's start and end from the event stream, and after a reconnect shows no gap and no repeat.

### 7.3 Plugins

A plugin lets another developer add to what hataori does, in any language, without changing the core. The aim is that more people use hataori and that it grows by reusing what exists.

Its shape follows OpenClaw's plugins: a manifest, registration, and hooks. Hook names and timing follow OpenClaw's where the meaning is the same, so someone who wrote an OpenClaw plugin can write one for hataori, and an existing one is easy to port. One thing differs. An OpenClaw plugin is loaded into the Gateway's process and is trusted like core code; its documentation says it is not sandboxed. A hataori plugin is a separate program, for two reasons:

- Go cannot reliably load code into a running binary. The standard `plugin` package needs the same toolchain and the same version of every shared package as the binary.
- Only a separate program makes what a plugin declares enforceable. The core checks each of its calls, as for every client.

A plugin's grant binds a plugin that presents its token. It is not confinement against a hostile plugin: a process of the daemon's OS user, a plugin included, can change the config or read the state directory, and a connection without a token from that user is the owner's. Running a plugin as another OS user is how to confine it (below, "Later"). One guard catches the mistake of a plugin that forgot its token: a connection without a token from a process in the process group of a plugin hataori started is refused, and the refusal names the plugin. A `hataori` command that a plugin starts is refused the same way. A process can leave its group, so this is a guard against mistakes, not a boundary (decided in #487).

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

**What the operator grants.** The plugin's config entry names the scopes it may see and act in, and may narrow the operations, events and hooks below what the manifest asks for. The plugin's `Caller` is what the manifest asks for and the config grants, and nothing more. Any other call is refused by the core's one authorization function (section 4.4), and the refusal is logged. This binds a plugin that connects with its token, as above.

Message text (input to the agent, the agent's text, outgoing posts) reaches a plugin only with an explicit `conversation` grant, as with OpenClaw's `allowConversationAccess`. Without it, the plugin gets events and hook calls with the text left out. Thread titles are left out too: a title is the first line of the first prompt, so a person wrote it. Such a plugin knows a thread by its id and number. It is refused `Terminal` with the ordinary refusal, logged like other refusals; there is no text-free terminal stream in this phase (decided 2026-11-19). It is refused `HostScreen` of every pane the same way, since a pane can keep a session's screen after `hataori attach` ends, and hataori does not try to tell which panes once showed one; `HostTerminals` stays as it is (decided 2026-11-20). No client can read the login relay's link and code (section 4.7), so no plugin can either.

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
| `thread_created` | Observe | core event | `session_start` |
| `turn_started` | Observe | core event | none |
| `tool_started`, `tool_ended` | Observe | core event | `after_tool_call` (closest) |
| `request` | Observe | core event: a question or an approval | none |
| `turn_ended` | Observe | core event | `agent_end` |
| `post_sent` | Observe | chat front end event, new in this phase: an outbox row was posted | `message_sent` |
| plugin started, stopping | Observe | told to the plugin itself | `gateway_start`, `gateway_stop` |

The observe rows are event kinds of [docs/protocol.md](../protocol.md), and the manifest's `events` names kinds from there; any other kind there can be asked for too. A cancelled post stays in the outbox with the plugin and the reason, and is visible through `hataori thread`. Nothing is posted in its place unless the plugin returned new text.

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
- [ ] No code path in the binary removes or resets a working directory, except the removal of section 4.6 (`internal/workspace/remove.go`), and that only through `Remove` (#255).
- [ ] Two instances with different roots on one host each ignore the other's channels.
- [ ] No core package imports a driver, an agent adapter or a client, or contains the name of a surface or an agent (checked in CI).
- [ ] For one week, every order the deployment's manager sends runs on hataori and none on c-lord.

### Alongside phase 1: the read-only dashboard

Section 7.2. It starts now and runs in parallel with the rest of phase 1 and with the phases after it (decided 2026-11-18, section 10). It does not hold up phase 1: phase 1's acceptance does not wait for it, and its issues are tracked apart.

It has the criterion of the later phases below, with three additions that the dashboard needs in core packages: the new operations and fields of Appendix D, the workspace check of section 4.6 (read-only use), and the session host's byte count for the terminal stream. Its acceptance criteria are in Appendix E.

How the dashboard is reached from another machine, passkeys on the web, typing into host tmux panes and hiding idle shell panes (decided 2026-11-19, section 10) are built the same way, tracked in #366. The native window is not part of that work.

### After phase 1

The order below was decided by the maintainer (section 10): Codex, people in chat and cleanup, the supervisor, plugins, then Slack and Teams. Each later phase also has this criterion: the change touches no core package, except to add an operation to the control API or to register an adapter; the supervisor phase may also let the control socket bind a connection to a limited `Caller` (which plugins use too, section 7.3) and let a session be started confined (no shell, only the listed tools, section 7.1); the plugin phase may also limit a `Caller` to the operations and event kinds a plugin was granted, and add hook points (section 7.3). Appendix E has the acceptance criteria of each.

**Codex.** The second agent adapter (section 3.4). It comes before the buttons of "people in chat", so it is built and tested with what phase 1 offers in chat. The adapter's code is written by Codex (`codex exec`), and reviewed and fixed by Claude. Choosing the agent per scope (`agent`) or order (`agent:`), with `model` and `effort` per adapter section, counts as registering an adapter under the phase criterion (#318).

**People in chat, and cleanup.** Slash commands, buttons for questions, lamps, pings, attach by hand (`hataori attach`, the agent's terminal on the host), and removing workspaces behind the proof in section 4.6. This is what lets c-lord be switched off. The dashboard's sign-in link from chat comes with it. A second chat link (`/attach <number>`, section 5.2) is built with thread numbers (#215), not in this phase.

**Supervisor.** Section 7.1.

**Plugins.** Section 7.3. Right after the supervisor (section 10).

**Slack.** Go has no official Slack SDK (official ones are for Java, JavaScript, Python and Deno). The adapter uses `github.com/slack-go/slack` (BSD-2-Clause, actively released; v0.36.0 on 2026-11-14), which Slack lists among community-developed tools, and its `socketmode` package. It covers Socket Mode, Block Kit buttons, `chat.update` (`UpdateMessageContext`), thread replies (`MsgOptionTS`) and the current file upload flow (`UploadFileContext`). Socket Mode needs no inbound endpoint. Catch-up (`Since`) reads `conversations.replies`, which allows 50+ requests per minute (Tier 3) for an app installed as an internal (customer-built) app and for a Marketplace app; a commercially distributed app outside the Marketplace gets 1 request per minute and 15 messages per request, which is too little for catch-up. hataori is therefore installed as an internal app in each workspace. `chat.update` is also Tier 3, so progress-line edits are paced. These facts come from Slack's documentation and the library's source (Appendix B, "Documentation read on 2026-11-18"); the library was not run.

Where Slack differs from what the driver interface assumes (Appendix B, "Documentation read on 2026-11-19"; nothing was run):

- A thread is the replies to one message, named by that message's `ts` (`thread_ts`). A thread that hataori opens without a trigger message, as for a `Start` through the control API, starts from a message that hataori posts in the channel.
- Custom slash commands "cannot, however, be invoked in message threads"; only Slack's built-in ones and Giphy's can. Their acknowledgment over HTTP must arrive within 3000 ms.
- `chat.postMessage` takes no key that makes a post safe to repeat. It takes `metadata`, an event type and a payload that every member of the workspace can read, and `conversations.replies` returns it with `include_all_metadata`. `slack-go/slack` v0.36.0 has `MsgOptionMetadata`, and `IncludeAllMetadata` on both reads.
- Text in `text` is Slack's own format; the `markdown` block (`NewMarkdownBlock`) renders standard Markdown, tables and code blocks included, with at most 12,000 characters across a message's markdown blocks. The documentation recommends at most 4,000 characters in `text` and cuts it after 40,000.
- An incoming webhook posts only into the channel it was made for, and into a thread with `thread_ts`, under the name and icon of the app it belongs to.

**Microsoft Teams.** Microsoft's bot SDKs are C#, TypeScript and Python; the older Bot Framework SDK is archived. There is nothing for Go, so the adapter is written against the HTTP protocol. By hand (from Microsoft's documentation, Appendix B, "Documentation read on 2026-11-18"; nothing was run):

- an HTTPS endpoint that receives activities, and validation of the signed token on each one;
- getting and refreshing the service token for outbound calls;
- inbound activity types: message, invoke (button press), conversation update **(unverified)**; `messageUpdate` only tells that a person edited a message and is ignored. hataori edits and deletes its own posts with `PUT` and `DELETE` on `/v3/conversations/{conversationId}/activities/{activityId}`;
- a button press (`invoke`, name `adaptiveCard/action`) is answered in the HTTP response with a status and a JSON body; every activity is answered within 25 seconds;
- Adaptive Cards for choices;
- conversation references, so hataori can post without an inbound activity;
- catch-up of missed replies through Microsoft Graph (`GET /teams/{team-id}/channels/{channel-id}/messages/{message-id}/replies`), with a Graph token and the `ChannelMessage.Read.Group` permission; the Bot Framework API cannot read messages;
- receiving every reply in a thread, which needs the `ChannelMessage.Read.Group` resource-specific consent: by default a bot in a channel gets only the messages that @mention it, and Teams never delivers messages written by other bots, even with that consent;
- files in channel threads through Microsoft Graph (the team's SharePoint storage); the bot's own file flow (file consent card, pre-authorized download URL) works in personal chat only;
- pacing and retry **(unverified)**.

Teams calls hataori at a public HTTPS address, which is a deployment decision. Teams also has a socket mode (public preview, new bots only, not recommended for production by Microsoft); from Go it needs the SignalR hub protocol written by hand, so hataori does not use it in this phase.

### Not scheduled: terminal holders, and sessions people started

Sections 4.3 and 4.8 (decided 2026-11-19, section 10). This work is designed but not scheduled, and holds up no phase. It is tracked in #394, in this order:

1. The holder operations around today's session host, with no change in what a session does.
2. The plugin, and taking in a Claude Code session that a person started in tmux.
3. hataori starting its own sessions in tmux, chosen by config.
4. cmux and iTerm2, last, when hataori runs on macOS.

Its criteria are in Appendix E. Besides the criterion of the later phases, it may change core packages to store a session's holder and window, to record that a workspace is not hataori's, and to add the take-in operations to the control API.

## 9. Future work, not designed here

**Surfaces without threads.** The core names a thread by its number and a link is opaque to it, so a surface with no native threads only needs a front end that makes the links' keys up. The intended rules: a reply to a hataori message continues that message's thread; any other message goes to the thread used last; `/new` starts one; every hataori message begins with the thread's name. If the bot cannot see what a message replies to, one chat is one thread. A surface whose threads expire (LINE OpenChat: 24 hours or two weeks) counts as having none.

## 10. Decisions

### Open

None.

### Decided

- **hataori's own terminal by default; other terminal tools as holders; taking in sessions people started** (#384; the maintainer, 2026-11-19). Sections 4.3 and 4.8, Appendix E. The maintainer asked whether hataori could leave the terminal to the tools people already use to run many agents (tmux, iTerm2, cmux), so that a session living in such a tool could still be listed, given work by the supervisor and operated from hataori. People who already run fleets of Claude Code sessions will not switch tools, and the maintainer was not yet sure that hataori should own the terminal itself. Looking into it showed that c-lord's terminal bugs came from reading the screen, typing keys and matching windows to threads, not from owning a pseudo-terminal. Handing the terminal to tmux or cmux keeps the typing path, through their paste or send command. The input bugs hataori's own host has had so far (#121, #155, #178) are about timing in Claude Code's input box, and would happen the same way through tmux. And the host opens its terminal with `creack/pty`, which is what tmux does at its core. The details below were chosen on the maintainer's direction:
  1. *hataori's own session host stays the default* for the sessions hataori starts: from chat, from a schedule, from the supervisor. *Why:* owning the pseudo-terminal was not the cause of c-lord's bugs, and the host already gives a raw byte stream, a process group and a relay that outlives the daemon.
  2. *hataori is added to what people run, not a replacement*, in three steps: without installing anything, terminals hataori does not own appear in the dashboard; with a Claude Code plugin the person installs, a session they started reports through the same hooks and can be taken in as a thread, which chat, the supervisor and the dashboard then treat like any other; and hataori can start a session inside the person's tool. *Why:* people with fleets of sessions keep their tools, so hataori has to come on top of them.
  3. *The terminal holder is a replaceable part* with five operations: start, deliver input, show the screen, stop, and tell whether the agent is alive. hataori's own host comes first, then tmux; cmux and iTerm2 when hataori runs on macOS. One holder at a time, and each one's input path is checked before it ships. *Why:* the typing path is where terminal bugs live, so each new one is proven on its own.
  4. *State never comes from the screen*, whatever the holder: hooks report it, as today. Which window a session runs in comes from the hook's environment (`TMUX_PANE`; `CMUX_SURFACE_ID` according to cmux's documentation, not run), not from matching windows to threads. *Why:* reading screens and matching windows were the causes of c-lord's terminal bugs.
  5. *Taking a session in is explicit:* per window, by its owner, and the scope is chosen then. The hooks of a session that was not taken in only let it appear in the dashboard's list. They never reach an agent or the chat, and never hold a permission request. *Why:* installing the plugin must not hand every session a person runs to hataori, nor change how a session the person did not hand over behaves.
  6. *Orca is out of scope*, except as a terminal in the list. *Why:* it is an orchestrator at the same level as hataori (an agent per worktree, a coordinating agent, phone notifications); putting it under hataori would manage the same work twice.
  7. *Choosing tmux as the holder for the sessions hataori starts* becomes a config option, later.

  This is designed and not scheduled (section 8). The rest of sections 4.3 and 4.8 is the design's, not the maintainer's: the relay staying with hataori's host whatever holds the terminal; holders that offer only snapshots holding taken-in sessions only; checking a window's terminal and foreground process before taking it in, since an environment is inherited; the plugin written by `hataori plugin` and installed by the person with Claude Code's own commands; reports of sessions not taken in kept only in memory, without text; permission requests of a taken-in thread following its scope's rules; `hataori release`; resuming with hataori's own launch after the window closes; and a taken-in thread's directory never being removed.
- **How the dashboard is reached, and typing into host tmux panes** (#356; the maintainer, 2026-11-19, after using the dashboard). Section 7.2 and Appendix E.
  1. *Three ways in, each with its own proof of who is at the other end:* a native window (Electron, later) of the same OS user, through a socket only that user can open, is the owner with no sign-in; another machine over SSH, with the daemon on loopback, a forwarded port opened as `http://localhost:<port>` and the sign-in link fetched over the same SSH connection, where SSH as the instance's OS user is the proof; the web over HTTPS with a stable name, set up by the operator, with the single-use link first and passkeys after. Plain HTTP that is not loopback still gets no dashboard. *Why:* an app on one's own machine knows its user because whoever has the window is the user; a daemon that may run elsewhere has to find out, and each way of reaching it offers a different proof.
  2. *SSH is the default for remote use, and must not be a chore.* `hataori connect [user@]host` on the person's machine, built for macOS too, uses the system's `ssh` and the person's own keys and config, keeps the forward up, reconnects by itself and remembers the host, so `user@host` is typed once. hataori stores no SSH key. *Why:* most people who run hataori on a Linux server can use SSH, far fewer can give it HTTPS and a domain name, and opening SSH by hand every time is not acceptable.
  3. *No dependency on one network product.* A VPN or tunnel that gives the daemon an HTTPS name inside a private network is documented as one example of the web way; nothing in hataori requires or detects it.
  4. *Typing into host tmux panes,* for the owner only, off by default. hataori's own session terminals stay read-only: input to an agent goes through chat or the control API. On the native window and over SSH a tab switches to "operate" directly; on the web it needs a fresh passkey check, or a recent sign-in when no passkey is registered. It ends after a few minutes without input and when the tab closes. Every start and end is logged with who, when and which pane, never what was typed. How the keys reach the pane was left to the design: by `tmux send-keys` and `paste-buffer`, not an attached client, so that tmux encodes each key for the pane's modes and no window changes size for anyone (section 7.2).
  5. *Idle shell panes are hidden by default,* by the switch that hides asleep threads (#335).

  This replaces, for tmux panes only, "no key is sent to them" in the layout decision of 2026-11-18 below. The details in section 7.2 are the design's, not the maintainer's: one listener per way, with links and sessions bound to it; the `Host` and `Origin` rules on loopback; sessions kept in the page's own storage instead of a cookie; a passkey registered only right after a fresh link or a passkey check; five minutes for the idle period and the sign-in age; where passkeys are kept; and the WebSocket.
- **People in chat, and cleanup: the seven points of #248** (the maintainer, 2026-11-19, all as recommended). Section 8 and Appendix E.
  1. *Commands* are separate: `/new` (repository, prompt, model, effort), `/stop`, `/status`, `/dashboard`, and the same words after the trigger on a surface without commands.
  2. *Order:* the chat commands and buttons (#256 to #259) wait for thread numbers and links (#226, #227), so they are written once against thread numbers; the `discord`, `signin`, `attach` and `cleanup` groups start at once. The thread-number issues (#225 to #229) start without waiting for the people-settings trial.
  3. *Lamp and ping* for a turn the agent started by itself, and for held messages: the agent's turn shows its lamp on the message of the last turn a caller started, and that message's sender is pinged for the agent's turn only when it ends `blocked` or `failure`. A turn a caller started still pings its sender once when it ends. A held message gets its own lamp when its turn starts.
  4. *`hataori attach`:* chat input while someone is attached is delivered as usual (held until the running turn ends, as today). The final answer of a turn typed at the terminal is posted in the chat thread with a "typed at the terminal" line, so the chat thread stays the full record.
  5. *Closing and removal* are host commands (`hataori close`, `hataori remove`), plus an automatic pass that removes threads closed longer than a configured age, off by default. Closing by idle age is off by default. No removal from chat in this phase.
  6. *A dashboard link from chat* is for the owner only in this phase; other people later, after the dashboard's terminal view has been checked against what a scope's people may see. This narrows section 7.2 and Appendix E.
  7. *Items phase 1 left out:* the Stop button is in this phase, as one more issue in group `chat` after #257. The turn footer, attachments into the agent, thread naming beyond the prompt and "ask in chat" for permission requests are not in this phase; each gets its own issue when wanted.
- **Supervisor: the five points of #267** (the maintainer, 2026-11-19, all as recommended). Section 7.1 and Appendix E.
  1. *`hataori mcp` proves its caller* with a token per supervisor session, in the MCP config in the supervisor's state directory. Plugins use the same mechanism (section 7.3).
  2. *The supervisor is a person of its own*, `supervisor:<scope>`, listed in the scope's `people`. A worker sees its messages with a fixed first line added by the daemon.
  3. *Its thread* is a chat thread in the scope's channel, where approvals and budget notices go. Its model is the adapter's cheapest when the config names none.
  4. *An interrupt* is a new control operation that stores a request row in the supervisor's thread; a person answers it with `Answer`, and the daemon then calls `Send` with `Interrupt`. The supervisor's own caller never holds the right to interrupt, and this does not depend on any agent's permission flow.
  5. *Defaults:* a quiet period of 30 s, 1 KB of output per thread and at most 20 threads per digest; 12 supervisor turns and 20 sends and starts per hour, and 2,000,000 tokens per day. Over budget, sends and starts are also refused at the call. The loop guard stops at 3 cycles and is reset by a person's input in that worker thread or in the supervisor's thread.
- **Thread titles are withheld from a caller that may not read text, as protocol 2.0** (#236; the maintainer, 2026-11-19). A title is the first line of the first prompt, so a person wrote it; `ThreadInfo.title` becomes optional, which is a major version, and every 1.x client is refused. Sections 7.3 and Appendix D.
- **A thread is named by hataori's own number, and chat threads link to it** (#215; the maintainer, 2026-11-18). The number is the identity in the control API, events and storage; a chat conversation is a link, and a thread has any number of them. A second link is attached by hand with `/attach <number>` from the channel that should show it, and only in a channel the scope's `show_in` allows. Every link shows the input and answers of the others; what did not reach the agent is not copied. Existing threads get their conversation as their first link; no workspace moves. Sections 5.1, 5.2 and 6.2.
- **hataori manages people, and the core decides whose input reaches the agent** (#216; the maintainer, 2026-11-18). The config lists people, each with a name and ids `<client>:<id>`; the owner and each scope's people are named by person; an id no person lists is a guest and can never start, send, answer or stop. The core resolves the id and checks the scope for `Start`, `Send`, `Answer` and `Stop` from every client; the chat front end has no allow list. Ids are linked to people only in the config. An old config's `owner` and `posters` are converted at start so that the same authors are accepted. Sections 5.1, 5.2 and 5.5.
- **A new workspace gets past the folder-trust dialog by keys** (#56; the maintainer, 2026-11-12). Claude Code 2.1.326 has no supported way to trust a directory in advance, and writing the trust record into its state file was rejected: that file also holds the account. Only the captured dialog is known, by its text. The line is chosen by its text, and keys are sent only before `SessionStart`. Any other screen gets no key, and the start fails as `blocked at start` (section 3.5).
- **The folder-trust dialog that names a project's files** (#423; the maintainer, 2026-11-20, as recommended): answered in a confined session for any file it names; in an ordinary thread only when the scope's config (`trust_files`) lists every file, and otherwise the start fails as `blocked at start`, naming them. Section 3.5.
- **A confined session may not write settings, git or tool configuration files** (#423; the maintainer, 2026-11-20, as recommended): its `PermissionRequest` hook refuses a write to any path in its directory with a part that starts with a dot, and to any path outside it, so a supervisor cannot block its own next start. Section 7.1.
- **Claude Code is not driven through `claude -p`.** Decided by the maintainer; section 3.4. Live attach comes back with the terminal, so the earlier question about take-over between turns is closed.
- **A message that arrives while a turn runs is delivered into the turn**; stopping is the explicit way to interrupt. On the Claude Code path it is held until the turn ends, until delivery during a turn has been checked (section 3.5).
- **Permission requests are allowed automatically** by default, so unattended orders do not stall. "Ask in chat" becomes a per-channel setting with the buttons.
- **Phase 1 is only the dispatched orders.** Nothing a person types into chat directly is in it. c-lord stays for hands-on threads until the phase "people in chat, and cleanup".
- **Dropped:** the HTTP API (replaced by the `hataori` command, the control API and webhook triggers), scheduled tasks and notifications, docker container tracking, table images, the status board (replaced by the dashboard). Scheduled tasks come back first if they are missed.
- **One bot identity per instance**, including a second host.
- **The read-only dashboard starts now**, in parallel with the rest of phase 1 and the later phases (the maintainer, 2026-11-18). This replaces its earlier place in the order, after people in chat. Phase 1's acceptance does not wait for it. Section 8.
- **Order after phase 1:** Codex, then people in chat and cleanup, then the supervisor, then plugins, then Slack and Teams (the maintainer, 2026-11-19). Codex moves ahead of people in chat and the supervisor; this replaces the earlier order, people in chat and cleanup, the supervisor, Codex, Slack, Teams. Section 8.
- **Plugins come right after the supervisor** (the maintainer, 2026-11-19; this was Open 1). The supervisor is the first program outside the daemon with a limited `Caller` that reads events from a position; plugins add the manifest, the grant and hooks on top of that. Section 7.3.
- **The dashboard's layout** is a tree of threads and host terminals, tabs with up to three panes side by side, a details panel and a status bar; the same page becomes a list on a narrow screen (the maintainer, 2026-11-18). It also shows terminals on the host that hataori does not own, for the owner only and for display only; no state is inferred from them and no key is sent to them (for tmux panes the owner operates, replaced on 2026-11-19, above). Section 7.2.
- **A supervisor has no shell:** control API tools and files in its own directory only.
- **Dashboard sign-in is a single-use link**, owner-only at first. In the first version the operator prints it with a `hataori` command on the host; the link issued in chat comes with the "people in chat" phase (2026-11-18). On the web, passkeys follow the first link (2026-11-19, above).
- **The dashboard's page is TypeScript, React and xterm.js, built with Vite**, with the built files committed and embedded, so building hataori needs no Node (2026-11-18).
- **A dashboard field with no source is left out, not guessed.** Pull requests and remaining usage wait for research (2026-11-18).
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
| Attach a thread to a hand-made tmux window | drop | c-lord matched windows to threads by name and read their screens. A session a person started can instead be taken in through its own hooks, with the window named by the hook's environment (section 4.8). |
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
| Folder-trust dialog handling | change | Still needed: the interactive CLI shows it and no hook runs before it. Answered by keys, before `SessionStart` only, for the captured dialog alone (section 10). |
| Unknown-prompt safety net, menu watchdog, dead-pane guard | drop | The screen is not read. A session that does not report its start fails with its terminal output attached. |
| Usage-limit menu answered with "wait" | change | The limit arrives as a hook call; whether the menu on screen still needs an answer is unverified. |

### Status and visibility

| Feature | Verdict | Why |
|---|---|---|
| Lamp on the triggering message (working, done, error) | keep | Derived from the state machine. |
| Stall lamps after 10 s and 30 s of silence | change | Driven by tool start and end reports and by process liveness, not by file silence. |
| Quiet-gap progress line, edited in place | keep | From tool events. |
| Turn footer: context use, model, versions | keep | From the usage the agent records per turn. On the Claude Code path, tokens and model come from the transcript; it gives no context window. |
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
| Owner-only by default; allow-list by user or role; explicit switch to open | keep | The allow list is by person, checked in the core (section 5.5). Roles and an open switch are not designed yet. |
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

Not checked: turns of hours; what the stream shows at a real usage limit or an expired login; `/compact` in `-p`; plan approval; two processes on one session at once; MCP tools given with `--mcp-config` and their approval requests; events for background subagents. (`--tools` was checked later, interactively: "Confining a supervisor session" below.)

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
| Interrupt | `turn/interrupt`: `turn/completed` with `status: "interrupted"` at once. The command and its sandbox wrapper were still running 12 s later, and were gone after the app-server exited. The next `turn/start` worked. The experimental `thread/backgroundTerminals/clean` with the thread's id stops it: the command and its wrapper were gone 3 s after, and the command's item completed with `status: "failed"`, `exitCode: -1` (#317, `interrupt`). The command runs in a process group of its own, not the app-server's. |
| Message during a turn | `turn/steer` with the running turn's id was taken into the turn. |
| Long turn | One turn of 889 s. |
| Resume | A new app-server with `thread/resume` answered from the old conversation. An unknown thread id: a JSON-RPC error, code `-32600`, `no rollout found for thread id <id>` (#317, `resume_unknown`). A thread whose app-server was killed during a turn resumed `idle`, that turn `interrupted` (#317, `exit`). |
| Question | The model's question arrived as an agent message with `questions` and `delivery: "async"`, and the turn went on waiting. No `item/tool/requestUserInput` request was seen. In the default collaboration mode the model asks with `request_user_input_async`: the question is an agent message with `questions` and `delivery: "async"`, and the answer is sent as a user message (`turn/steer` during the turn, `turn/start` after it). `item/tool/requestUserInput` is used only in Plan mode or with the feature `DefaultModeRequestUserInput`. The codec handles both. Both were captured (#317): `request_user_input_async` only with a model whose catalog lists it (`gpt-6-luna`, not `gpt-5.6-luna`); `item/tool/requestUserInput` under `collaborationMode: plan`. Unanswered, the model may end the turn on its own: once after 34 s, once still waiting after 120 s. |
| Turn nobody asked for | None started in 60 s after a turn that ran `(sleep 40; ...) &`; the background child did not outlive its command (#317, `background`). |
| Trust of the workspace | In 0.163.0, `thread/start` with a `cwd` and a sandbox that can write there records the `cwd`'s Git root in the user's `config.toml` as trusted when no trust level is set (from the source, `set_project_trust_level`; `codex exec` does the same). Not taken from a run with a `CODEX_HOME` of its own: none with its own login was made. `-c projects.<cwd>.trust_level="trusted"` sets a trust level without writing the file; the #317 captures used it and left the file unchanged. |
| Final answer | Agent messages carry `phase`: `commentary` or `final_answer`. |
| Second client | With `--listen unix://<path>` (the socket's directory must be private to the user), `codex resume --remote unix://<path> <thread>` showed the thread in the terminal UI. A raw client through `app-server proxy` got no reply to `initialize`. `--listen unix://` serves WebSocket over the Unix socket: a client sends an HTTP upgrade first, then one JSON-RPC message per text frame. Whether `app-server proxy` adds that framing for its stdio side was not checked. |

Not checked: turns of hours; behaviour at a usage limit or with an expired login; two clients driving one thread; turns the agent starts itself over a longer time than 60 s; the trust record in a `CODEX_HOME` of its own. The captures of these rows are in `internal/agent/codex/testdata/0.163.0/` (README there).

### Confining a supervisor session (#268)

Claude Code 2.1.327 and codex-cli 0.163.0, Linux, 2026-11-18. Every attempt ran in a new empty temporary directory; "outside" is a second one. One MCP server was given, a stdio server with one tool that only answers. The captures, and the scripts that took them, are in `internal/agent/claudecode/testdata/2.1.327/confine_*` and `internal/agent/codex/testdata/0.163.0/`. Results: *allowed*; *refused* (the tool was there and the attempt failed); *not available* (the tool was not in the session's list).

**Claude Code**, interactive in a terminal, `--settings <file> --setting-sources project,local --model haiku --permission-mode default --restricted --strict-mcp-config --mcp-config <file>`, plus `--tools` as below. The `--settings` hook answered every permission request with allow, so a refusal is the CLI's confinement, not an unanswered prompt. The tool list is the one the session sent to the model.

| Attempt | `--tools ""` | `--tools "Read,Write,Edit,Glob,Grep"` |
|---|---|---|
| Tool list | `mcp__sv__ping` (and `advisor`, a server-side tool, and `DeferredToolPlaceholder`); no deferred tools, skills or agent types | the five file tools and `mcp__sv__ping`, the same two extras |
| Shell command (`Bash`) | not available | not available |
| Web fetch (`WebFetch`) | not available | not available |
| Subagent (`Agent`) | not available | not available |
| Wake-up or background task (`CronCreate`, `ScheduleWakeup`, `Monitor`) | not available | not available |
| Write outside the directory | not available | refused: `<path> is outside <cwd>; --restricted confines the file tools to the working directory.` No `PostToolUse` or `PostToolUseFailure` hook followed. |
| Read outside the directory | not available | refused, the same message |
| Write inside the directory | not available | allowed |
| Call to the listed MCP server | allowed | allowed |
| `--resume` with the same flags, then `Bash` | | not available |
| `--resume` without the flags, then `Bash` | | allowed: the flags belong to the launch |
| `/model sonnet` in the session, then `Bash` and `Agent` | | not available; the tool list unchanged. Confirming the dialog saved the choice in the user-level settings file, outside the session's directories (`"model": "sonnet"`). |
| `--resume` with `--model sonnet`, then `Agent` | | not available |
| Project settings allowing `Bash`, a project hook, `.mcp.json`, a skill, an agent and a command | | none loaded; the hook did not run |
| `--settings` allowing `Bash`, `WebFetch`, `Agent`, `CronCreate`, with `defaultMode: bypassPermissions` | | `Bash` not available |
| The user's own settings, skills and plugins (no `--setting-sources`) | | nothing added |
| A plugin with an MCP server and a skill, given with `--plugin-dir` | | nothing added |

Without `--tools` (the set of section 7.1 before this check): `Agent`, `ScheduleWakeup`, `Skill`, `ToolSearch`, `ListAgents`, `AskUserQuestion` and the file tools were listed, with 16 deferred tools (`WebSearch`, `CronList`, `PushNotification`, `SendMessage`, `TaskStop` among them), 17 skills and 6 agent types.

**Codex**, `codex app-server` (stdio), `thread/start` with `approvalPolicy: "never"`, `ephemeral: true` and the sandbox below, the MCP server given with `-c` and `default_tools_approval_mode = "approve"`. The prompt asked for one real attempt, since the model otherwise often declined without trying. "Locked" is `workspace-write` with `exclude_slash_tmp` and `exclude_tmpdir_env_var`, `--disable shell_tool --disable unified_exec --disable multi_agent --disable goals --disable sleep_tool --disable apps --disable plugins --disable hooks`, `-c web_search="disabled"` and `-c agents.max_depth=0`.

| Attempt | `read-only` | `workspace-write` | `workspace-write`, `/tmp` and `$TMPDIR` excluded | Locked |
|---|---|---|---|---|
| A command can run at all | yes | yes | yes | no |
| Shell command | allowed | allowed | allowed | not available |
| Web fetch | refused: `curl` could not resolve the host | refused, the same | refused, the same | not available. With the shell and the other features off but web search left on (`workspace_write_no_tools_webfetch`), web search answered. |
| Subagent (`spawn_agent`) | the tool was there: the kept capture shows the model's call failing on its own arguments; a `spawnAgent` completed in an earlier run, not kept | allowed | allowed | not available. `--disable multi_agent` alone left it. |
| Background task (`sleep 600 &`) | allowed; gone after the app-server exited | allowed, the same | allowed, the same | not available |
| Write outside the directory | refused: `writing is blocked by read-only sandbox` | **allowed** (the outside directory was under `/tmp`) | refused: `writing outside of the project` | refused, the same |
| Read outside the directory | allowed | allowed | allowed | not available (no tool reads a file) |
| Write inside the directory | refused | allowed | allowed | allowed |
| Call to the listed MCP server | allowed | allowed | allowed | allowed. Without `default_tools_approval_mode`, refused: `MCP tool call requires approval, but approval policy is never`. |
| A project `.codex/config.toml` with `danger-full-access` and an MCP server | | | the thread kept its sandbox: the write outside was refused. The project's server was loaded, and its call refused for want of an approval. | the same |

`codex app-server` also wrote the project directory into the user's `~/.codex/config.toml` as a trusted project, one entry per run. A parent of it was trusted there, which may be why the project's configuration was loaded at all. `experimentalFeature/list` still reported `unified_exec` on after `--disable unified_exec`.

Not run: a Codex resume and model switch (not needed until the Codex phase); which of the Codex `--disable` flags are each needed; Codex with a project directory whose parents are not trusted (it needs a change to the user's Codex configuration); Claude Code managed settings (they need a file outside the user's directories); what a person can do by typing at the terminal (`/add-dir`, `/cd`, `!`), which is not the model's confinement.

### tmux: typing into a pane (#356)

Run on 2026-11-19 against tmux 3.4 on Linux, on a tmux server of its own (`-L`, no configuration file), in a 100×30 window with no client attached. The program in the pane was bash running `od -c` on what it read, with the terminal in raw mode where a byte would otherwise be edited by the terminal driver. Not run on macOS.

| Check | Result |
|---|---|
| `send-keys Up`, the program in normal cursor mode | `ESC [ A` |
| `send-keys Up` after the program printed `ESC [?1h` (application cursor keys; `#{keypad_cursor_flag}` became 1) | `ESC O A` |
| `set-buffer`, then `paste-buffer -p`, after the program printed `ESC [?2004h` | `ESC [200~` and the text; the program read 8 bytes, so the closing mark was not read |
| Text with a DEL in it given on standard input to `load-buffer -b <name> -`, then `paste-buffer -d -b <name>` | the same bytes, the DEL included; the buffer was gone afterwards |
| `#{window_width}x#{window_height}` and `#{session_attached}` before and after the keys | `100x30` and `0`, both times |
| `#{pane_current_command}` of panes started as `exec -a -bash bash` and `exec -a -zsh zsh` (how a login shell names itself) | `bash` and `zsh` |
| The default of `window-size` | `latest` |
| `attach-session -f` in the manual page | the client flags include `ignore-size`; `-r` is `read-only,ignore-size` |

On tmux 3.5a on macOS (#399), the holder's checks passed against `stty raw` and `cat` in a pane: a paste of several lines with a tab, a DEL, Japanese and a leading `-` arrived byte for byte between the marks, and `send-keys Enter Escape Up Down` as `\r`, `ESC`, `ESC [ A`, `ESC [ B`. There, under a locale that is not UTF-8 (`LC_CTYPE=C`) and with `TMUX` unset, the client printed every tab of a `-F` format, in `list-panes` and in `new-session -P`, as `_`, and every character outside ASCII too; with `-u`, or with `TMUX` set in its environment even empty, it printed them as they are. A tab in a session's name it printed as `\t` either way, as tmux 3.4 did; a tab in a window's name or a directory it printed like the separator, which the `s/\t/ /` of `hostterm`'s format turns into a space (#483).

Later, for #362: on tmux 3.4, `send-keys` typed a name it cannot encode (`S-Tab`, `S-a`, `C-{`) into the pane as its letters and dropped some (`C-Enter`, `C-BSpace`), and an argument ending in `;` ended the command (`M-;` typed `M-`; `M-\;` sent `ESC ;`); `paste-buffer -S` was refused with "unknown flag -S" and the buffer kept. On tmux 3.8 (macOS CI), `paste-buffer` without `-S` gave a DEL as `^?`: tmux 3.7 passes a paste through vis(3) unless given `-S`.

### Claude Code plugins, and hooks in a tmux pane (#384)

Run on 2026-11-19 against Claude Code 2.1.328 and tmux 3.4 on Linux. Every run of Claude Code had a throwaway directory as both `HOME` and `CLAUDE_CONFIG_DIR`, and no login. The real configuration was never used. Its plugin files were compared before and after the runs: the only change was the refresh time of the official marketplace, written by another session that uses the real configuration (the throwaway sessions wrote their own refresh into the throwaway directory, at another time). In the throwaway `.claude.json`, onboarding was marked done and the two test directories trusted. A plugin `probe`, in a marketplace that is a local directory, registered `SessionStart`, `UserPromptSubmit`, `PermissionRequest`, `Stop` and `SessionEnd`, each running a script that wrote its environment, its chain of parent processes, what tmux said of its pane, and its payload to a file of its own for each call. The sessions ran in panes of a tmux server of its own (`-L`, no configuration file). With no login no turn reached the model, so `PermissionRequest` and `Stop` never fired.

| Check | Observed |
|---|---|
| `claude plugin marketplace add <directory>`, then `claude plugin install probe@probe-market` | The user settings file got `extraKnownMarketplaces` (source `directory`, with its path) and `enabledPlugins` with `"probe@probe-market": true`. `plugins/` got `known_marketplaces.json`, `installed_plugins.json` (scope `user`) and a copy of the plugin under `plugins/cache/`. `claude plugin list` showed it enabled, "Read from" the marketplace's directory. |
| Where the hooks ran from | `CLAUDE_PLUGIN_ROOT` was the marketplace's directory, not the copy in the cache. `CLAUDE_PLUGIN_DATA` was a directory of the plugin's own under `plugins/data/`. |
| A plugin option | `userConfig` declared in `plugin.json` and set with `claude plugin configure <plugin> --values-stdin` was stored in the user settings file under `pluginConfigs`, and reached every hook as `CLAUDE_PLUGIN_OPTION_ROOT`. |
| Hooks without a login | `SessionStart` ran at start and `UserPromptSubmit` when a prompt was submitted; then the CLI answered "Not logged in". |
| Which sessions | The plugin's `SessionStart` ran in interactive sessions in two directories, in a session started with `--settings` that registered hooks of its own (both sets ran), and under `claude -p`. It did not run under `--setting-sources project,local`. |
| The hook's environment in a pane | `TMUX` (`<socket path>,<server pid>,<session index>`), `TMUX_PANE` (`%0`, `%1`, ...), `TERM_PROGRAM=tmux`, `TERM=tmux-256color`, `CLAUDE_PROJECT_DIR`, `CLAUDECODE=1`, and `CLAUDE_CODE_ENTRYPOINT`: `cli` in an interactive session, `sdk-cli` under `-p`. On a plain pseudo-terminal there were no `TMUX` variables. |
| `HATAORI_HOST_SOCKET` in the session's environment | The plugin's hook saw it. |
| The hook's parent processes | the hook, under `sh` with no terminal, under the Claude Code process (named after its version, `2.1.328`) on the pane's terminal, which led its own process group and was the terminal's foreground group. `#{pane_pid}` was the Claude Code process when it was the pane's command, and the shell when `claude -p` was started from a shell. `#{pane_current_command}` was `2.1.328`. |
| A prompt pasted through tmux | 1,441 characters in 41 lines (a leading `-`, backticks, quotes, `$HOME`, `${PATH}`, Japanese, `<>&\|;`, a trailing `;`, one tab per line), by `load-buffer -b <name> -` and `paste-buffer -p -d -b <name>`, then `send-keys Enter` 1.5 s later. `UserPromptSubmit` reported every character unchanged except the tabs, each of which was four spaces. There was no `pasted_content` tag at this length. The buffer was gone afterwards. |
| The same without tmux | Written to a plain pseudo-terminal inside bracketed-paste marks, then Enter: `tab<TAB>here, two<TAB><TAB>tabs; end` arrived as `tab    here, two        tabs; end`, four spaces for each tab and not spaces to the next tab stop (#393). |
| `/exit` | `SessionEnd` with reason `prompt_input_exit` |
| `kill-pane` on a session with the plugin | `SessionEnd` with reason `other`, run after the pane had gone |
| `kill-server` under a session with both sets of hooks | `SessionEnd` with reason `other`, from both sets; the Claude Code process had lost its terminal and its parent |
| `claude -p` ending | `SessionEnd` with reason `other` |

Not run: a `PermissionRequest` whose hook prints nothing while a dialog would show, since that needs a turn and so a login (Claude Code's hooks reference says that a hook exiting 0 with no output "has no decision to report, so the tool call continues through the normal permission flow"); a `claude` that the session's own shell tool starts; macOS.

### tmux: holding an agent's terminal (#384)

Run on 2026-11-19 against tmux 3.4 on Linux, on tmux servers of their own, as above. The reading program was bash running `od -c` with the terminal in raw mode, after it printed `ESC [?2004h`.

| Check | Result |
|---|---|
| `paste-buffer -p -d` of `one<LF>two` | `ESC [200~`, `one`, CR, `two`, `ESC [201~`: the line feed became a carriage return |
| `paste-buffer -p -r -d` of the same | `ESC [200~`, `one`, LF, `two`, `ESC [201~` |
| `paste-buffer -p` of text with a tab and a DEL | the same bytes, and both marks were read |
| `send-keys Enter Escape Up` | `\r`, `ESC`, `ESC [ A` |
| `pipe-pane -O` set on a pane running a placeholder, then `respawn-pane -k` with a command that prints at once | `#{pane_pipe}` was still 1, and the pipe's file had the command's output from its first byte, the bracketed-paste switch included |
| `remain-on-exit on`, then a command that exits 7 | `#{pane_dead}` 1, `#{pane_dead_status}` 7 |
| `display-message -p -t <pane>` for a pane that has gone | exit status 0, every field empty; `list-panes -a` no longer listed it |
| Pane ids after the server was killed and started again | the first pane was `%0` again, and the server's process id in `TMUX` was another |

### Documentation read on 2026-11-12

- Agent SDK overview: Python and TypeScript only; other languages run the CLI with `-p`; the note on third parties and claude.ai login.
- "Run Claude Code programmatically": `--bare` does not read the subscription login and "will become the default for `-p` in a future release"; on SIGTERM the turn is left unfinished and running commands are terminated.
- Hooks reference: the list of hook events and their payloads; `Stop` "does not run if the stoppage occurred due to a user interrupt"; `StopFailure` runs instead when a turn ends with an API error and names the error type; command hooks time out after 600 seconds by default; in an interactive session no hook runs until the folder-trust dialog is accepted.
- Permissions: trust is keyed on the repository root, and trusting a parent directory does not extend to a repository nested in it.
- Channels reference: a channel is an MCP server started by Claude Code; `notifications/claude/channel` pushes an event into the session; replies go through a tool the channel offers; permission requests can be relayed; custom channels need `--dangerously-load-development-channels`, which shows a warning dialog and is ignored with `-p`; notifications are not acknowledged.
- Agent view: `claude --bg` sessions run under a supervisor; `claude agents --json` reports their state; no way to send input to a running session other than interactively is described.

### Documentation read on 2026-11-18

Read for section 8 (Slack, Microsoft Teams) and for the Codex rows above. Nothing here was run.

- Slack tools, https://docs.slack.dev/tools/: official Bolt frameworks for Java, JavaScript and Python, and SDKs for Deno, Java, Node and Python; none for Go.
- Slack community-developed tools, https://docs.slack.dev/tools/community-developed-tools/: lists `slack-go/slack` under Go.
- `slack-go/slack` v0.36.0, https://github.com/slack-go/slack/releases/tag/v0.36.0 (published 2026-11-14); licence https://github.com/slack-go/slack/blob/v0.36.0/LICENSE (BSD-2-Clause); Socket Mode client https://github.com/slack-go/slack/tree/v0.36.0/socketmode. In that tag, `UpdateMessageContext` and `MsgOptionTS` are in `chat.go`, and `UploadFileContext` in `files.go` uses `files.getUploadURLExternal` and `files.completeUploadExternal`.
- Slack interactivity, https://docs.slack.dev/interactivity/handling-user-interaction/: the HTTP acknowledgment of an interaction "must be sent within 3 seconds of receiving the payload", or the user sees an error; the page does not mention Socket Mode.
- Slack Socket Mode, https://docs.slack.dev/apis/events-api/using-socket-mode/: each envelope, interactive payloads included, is acknowledged by sending back its `envelope_id`; this page states no time limit.
- Slack `conversations.replies`, https://docs.slack.dev/reference/methods/conversations.replies/, and `chat.update`, https://docs.slack.dev/reference/methods/chat.update/: Tier 3, 50+ per minute.
- Slack rate limit change for non-Marketplace apps, https://docs.slack.dev/changelog/2025/05/29/rate-limit-changes-for-non-marketplace-apps/: for commercially distributed apps outside the Marketplace, `conversations.history` and `conversations.replies` allow 1 request per minute and at most 15 objects; internal customer-built apps keep 50+ per minute and up to 1,000 objects, and "This change will not impact Marketplace apps."
- Teams without an SDK, https://learn.microsoft.com/en-us/microsoftteams/platform/agents-in-teams/connect-agent-without-sdk: the token checks on each inbound request (`401` when one fails); the service token by client credentials; update and delete with `PUT` and `DELETE` on `{serviceUrl}/v3/conversations/{conversationId}/activities/{activityId}`; "For an invoke activity (`"type": "invoke"`), return the invoke response as the HTTP response, with its status code and JSON body. For any other activity, return `200 OK` once you've handled it. Respond within 25 seconds."; socket mode is in public preview, only for newly created bots, and an HTTPS endpoint is recommended for production; it is SignalR ("You can't use a plain WebSocket library unless you implement the SignalR hub protocol yourself"), with Microsoft-supported clients for JavaScript and .NET only, one socket per geo (`amer`, `emea`, `apac`), each replaced before its token expires.
- Teams conversations, https://learn.microsoft.com/en-us/microsoftteams/platform/bots/build-conversational-capability#update-and-delete-messages-sent-from-bot: `messageUpdate` with `eventType: "editMessage"` notifies that a sent message was edited.
- Bot Framework Connector API reference, https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-api-reference: the conversation operations create, send, reply, update and delete activities, list conversations and members, upload attachments and send history; none reads the messages of a conversation.
- Microsoft Graph, list channel message replies, https://learn.microsoft.com/en-us/graph/api/chatmessage-list-replies: `GET /teams/{team-id}/channels/{channel-id}/messages/{message-id}/replies`; application permission `ChannelMessage.Read.Group` (resource-specific consent), or `ChannelMessage.Read.All`.
- Teams channel and group conversations, https://learn.microsoft.com/en-us/microsoftteams/platform/bots/how-to/conversations/channel-and-group-conversations, and receiving all messages, https://learn.microsoft.com/en-us/microsoftteams/platform/agents-in-teams/enable-receive-all-chat-messages: by default a bot in a channel gets only the messages that @mention it, not replies to its own messages without one; the resource-specific consent `ChannelMessage.Read.Group` delivers all user messages in the team's channels; "Teams doesn't deliver messages authored by other agents or bots, including the Workflows (Power Automate) bot", even with that consent.
- Teams files, https://learn.microsoft.com/en-us/microsoftteams/platform/bots/how-to/bots-filesv4: the file accessor works in personal chat ("Not explicitly supported" in group chat and channel); bot file consent in personal chat only; "Send or retrieve files with Graph" in all three; channel files are in the team's SharePoint site; a bot reads a received file from a short-lived, pre-authorized `downloadUrl`.
- Codex Unix socket transport, https://github.com/openai/codex/blob/main/codex-rs/app-server-transport/src/transport/unix_socket.rs: each connection is accepted with `tokio_tungstenite::accept_hdr_async_with_config`, a WebSocket upgrade; https://github.com/openai/codex/blob/main/codex-rs/app-server-transport/src/transport/websocket.rs reads each text frame as one message.
- Codex questions: https://github.com/openai/codex/blob/main/codex-rs/protocol/src/config_types.rs (`allows_request_user_input` is true only for `ModeKind::Plan`), https://github.com/openai/codex/blob/main/codex-rs/tools/src/tool_config.rs (or in the default mode with the feature `DefaultModeRequestUserInput`), https://github.com/openai/codex/blob/main/codex-rs/core/src/tools/handlers/request_user_input_async.rs (the `request_user_input_async` tool emits an agent message with `questions` and `delivery: Async`), https://github.com/openai/codex/blob/main/codex-rs/prompts/src/model_messages.rs (its description: "any reply arrives asynchronously as a new user message").
- Codex methods, https://github.com/openai/codex/blob/main/codex-rs/app-server-protocol/src/protocol/common.rs: `thread/backgroundTerminals/clean`, `…/list` and `…/terminate`, each `#[experimental]`, with `threadId` in the params.

### Documentation read on 2026-11-19

Read to split the Slack phase (section 8). Nothing here was run.

- Slack slash commands, https://docs.slack.dev/interactivity/implementing-slash-commands: "Slash commands created by developers cannot, however, be invoked in message threads"; built-in commands and Giphy's can; the acknowledgment "must be received by Slack within 3000 milliseconds of the original request being sent" (HTTP; the page does not mention Socket Mode).
- Slack `chat.postMessage`, https://docs.slack.dev/reference/methods/chat.postMessage/: its arguments include `thread_ts`, `blocks`, `text`, `markdown_text` and `metadata`, and none that makes a post safe to repeat; `metadata` is "accessible to any app or user who is a member of that workspace"; keep `text` within 4,000 characters, and more than 40,000 are cut with a `message_truncated` warning; `markdown_text` is not combined with `text` or `blocks`; about one message per second per channel, with bursts allowed.
- Slack `markdown` block, https://docs.slack.dev/reference/block-kit/blocks/markdown-block: standard Markdown in messages, tables and code blocks included; 12,000 characters across the markdown blocks of one payload; headers render at one size and images become links.
- Slack `conversations.replies`, https://docs.slack.dev/reference/methods/conversations.replies/: `ts` of the parent or of any message in the thread, `oldest`, `latest`, `inclusive`, `cursor`, `limit` and `include_all_metadata`; Tier 3.
- Slack incoming webhooks, https://docs.slack.dev/messaging/sending-messages-using-incoming-webhooks/: a webhook is tied to one channel, which cannot be overridden, nor can the name or icon; a reply goes into a thread with `thread_ts`; the response carries no `ts`.
- `slack-go/slack` v0.36.0: `MsgOptionMetadata` in https://github.com/slack-go/slack/blob/v0.36.0/chat.go, `NewMarkdownBlock` in https://github.com/slack-go/slack/blob/v0.36.0/block_markdown.go, `IncludeAllMetadata` on the history and replies parameters in https://github.com/slack-go/slack/blob/v0.36.0/conversation.go.

Read for sections 4.3 and 4.8 (#384). Nothing here was run.

- Claude Code hooks reference, https://code.claude.com/docs/en/hooks: "Exit code 0 with no output means the hook has no decision to report, so the tool call continues through the normal permission flow. The hook can deny the call, but staying silent doesn't approve it." Any other exit code but 2 is a non-blocking error, and on events such as `PreToolUse` the person sees "a `<hook name> hook error` notice in the transcript". Command hooks time out after 600 seconds, 30 on `UserPromptSubmit`. A plugin's hooks "merge with your user and project hooks", and "A plugin's or skill's copy of the same handler stays separate."
- cmux, https://github.com/manaflow-ai/cmux, release v0.65.2.2 of 2026-10-27: "Open source Ghostty-based macOS terminal with vertical tabs and notifications for AI coding agents."
- cmux CLI contract, https://github.com/manaflow-ai/cmux/blob/v0.65.2.2/docs/cli-contract.md: `CMUX_WORKSPACE_ID` "Default workspace context inside cmux terminals", `CMUX_SURFACE_ID` "Default surface context inside cmux terminals", `CMUX_SOCKET_PATH` "Canonical socket path override". `paste` sends text "through the Cmd+V paste path (`terminal.paste`). The CLI sends the text unchanged; Ghostty brackets it when the program enabled bracketed paste (otherwise newlines become Enter) and replaces unsafe control bytes with spaces." `send` sends text "as keystrokes", and `send-key` "one key". Before writing, `send` and `paste` "refuse when an agent's prompt holds text someone is typing, or when a question or permission dialog is open in an agent", unless `--force`. `read-screen` reads "terminal text from a surface". `new-workspace` and `new-surface` take `--command`; there are `close-surface`, `surface-health` and `events` ("Stream reconnectable cmux events as newline-delimited JSON"), and tmux-like `capture-pane` and `pipe-pane`, which pipes "the selected surface's text".
- cmux agent session tracking, https://github.com/manaflow-ai/cmux/blob/v0.65.2.2/docs/agent-session-tracking-spec.md: the surface id is "persisted in the session snapshot and ... REUSED verbatim on restore", and regenerated only on a collision; the workspace id is "ALWAYS regenerated on restore".
- cmux agent hooks, https://github.com/manaflow-ai/cmux/blob/v0.65.2.2/docs/agent-hooks.md: "Claude Code is handled by the cmux Claude wrapper when Claude Code integration is enabled in Settings"; its table lists Claude Code's hooks as "wrapper-injected settings", with `PermissionRequest`. Outside a cmux shell, `cmux claude-hook install` writes the user's Claude Code settings file.
- iTerm2 Python API, `Session`, https://iterm2.com/python-api/session.html: `async_send_text`, "Send text as though the user had typed it."; `session_id`, "the globally unique identifier for this session"; `async_get_screen_contents`, "Returns the contents of the mutable area of the screen."; `async_inject`, "Injects data as though it were program output." No paste method is documented.
- iTerm2 variables, https://iterm2.com/documentation-variables.html: a session's `id` is "A unique identifier for the session", `termid` the "Window, tab, and pane number as used in the `$TERM_SESSION_ID` environment variable", and `tty` and `pid` its terminal and root process. `ITERM_SESSION_ID` is not on the page; its form `w0t0p0:<UUID>` is known from third-party code only.

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
	// Thread returns state, open requests and the last output, capped at
	// recent bytes, and the thread's last events, at most events of them
	// (and at most MaxEvents), newest last.
	Thread(ctx context.Context, id ThreadID, recent, events int) (ThreadDetail, error)
	// ThreadEvents returns the thread's events before position before, or
	// its last ones when before is 0: at most n of them (and at most
	// MaxEvents), newest last, as Thread returns them. Asking again before
	// the oldest one returned reads the thread's log back to its start; an
	// empty result is the start. Since protocol 3.3.
	ThreadEvents(ctx context.Context, id ThreadID, before uint64, n int) ([]Event, error)
	// SetName records the name the chat service shows people for a
	// conversation: a scope's own channel when l.Thread is empty (l.Client
	// owns the scope and l.Channel is its key), else a linked thread. An
	// empty name forgets it. A change writes one named event; the same
	// name again changes nothing. It needs OpName in the conversation's
	// scope: a chat front end's caller has it. Since protocol 3.5.
	SetName(ctx context.Context, l LinkRef, name string) error
	// Scopes lists the scopes the caller may see, by key, each with the
	// name a person knows it by: the config's, else its channel's as the
	// chat service shows it (SetName), which is text and left out for a
	// caller that may not read text. It needs OpRead. Since protocol 3.5.
	Scopes(ctx context.Context) ([]ScopeInfo, error)

	Start(ctx context.Context, r StartRequest) (ThreadID, error)
	// key makes Send safe to repeat; for chat, the surface's message id.
	// in.Link names the link the input came through, if any.
	Send(ctx context.Context, id ThreadID, key string, in Input, busy Busy) error
	Answer(ctx context.Context, req RequestID, a Answer) error
	// AskInterrupt asks a person to approve interrupting the running turn of
	// thread id, with reason. It stores an approval request in the caller's
	// AsksIn thread and returns its id at once; nothing is interrupted until
	// a person approves it with Answer. A repeated key returns the original
	// request; an empty key makes no repeat check. An open request for the
	// same running turn is returned instead of a second one. The caller's
	// AsksIn thread cannot be the target (ErrForbidden). Since protocol 3.10.
	AskInterrupt(ctx context.Context, id ThreadID, key string, reason string) (RequestID, error)
	// Stop interrupts the thread's running turn. A turn other than 0 names
	// it by turn_started's number, checked in the step that interrupts: when
	// it is not the running turn, nothing is interrupted and the call is
	// refused. Since protocol 3.14.
	Stop(ctx context.Context, id ThreadID, turn int64) error

	// CloseThread ends the agent process of an idle, asleep or failed thread
	// and closes it (section 4.5): the workspace stays, and Send is
	// ErrClosed. An idle thread whose last turn left background tasks
	// running, or with a message or an answer accepted and not yet reported
	// by a turn, is ErrRefused and stays as it is. RemoveThread removes the workspace of a closed thread only
	// through the proof of section 4.6, by a rename into trash; a proof that
	// does not hold is ErrRemovalRefused with the reason, and the thread
	// stays closed. Each removal and refusal writes one event. Both are
	// checked like Stop and need OpClose and OpRemove, which only the
	// owner's connection on the control socket has. On the wire they are the operations
	// Close and Remove (the client's Close closes its connection). Since
	// protocol 2.3.
	CloseThread(ctx context.Context, id ThreadID) error
	RemoveThread(ctx context.Context, id ThreadID) error

	// Link shows the thread in one more conversation and returns its link.
	// It is checked like Send, and refused when l.Channel is neither the
	// scope's own channel nor in its show_in for l.Client; a refusal makes
	// nothing and is logged with the thread, the client and the channel.
	// With l.Thread empty, the core asks l.Client's ScopeOwner to open a
	// conversation in l.Channel, titled like the thread, once the link is
	// allowed, unless the thread has a link there already, which it returns.
	// Linking a conversation twice to the same thread changes nothing; to
	// another thread, it is ErrLinkedElsewhere. Since protocol 3.4.
	Link(ctx context.Context, id ThreadID, l LinkRef) (LinkRef, error)

	// Events delivers events after a position, then live ones. Between the
	// two it delivers one mark of kind CaughtUp, which is not stored, so a
	// reader can stop after the stored events.
	Events(ctx context.Context, f Filter, after uint64) (<-chan Event, error)

	// SignInLink makes a single-use, short-lived dashboard sign-in link for
	// the caller's person, and the session it opens belongs to that person:
	// its Caller has the scopes that person may act in (every scope for the
	// owner) and OpRead. It needs OpSignIn, which the owner's connection on
	// the control socket has, and which a chat front end gives the caller it
	// makes for a person who asks in chat with /dashboard (#259), and sends
	// the link to in a direct message, never through its outbox;
	// the dashboard's caller never has it. Who may get a link is one rule in the core, today the
	// owner only (section 10); a guest and anyone else are refused and
	// logged with the client and the id. The token is never logged or
	// stored as an event. way names the dashboard's listener the link is
	// for, and the session it opens works only there: WayLocal, WayWeb, or
	// empty for the loopback listener when the instance has one and the web
	// listener otherwise (section 7.2). On an instance without a dashboard
	// it is ErrNoDashboard. Since protocol 1.7; for a chat caller since
	// 1.11; way since 3.2.
	SignInLink(ctx context.Context, way Way) (string, error)

	// Workspace reads the state of a thread's workspace and changes nothing.
	// It is the check of section 4.6 as numbers, answering from the
	// remote-tracking refs as last fetched; it does not ask the remote.
	Workspace(ctx context.Context, id ThreadID) (WorkspaceState, error)

	// Terminal streams the terminal output of the thread's current session
	// from its session host, read-only, until ctx ends (section 7.2). The
	// first chunk continues after position after of run, or, when after is
	// 0, belongs to another run or is no longer kept, is the end of the kept
	// log with Reset set. A session whose host has ended sends the end of the
	// last run's log with Reset and an empty Run, then nothing until it runs
	// again. When the thread gets a new session, the stream goes on with it,
	// from a chunk marked Reset. A session on pipes has no terminal:
	// ErrNotFound. It takes no input, and the stream's connection carries
	// none.
	Terminal(ctx context.Context, id ThreadID, run string, after uint64) (<-chan TerminalChunk, error)

	// HostTerminals lists the terminals on the host that no session host
	// owns: tmux panes, then other terminals. HostScreen returns one tmux
	// pane's screen, by TmuxPane.Pane. Display only; both need OpHost, and
	// the core answers only the owner, and only when the config's
	// host_terminals is true (section 7.2). Since protocol 1.10.
	HostTerminals(ctx context.Context) ([]HostTerminal, error)
	HostScreen(ctx context.Context, pane string) (string, error)
	// HostView is HostScreen with the cursor's position, read right after
	// the screen; allowed as HostScreen is. The dashboard reads it for a
	// pane being operated. Since protocol 3.13.
	HostView(ctx context.Context, pane string) (HostView, error)

	// OperateHost lets the caller type into one tmux pane on the host that
	// no session host owns, by TmuxPane.Pane (section 7.2). It needs
	// OpHostInput, which the core honours only for the owner and only when
	// the config's host_input (which needs host_terminals) is true. It
	// refuses a pane in which one of hataori's own processes runs, and a
	// pane already being operated (ErrBusy, naming since when and by whom).
	// It logs the start and the end with the person, the pane and the
	// reason, and never a key. On the socket its connection becomes a
	// stream both ways (below). Since protocol 3.11.
	OperateHost(ctx context.Context, pane string) (HostOperation, error)

	// TakeIn makes a thread of a window that a person started (section
	// 4.8): the window's last report must be of an interactive session,
	// the window must not be taken in already, and its holder's check must
	// pass; else nothing changes, and the refusal is logged. The thread is
	// made in req.Scope as Start makes one, with its first link; its
	// session is the window's, and its workspace is the session's
	// directory, recorded as not hataori's, which no removal touches. It
	// needs OpTakeIn, which only the owner's connection on the control
	// socket has. Since protocol 3.17 (#400).
	TakeIn(ctx context.Context, req TakeInRequest) (ThreadID, error)

	// Release ends a take-in: the thread goes asleep, its window's hooks
	// only report again, and the window is left as it is. A thread that is
	// not idle is refused. It needs OpTakeIn. Since protocol 3.17.
	Release(ctx context.Context, id ThreadID) error

	// Limits returns each agent's remaining usage as its sessions last
	// reported it; an agent that reports none has no entry. It needs
	// OpRead and no scope: the usage is the account's. Since protocol 1.13.
	Limits(ctx context.Context) ([]AgentLimits, error)

	// AttachTerminal readies the thread's agent for a terminal on the host
	// (`hataori attach`, section 4.3): it needs OpAttach, which only the
	// owner's connection on the control socket has, and the owner's
	// person. A closed, removed or failed thread and an agent with no
	// terminal are refused and change nothing; an asleep thread is woken
	// with no input first. The result names the session host's socket,
	// which the terminal connects to itself. Since protocol 2.1.
	AttachTerminal(ctx context.Context, id ThreadID) (Attachment, error)
}

// Attachment: Socket is the session host's socket path, Number the
// thread's number.

// Way is one of the dashboard's listeners (section 7.2), "local" or "web"
// on the wire.
type Way string

const (
	WayLocal Way = "local" // the loopback listener: the host, and ports forwarded to it over SSH
	WayWeb   Way = "web"   // the web listener, over HTTPS
)

// AgentLimits: At is when the report was made, Every how often the
// agent's sessions report while one runs. Older than Every, a report is
// not current, and the dashboard shows it with its time.
type AgentLimits struct {
	Agent   string
	At      time.Time
	Every   int64 // seconds
	Windows []LimitWindow
}

type LimitWindow struct {
	Name    string     // the agent's name for it: five_hour, seven_day, primary, secondary
	Minutes int64      // its length; 0 when the agent does not say
	Used    int        // percent
	Resets  *time.Time // nil when the agent does not say
}

type HostTerminal struct {
	Device   string     // the terminal device
	Process  string     // the command of its foreground process
	Activity *time.Time // last output, when known
	Tmux     *TmuxPane  // absent for a terminal outside tmux
	Reported *ReportedSession // what the plugin's hooks reported from this tmux pane; absent without them (since 3.15)
}

// ReportedSession is what the hooks of a session that a person started in
// their own window last reported (section 4.8). No text of the session.
type ReportedSession struct {
	Window  string    // holder and reference, as TakeIn takes it: "tmux:<socket>,<server pid>,<pane>"
	Agent   string    // the adapter's name
	Session string    // the agent's session id
	Dir     string
	Event   string    // the last hook event's name
	At      time.Time // when the daemon took it
	Thread  *ThreadID // the thread whose session this is, if any
}

type TmuxPane struct {
	Pane       string // the pane's id, such as %3; what HostScreen takes
	Session    string
	Window     string // the window's index
	WindowName string
	Index      string // the pane's index in its window
	Dir        string
	Attached   bool
}

// HostView: Screen as HostScreen gives it; CursorX and CursorY from 0, the
// row counted from the top of the screen.
type HostView struct {
	Screen           string
	CursorX, CursorY int
}

// HostOperation is one pane being operated. It ends when ctx ends, when
// End is called, when no key came for Idle, when the pane goes or comes
// to run one of hataori's processes, or when the daemon stops. Done is
// then closed and Reason says which: "closed", "ended", "idle", "pane
// gone", "hataori's own" or "stopping" (control.HostClosed and the rest).
// The socket's client says "disconnected" when its connection breaks
// without the daemon's last message.
type HostOperation interface {
	Send(ctx context.Context, keys []HostKey) error // in order; ErrEnded once it ended
	Idle() time.Duration                             // the config's host_input_idle
	Done() <-chan struct{}
	Reason() string
	End()
}

// HostKey is one input to a pane. Key is one of a fixed list of tmux key
// names (control.ValidHostKey: Enter, Tab, BTab, BSpace, Escape, the
// arrows, Home, End, PPage, NPage, IC, DC, F1 to F12, and C-, M- and S-
// with them or with one character, as far as tmux encodes each), passed
// to send-keys. Otherwise Text is
// written to the pane through a buffer of the operation's own (load-buffer
// from standard input, then paste-buffer -d, with -S since tmux 3.7 so
// that control characters go as they are), with Paste as a paste
// (paste-buffer -p, as tmux pastes it). hataori logs and stores neither; Text is in the
// buffer for one paste, and the buffer is deleted after a failed paste,
// at the end of the operation and at the daemon's start.
type HostKey struct {
	Key   string
	Text  string
	Paste bool
}

type TerminalChunk struct {
	Run   string // the session host's run, opaque; a new run starts a new count; empty when no host runs
	Pos   uint64 // the byte position after Data, counted since the run started; 0 when Run is empty
	Reset bool   // Data is the end of the log, not a continuation: redraw
	Cols  int    // the terminal's size
	Rows  int
	Data  []byte // raw and uninterpreted; text, so optional
}

type WorkspaceState struct {
	Branch      string     // empty when HEAD is detached
	Base        string     // the commit the branch started from: where it meets origin/HEAD, else its upstream, else HEAD
	Added       int        // lines, against Base, staged and unstaged changes of tracked files included
	Removed     int
	Uncommitted int        // modified, staged and untracked files, each once; ignored files not counted
	Unpushed    int        // commits reachable from HEAD or a local branch and from no remote-tracking ref
	Fetched     *time.Time // the last fetch, push or clone; nil when the repository does not say
	// PullRequest is added only if the research finds a source (section 7.2).
}

// Caller is who is asking. An API value is bound to one Caller,
// and the core checks every call against it.
type Caller struct {
	// AsksIn is where approvals the caller asks for are shown and answered.
	// The daemon sets it when it binds the caller; zero: it cannot ask.
	AsksIn ThreadID
	// From is the "<client>:<id>" a relayed input came from, such as a chat
	// message's author. The core resolves it to a person and sets Principal;
	// an id no person lists is a guest, with no Principal. Empty for a
	// caller the daemon authenticated itself.
	From      string
	Principal Principal // a person's name (section 5.5)
	Scopes    []string  // the scopes it may see and act in; a thread of another is answered as none (ErrNotFound)
	May       Ops       // any of OpRead, OpStart, OpSend, OpAnswer, OpStop, OpSignIn, OpHost, OpAttach, OpClose, OpRemove, OpName, OpAsk, OpHostInput, OpTakeIn
	// NoText leaves text written by a person or an agent out of what the
	// caller reads: ThreadInfo.Title, ThreadInfo.Last and ThreadInfo.Name,
	// ThreadDetail.Recent, the text of its requests and a question's
	// choices, a scope's name read from chat (ScopeInfo.Name, when the
	// config gives none), and the text fields of events (section 7.3). It
	// also refuses the reads that show a terminal, whose screen holds such
	// text: Terminal, and HostScreen and HostView of every pane, before any
	// pane is read.
	NoText bool
	// RefuseBusy makes every Send of the caller one with Refuse, whatever
	// Busy it gives; Deliver or Interrupt to a running thread is refused
	// with ErrBusy and logged. A supervisor's caller has it (section 7.1).
	RefuseBusy bool
	// NoAnswer refuses the caller's Send with ErrBusy when the thread waits
	// on a question, instead of taking it as the answer, in the
	// transaction that would take the input. The supervisor's waker has it
	// (section 7.1, #441).
	NoAnswer bool
	// Mark is a line the core puts before the text of every input of the
	// caller's Start and Send, except a Send that answers an open question;
	// the title is made without it. Set by the
	// daemon when it binds the caller, never by the client.
	Mark string
	// ReadAsPerson checks the caller's reads like its input: the person
	// From resolves to must be allowed in a scope to read its threads,
	// events and name, else the read is refused and logged; a list of every scope
	// leaves those scopes out. The chat front end's caller for a /status
	// command has it (#256).
	ReadAsPerson bool
	// Limit, when set, limits the caller to the operations and event kinds
	// it names, within May and Scopes; nil limits nothing. A plugin's
	// caller has one, from its grant (section 7.3, #348).
	Limit *Limit
}

// Limit is what a Caller is limited to.
type Limit struct {
	// Operations, by their names in docs/protocol.md ("Threads", "Close").
	// Any other is refused with ErrForbidden and one log line naming the
	// operation, the scope and the caller. HostKeys and End go with
	// OperateHost.
	Operations []string
	// Events are the kinds it reads through Events, ThreadEvents and
	// Thread; others are left out, and positions stay the instance's own,
	// so a reader resuming after its last position skips none of these.
	// CaughtUp is delivered whatever this says.
	Events []EventKind
}

// A connection to the control socket is the owner's, unless its hello
// carries a token that the daemon issued (wire.Server.Issue, since
// protocol 2.2): then it is bound to the API of the Caller the token was
// issued for. wire.Server.Revoke forgets a token and closes its
// connections. supervisor.Caller(scope, thread) is a supervisor's Caller: From
// "supervisor:<scope>", Scopes [scope], OpRead|OpStart|OpSend|OpAsk, AsksIn thread, RefuseBusy,
// and the Mark of section 7.1. plugin.Grant.Caller() is a plugin's Caller
// (section 7.3): From "plugin:<id>", the grant's Scopes, the classes of May
// its operations need, a Limit to exactly its operations and event kinds
// (empty, never nil, when it names none), and NoText without the
// "conversation" grant. wire.Server.Restore binds a token issued by
// an earlier run of the daemon again, for a client that still runs with it
// (#270). wire.Server.NotOwner, when set, is asked about the process (its
// pid, from SO_PEERCRED or LOCAL_PEERPID) of a connection whose hello has no
// token: a name refuses the connection, logged with that name. The daemon
// sets it to plugin.Runner.PluginOf, which names the plugin whose process
// group the process is in: a guard against a plugin that forgot its token,
// not a boundary (section 7.3, #487). wire.Server.Peered is told of each connection on which the
// daemon may send requests, with the API it is bound to; plugin.BindAs
// marks the API of a plugin's caller, so that plugin.Dispatcher knows the
// plugin's connections, tells it plugin_started on its first one, and
// sends its hook requests on its oldest one still open (#350).

// Hooks runs the handlers plugins have on a hook point (section 7.3):
// plugin.Dispatcher. The core and the chat front end call it at their
// points, and so name no plugin. The handlers of the connected plugins
// whose grant has the point and the scope run by priority, higher first,
// then in config order, each given the text as the one before left it. A
// handler fails when it does not answer within its timeout, when its
// plugin is not connected (decided 2026-11-19) or its connection closes,
// when it answers with an error, not_supported too, and when its answer
// is one the point does not allow; a failure blocks or skips by the
// handler's on_failure, else its point's. Calls are logged without text.
// A point with no handler calls nothing and waits for nothing. Since
// protocol 3.16.
type Hooks interface {
	Run(ctx context.Context, c HookCall) HookResult
}

type HookCall struct {
	Point  string   // before_agent_run, before_prompt_build, message_sending
	Thread ThreadID // 0 at a point before the thread has a number
	Scope  string
	Text   string // the input, or the post; left out of the call for a handler without "conversation"
}

// HookResult: Text as the handlers left it. Stopped when a handler
// stopped it or failed where its policy blocks; By names that plugin, and
// Reason is the plugin's reason (text) or, when Failed, hataori's account
// of the failure.
type HookResult struct {
	Text    string
	Stopped bool
	Failed  bool
	By      string
	Reason  string
}

// Not operations of the control API, for a client inside the daemon (#270):
// core.Core.StartConfined(ctx, caller, r) is Start for a thread whose
// sessions all run confined (agent.Spec.Confine), in an empty directory
// that core.Workspaces.Empty makes instead of a clone; r.Repo must be
// empty. A Start key that started a thread of the other kind is refused
// (core.ErrConfinedKey). core.Core.LiveConfined lists the sessions of such
// threads whose agent may run.

// Fields of ThreadInfo, and so of ThreadDetail, besides its id, number,
// repository, state, who started it and when (section 7.2).
type ThreadInfo struct {
	// ...
	Title      string // the first line of the first prompt; text, so optional since protocol 2.0
	Name       string // the chat thread's own name, of the first link (SetName), following a rename; text; since protocol 3.7
	Agent      string // the agent's registered name
	Model      string // as the agent reports it; as asked for until then
	Effort     string // the same
	Turns      int    // turns started in this thread
	Last       string // the last line of output, at most LastMax bytes; text, so optional
	Background int    // the background of the thread's last turn_ended event
	Context    *Context    // the last context use reported; absent until one is
	Tokens     *TokenCount // absent until the agent reports usage
	Terminal   string      // the terminal its current session's agent runs in, such as /dev/pts/3; absent on pipes and with no process
}

type Context struct{ Used, Size int64 } // tokens
// Tokens are input, output, cache reads and writes together. Today counts
// what each report added in the day it came, in the daemon's local time
// zone, so a turn past midnight counts in both days.
type TokenCount struct{ Turn, Today int64 }

type ThreadDetail struct {
	ThreadInfo
	Requests []Request
	Recent   string  // the last output, its messages apart by a blank line (since 3.12)
	Events   []Event // the last events asked for, newest last
}

// Start, Send, Answer and Stop are refused unless Principal is the owner or
// one of the people of the thread's scope.

type TakeInRequest struct {
	Window string // as HostTerminal.Reported.Window names it
	Scope  string
	Key    string // makes the call safe to repeat
}

type StartRequest struct {
	Agent  string // empty for the scope's agent
	Key    string // safe to repeat: the same key starts nothing twice
	Scope  string // from the config; for chat, the scope's own channel
	Thread string // the first link's conversation in the scope's own channel, of the scope's owner; empty to ask the owner
	Repo   string
	Input  Input
	Model  string
	Effort string
}

// Busy says what Send does when a turn is running.
type Busy int // Refuse, Deliver, Interrupt

// ThreadID is the thread's number, threads.id.
type ThreadID int64

// LinkRef is where a thread is shown: a conversation of one client.
type LinkRef struct {
	Client  string
	Channel string
	Thread  string
}

// Filter gains Link: Threads with a Link returns the thread linked there,
// or none. Input and Answer gain Link, the conversation they came through (since protocol 3.6); the core keeps it only when it is a link of the thread, and the `input` and `answer` events name it with the person, so that every other link shows them as `<person>: <text>`. ThreadInfo gains Scope and Links; Event carries Thread (the
// number) and Scope.

// ScopeOwner is the client that gives a scope its threads. The core asks
// the owner for a new thread's first link, after the Start is allowed; key
// is the Start's key. A chat front end opens a chat thread, from the
// trigger message when key is one, and returns where.
type ScopeOwner interface {
	NewThread(ctx context.Context, scope, key, title string) (LinkRef, error)
}
```

Changing `ThreadID` from a scope and a key to a number changes how every thread and event is encoded, so it is a new major protocol version; the issue that implements it sets the version and the golden files. `Link`, `LinkRef` and the `link` field of `input` and `answer` events come with it.

Sleep, close and remove join the interface with the phase that needs them.

The types carry lowercase JSON names, and the data of every event kind is declared in `control.EventKinds`; the store refuses an event that does not match. A `turn_started` event carries the key of the call that started the turn, so a chat front end can reply to the message that started it. A `subagent_started` event carries the subagent's name, and a `tool_started` event the subagent the tool runs in (`agent.Event.Parent`), so a front end can count a subagent's tool calls. A `file_out` event carries every file of one hand-over (`More`) and its caption (`Caption`). A `request` event carries `multi` when several of its choices may be chosen at once, and an `answer` event the `request` it answers, so a chat front end can take a question's buttons off once it is answered (#257). A `usage` event carries the counts of `agent.Event.Usage`, each the running turn's total so far, and the turn; the core keeps the thread's `Tokens` and `Context` from them in the same transaction. The Claude Code adapter fills the counts and `Model` from the usage of each model message in the transcript, and leaves the context use out: neither its transcript nor its hooks give the context window. How all of this is encoded on the socket, and when the protocol version changes, is in [docs/protocol.md](../protocol.md).

### Control API: additions for the dashboard (proposal)

Section 7.2. `ThreadID` in these operations is the thread's number, as above. Not declared yet: each item lands with the issue that implements it, which also changes [docs/protocol.md](../protocol.md) and its golden files. Every item is a new optional field or a new operation, so each is a minor version, unless the table below says otherwise. Names may still change.

```go
// New operations of API.
type API interface {
	// ...the operations above, and:

	// Workspace: declared above, since protocol 1.4 (#196).

	// Terminal: declared above, since protocol 1.9 (#198).

	// HostTerminals, HostScreen: declared above, since protocol 1.10 (#199),
	// with OpHost.

	// ThreadEvents: declared above, since protocol 3.3 (#336), for the
	// dashboard's Conversation view.

	// SignInLink: declared above, since protocol 1.7 (#200); for a chat
	// caller with From since 1.11 (#252). The control socket's connections
	// have OpSignIn, and so has the chat front end's caller for a person's
	// /dashboard (#259); a dashboard session's Caller has
	// OpRead only, OpHost for the owner when host terminals are on, and
	// OpHostInput for the owner when host input is on.

	// SignInLink's way: declared above, since protocol 3.2 (#359). A chat
	// front end asks for WayWeb when there is a web listener (#259,
	// FrontEnd.SignInWay).

	// OperateHost: declared above, since protocol 3.11 (#362), with
	// OpHostInput, which the owner's connection on the control socket has
	// when host input is on.
}

// Way: declared above, with SignInLink.

// HostOperation, HostKey: declared above, with OperateHost.

// WorkspaceState: declared above, with Workspace.

// TerminalChunk: declared above, with Terminal.

// HostTerminal, TmuxPane: declared above, with HostTerminals.
```

On the socket, `OperateHost` turns its connection into a stream in both directions, after its reply: the client sends batches of keys (`HostKeys`) and may send an end (`End`); the daemon answers each batch and sends one message with the reason (`ended`) when the operation ends. A connection that closes ends the operation. The daemon gives the core the tmux lister as `core.Core.HostInput` when host input is on, with the idle period and its own context's end as `Stopping`, and deletes the buffers an earlier run left before it listens; each instance's buffers have a name of their own.

Remaining usage per agent is `Limits`, declared above (#211), from the sources the research found (#191, [docs/notes/agent-usage-limits.md](../notes/agent-usage-limits.md)). An adapter reports it as an agent event of kind `Limits`; the session machine keeps the newest report per agent in the same transaction and writes no control event, since it belongs to no thread. Claude Code: hataori's `--settings` sets `statusLine` to `hataori statusline`, which hands the input's `rate_limits` to the session host when they change and every 5 minutes, refreshes at least once a minute, and runs and prints the status line the user would have had (the workspace's local and shared settings, then the user's own; only read, never written), so the user's line still shows. Codex: the decoder of `account/rateLimits/read` is in `internal/agent/codex`; the Codex adapter reads it at start and after each turn when it comes (section 8).

| Change | Protocol |
|---|---|
| `Workspace` | minor: new operation |
| `HostTerminals`, `HostScreen`, with `Ops` gaining `OpHost` | minor: new operations; landed in 1.10 |
| `SignInLink`, with `Ops` gaining `OpSignIn` | minor: new operation; landed in 1.7 |
| `SignInLink` for a chat caller, the session belonging to its person | minor: an operation allowed to one more caller, no encoding change; landed in 1.11 |
| `ThreadInfo.terminal` | minor: new optional field; landed in 1.12 |
| `Limits` | minor: new operation; landed in 1.13 |
| `ThreadInfo.title` left out for `Caller.NoText` (#236) | major: a required field becomes optional; 2.0 |
| `AttachTerminal`, with `Ops` gaining `OpAttach` (#253) | minor: new operation; 2.1 |
| `hello.token`, and `principal` and `from` in the `input` and `answer` events (#269) | minor: new optional fields; 2.2 |
| `SignInLink`'s `way` (section 7.2, #359) | minor: new optional argument; 3.2 |
| `ThreadEvents` (#336) | minor: new operation; 3.3 |
| `SetName`, with `Ops` gaining `OpName`, `Scopes` and `ScopeInfo`, and the `named` event (#405) | minor: new operations and event kind; 3.5 |
| `ThreadInfo.Name`, the chat thread's own name (#406) | minor: new optional field; 3.7 |
| `OperateHost`, with `Ops` gaining `OpHostInput`, `HostKeys` and `End` on its stream, and the `ended` error code (section 7.2, #362) | minor: new operation, whose connection becomes a stream both ways; 3.11 |
| `HostView`, a pane's screen with its cursor, for the dashboard's screen of a pane being operated (section 7.2, #363) | minor: new operation; 3.13 |
| `HostTerminal.Reported` and `ReportedSession`, from the hooks of the Claude Code plugin (section 4.8, #397) | minor: new optional field; 3.15 |
| `TakeIn` and `Release`, with `Ops` gaining `OpTakeIn`, and the removal reason `the workspace is not hataori's` (section 4.8, #400) | minor: new operations; 3.17 |
| `WorkspaceState.PullRequest` | minor, and only after its research |

Declared above, in protocol 1.5: `ThreadInfo`'s `agent`, `model`, `effort`, `turns`, `last` and `background`; `Thread`'s argument `events` and `ThreadDetail.events`. In protocol 1.8: `ThreadInfo`'s `context` and `tokens`, and the fields of the `usage` event. In protocol 1.9: `Terminal`, which turns its connection into a stream as `Events` does. In protocol 1.12: `ThreadInfo.terminal`, the terminal device its session host opened, for the tree's filter, the status bar and the details panel (#285); `HostTerminals` still leaves these terminals out.

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

	// OpenThread opens a thread in parent.Channel, from the message
	// parent.Thread names when it is set; a message that has a thread
	// already gets that thread back, and no second one is opened.
	OpenThread(ctx context.Context, parent Conversation, title string) (Conversation, error)
	// OpenDirect returns the private conversation with one person, given
	// by the person's id on the service.
	OpenDirect(ctx context.Context, who string) (Conversation, error)
	Post(ctx context.Context, conv Conversation, key string, m Message) (MessageRef, error)
	Edit(ctx context.Context, ref MessageRef, m Message) error
	Delete(ctx context.Context, ref MessageRef) error
	SetStatus(ctx context.Context, ref MessageRef, s Status) error        // lamp on a message
	SetThread(ctx context.Context, conv Conversation, t ThreadMeta) error // title, archived

	Limits() Limits // message length, choices and files per message, file size, which optional calls work, whether commands work
}

// Factory makes a Driver from its opaque section of the instance's config.
// config is the instance's config directory, the one place a driver may read
// a secret from. Register(name, f) fails when name is taken; Lookup(name)
// finds it.
type Factory func(config *os.Root, settings []byte) (Driver, error)

// Namer is implemented by a Driver that can tell the name the service shows
// people for a conversation, such as "#general" for a Discord channel; ""
// for none. The chat front end asks it for each scope's channel after each
// connect, and records the names with SetName (#405).
type Namer interface {
	Name(ctx context.Context, conv Conversation) (string, error)
}

// Renamer is implemented by a Sink that wants the names of conversations
// as the service shows them: Run calls Renamed when it learns a thread's
// name, and when the name changes. The chat front end records them with
// SetName, and the dashboard shows a thread by its chat thread's name
// (#406).
type Renamer interface {
	Renamed(conv Conversation, name string)
}

// Sink is implemented by the chat front end, not by the core.
type Sink interface {
	Message(Inbound)  // text, files, author, ids
	Choice(Selection) // a button or menu was used: Token, and Tokens for every option chosen
	Command(Command)  // a slash or text command
}

// Reconnected is implemented by a Sink that wants to catch up: Run calls
// Connected after it connects and after each reconnect.
type Reconnected interface {
	Connected()
}

// Commander is implemented by a Sink that handles commands. Run makes the
// commands it returns the service's commands for the bot when it starts,
// and changes nothing on the service when they are already these.
type Commander interface {
	Commands() []CommandSpec
}

type CommandSpec struct {
	Name        string
	Description string
	Args        []ArgSpec // text arguments, required ones first
	Private     bool      // the answer is shown only to the person who used it
}

type ArgSpec struct {
	Name, Description string
	Required          bool
}

// Command is the use of a command. Reply, when the driver sets it, is where
// the answer goes: a Post into Conv with ReplyTo set to Reply answers the
// command in place (privately when its spec says Private). A driver
// acknowledges the command itself, before the sink sees it.
type Command struct {
	Conv   Conversation
	Author Author
	ID     string // safe-to-repeat key of the command
	Name   string
	Args   []Arg  // {Name, Value}, in the order given
	Reply  *MessageRef
}

// Inbound is a received message. Direct says that its conversation is a
// private one with the author; Service, that the service wrote it itself
// (a notice that a thread was renamed, for example).
type Inbound struct {
	Ref     MessageRef
	Author  Author
	Text    string
	Files   []File
	ReplyTo *MessageRef
	Direct  bool
	Service bool
	Time    time.Time
	Cursor  string // pass to Since to get the messages after this one
}

type Message struct {
	Text    string
	Files   []File
	Choices []Choice    // rendered as buttons or a menu; each carries an opaque token
	Multi   bool        // several may be chosen at once (a menu); needs Limits.MultiSelect
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
	// The Codex adapter runs `codex --version` and reads the exit status of `codex login status`.
	Probe(ctx context.Context) (Health, error)

	// Launch returns how to start the agent for spec: a new session,
	// or a resumed one when spec.ResumeID is set.
	// The Codex adapter runs `codex app-server` on pipes, with no files,
	// the fixed environment plus pass_env, and the workspace's trust level
	// given with -c, so that Codex does not record it in the user's
	// configuration; a resumed session gets the same command (the codec
	// sends thread/resume).
	Launch(spec Spec) (Launch, error)

	// NewCodec returns the translator for one running session.
	NewCodec(spec Spec) Codec
}

// ErrNoCodeBack is returned by LoginCode, for every code, when the login
// takes no code back: the owner only enters ShowCode on the link's page.
var ErrNoCodeBack = errors.New("agent: this login takes no code back")

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
	// A flow taking no code back returns ErrNoCodeBack for every code, even empty.
	LoginCode(code string) ([]byte, error)
}

// LoginStep is what a line of the login command means: a link to show
// (with a code to show beside it, if the agent gives one), a request for a
// code, a refusal with its text, or nothing. ShowCode may arrive alone after Link.
type LoginStep struct {
	Link      string
	ShowCode  string
	WantsCode bool
	Refused   string
}

// CheapModel is implemented by an adapter that can name its cheapest
// model: a supervisor's when the config names none (section 7.1).
type CheapModel interface {
	CheapModel() string
}

// Launch is everything the session host needs to start an agent.
type Launch struct {
	Argv     []string
	Env      []string
	Terminal bool              // run on a pseudo-terminal instead of pipes
	Files    map[string][]byte // written into the session's state directory first
	// Confined: the launch confines the session as Spec.Confine asks. The
	// runner never starts a confined spec's launch without it, so an
	// adapter that knows nothing of confinement refuses.
	Confined bool
}

// Source says where a line came from: Output (the agent's output on pipes),
// Hook (a call from the agent's hook command) or File (a file the codec asked
// to follow).
type Source int

// Codec turns what an agent reports into events, and requests into what the
// agent accepts. It does no I/O, so it is tested against captured output.
type Codec interface {
	Open() Actions                          // what to do first, if anything
	// One reported line. src is Output, Hook, File, or Screen: the agent's
	// terminal output, given only to a codec whose Open asked for it, and
	// only until the codec says StopScreen. In each step the runner gives
	// the followed files' new lines, then the host's log, then files just
	// named, so a line can come before a hook call made just before it; a
	// codec rebuilt after a restart or a failed commit gets the committed
	// lines step by step in the same order (the store keeps each step's
	// ranges). The Claude Code codec opens nothing for a call of a turn
	// already interrupted. The Codex codec (codex app-server on pipes, one
	// JSON-RPC message per line) writes initialize, initialized and
	// thread/start or thread/resume from Open; a codec rebuilt after a
	// restart was never opened, so it tells responses apart by their shape
	// and numbers its own requests above every response id it has read. A
	// rebuilt codec knows the open requests and running turn from those lines.
	// Under AllowAll, command and file-change approvals are accepted at once
	// without opening a request. Under AskInChat each becomes a Request;
	// Answer writes the JSON-RPC response, accepting or declining it.
	// item/tool/requestUserInput becomes a Request answered with its response.
	// An agent message with questions and delivery: "async" becomes a Request
	// answered by a user message: turn/steer while its turn runs, turn/start
	// afterward. RequestWithdrawn reports an unanswered server request resolved
	// by serverRequest/resolved, or a server request still open at its thread's
	// turn end. An async question is withdrawn by a newer request, a user
	// message in its thread, or a turn its answer did not start. Any other
	// server request gets a JSON-RPC error and an Unknown event. The schema's
	// item/permissions/requestApproval, execCommandApproval and
	// applyPatchApproval have no captures yet and get the same error.
	Decode(src Source, line []byte) Actions
	// A user message; TurnStarted returns its ID. Codex steers input into a
	// running turn and reports InputTaken; Claude Code holds it until the turn
	// ends. The Codex codec ties a turn to its input by the turn/start it
	// wrote, or, for an input written before a restart, by the text of the
	// turn's user message; a steer refused because the turn ended becomes a
	// turn/start.
	Input(in Input) (Actions, error)
	Answer(a Answer) (Actions, error)       // to an approval or a question
	// Codex interrupts with turn/interrupt, then cleans background terminals
	// at turn end. This ends every background terminal in the thread, including
	// commands moved to the background in earlier turns.
	Interrupt() Actions
	// Tick gives the codec the time for its deadlines (input not reported
	// as delivered); the codec has no clock of its own.
	Tick(now time.Time) Actions
}

// Actions is what the caller of a Codec does next.
type Actions struct {
	Events []Event     // for the core
	Keys   []Part      // an agent in a terminal: pastes and named keys, in order, for its holder (Terminal holder, below)
	Write  [][]byte    // an agent on pipes: bytes for its standard input, in order
	Reply  []HookReply // to hook calls that are waiting for an answer
	Follow []string    // files to start following, such as a transcript
	Inputs []string    // the Input.IDs of the messages Keys or Write carries
	// WatchScreen, from Open only: give Decode the terminal (Source Screen)
	// until StopScreen, for what no hook reports (a start-up dialog, whether
	// the agent reads keys yet). A codec rebuilt after a restart never
	// watches.
	WatchScreen bool
	// StopScreen, from any call, ends that watch: the codec needs the screen
	// no more. SessionReady does not end it; a codec that watches ends it.
	StopScreen bool
}

// Input is a user message. ID is opaque to the codec; the TurnStarted of
// the turn that reports the message carries it back, and so does a
// TurnEnded "not delivered" when no report came. Written: the message was
// written before a restart; the codec records it and writes nothing.
type Input struct {
	ID      string
	Text    string
	Written bool
}

type Spec struct {
	Dir          string // workspace
	StateDir     string // settings, attachments, extra instructions; outside the workspace
	ResumeID     string
	Model        string
	Effort       string
	Instructions string // appended to the agent's own
	Approvals    Policy // allow everything, or ask in chat
	// Confine, when set (section 7.1): no shell, no subagent and no other
	// tool that runs a command, reaches the network or wakes the agent
	// later; file tools only in Dir; no MCP servers but Confine.MCP. nil for
	// an ordinary session, whose launch is unchanged.
	Confine *Confinement
	// TrustFiles: the files a start-up dialog may say the workspace loads
	// (a project settings file) that an ordinary session is let past the
	// dialog for; its scope's trust_files at each launch (section 3.5, #423).
	TrustFiles []string
	// TakenIn: a session a person started in their own window, taken in
	// (section 4.8, #400). It is past its start and runs as ResumeID with
	// Model; its codec starts ready, Open reports SessionReady and watches
	// no screen, and nothing launches it. A resume clears it.
	TakenIn bool
}

// Confinement is what a confined session may use besides its file tools.
// MCP may carry credentials for one launch; the runner never stores them.
type Confinement struct{ MCP []MCPServer }

// MCPServer is an MCP server the agent starts on stdio.
type MCPServer struct {
	Name string   // the name its tools are listed under
	Argv []string // the command and its arguments
	Env  []string // NAME=value, added for it
}

// Event is one thing the agent did. Kind is one of SessionReady, TurnStarted,
// Text, ToolStarted, ToolEnded, SubagentStarted, SubagentEnded, Request,
// RequestWithdrawn, FileOut, Usage, TurnEnded, SessionEnded, Limits, InputTaken, Unknown.
type Event struct {
	Seq  uint64
	Kind Kind
	// Plus the fields each kind needs: the text and whether it is final,
	// who started a turn and the Input.ID of the message it reports, the
	// request id and its questions (RequestInfo: ID, Approval, Text,
	// Choices, and Multi when several choices may be chosen at once, since
	// #257), the subagent a tool runs in (Parent),
	// the files handed over with their caption (File, More, Caption),
	// the end status and reason, the background tasks still running,
	// the tokens of the turn so far and the context use (Usage), the
	// account's remaining usage with when the agent gave it and how often
	// the adapter reports it (Limits: LimitReport{At, Every, Windows}).
	// SessionEnded has a reason: the agent's own, CannotResume ("cannot
	// resume") when a resumed session's conversation is gone, or
	// WrongSession when a resume reported another session id, or
	// BlockedAtStart when the session may not answer a start-up dialog,
	// with Text saying why for a person (#423). SessionReady
	// in a live session is a new session id, as after a person cleared the
	// conversation at the terminal.
}

// Factory makes an Adapter from its section of the instance's config,
// which is opaque to everything but the adapter, except for the keys
// "model" and "effort": hataori reads those as the defaults of the
// agent's threads and takes them out of settings.
// Register(name, f) fails when name is taken; Lookup(name) finds it.
type Factory func(settings []byte) (Adapter, error)
```

### Terminal holder

Section 4.3. Package `internal/holder`; hataori's own session host (`holder.Host`) is the only holder so far (#395). A holder sits below the core, next to the session host. For an agent in a terminal, the codec's `Actions` carry `Keys []agent.Part`; an agent on pipes keeps `Write`, and its holder is always hataori's own host. The runner reaches starting, input, the screen, stopping and whether an agent is alive only through the holder; it still talks to the session host directly for the relay (hook calls that wait, and their answers), for attach (and the count of attached terminals the idle check reads) and for the terminal's device name.

```go
// Package agent: input for an agent in a terminal.

// Part is a paste when Key is empty, else one named key.
type Part struct {
	Paste string // delivered as one bracketed paste, its bytes unchanged
	Key   Key
}

// Key is a name from a fixed table. Each holder maps every name or
// refuses the whole input; it never guesses.
type Key string

const (
	Enter  Key = "Enter"
	Escape Key = "Escape"
	Up     Key = "Up"
	Down   Key = "Down"
)
```

```go
// Package holder: what holds an agent's terminal. The relay of hook calls
// is not part of it: hataori's session host keeps that job for every holder.

type Holder interface {
	// Caps says what this holder offers: only a holder with CanStart
	// starts agents, and only one with RawScreen holds a session whose
	// screen is watched (Actions.WatchScreen). With one holder, the runner
	// does not ask yet.
	Caps() Caps

	// Start starts the agent with exactly spec's arguments, environment
	// and size, in a process group of its own, once the relay for
	// stateDir listens.
	Start(ctx context.Context, stateDir string, spec Spec) (Window, error)

	// Input hands the parts to the terminal in order and returns once
	// all of them were handed over. It refuses the whole input when it
	// cannot encode one part. Text never goes on a command line.
	Input(ctx context.Context, w Window, parts []agent.Part) error

	// Screen streams the terminal from a position: raw output when
	// Caps().RawScreen, else one snapshot, marked Reset, each time it
	// changes.
	Screen(ctx context.Context, w Window, run string, after uint64) (<-chan control.TerminalChunk, error)

	// Stop ends the agent's process group, interrupt first. A holder
	// that did not start the agent returns ErrNotOwned and does nothing.
	Stop(ctx context.Context, w Window, interrupt, terminate time.Duration) (StopResult, error)

	// Alive says whether the agent runs. Unknown is never Gone.
	Alive(ctx context.Context, w Window) (Liveness, error)
}

type Caps struct {
	CanStart  bool // hataori's own host; tmux for windows it starts
	RawScreen bool // raw output from the first byte, with positions
}

type Spec struct {
	Argv       []string
	Env        []string // exactly the agent's environment
	Dir        string
	Terminal   bool // else pipes, which only hataori's own host offers
	Cols, Rows uint16
	// Again: a daemon that died had begun this start and may have started
	// the agent. It holds the window recorded before that start began
	// (Process 0 when none was); a window that start made is taken instead
	// of making another.
	Again *Window
}

// Window names one window of one holder. Ref is opaque to the core. For
// hataori's own host it is the session's state directory, and each run of a
// host there is told apart by its run id; for tmux it will hold the
// server's socket path and process id and the pane id.
type Window struct {
	Holder  string // the holder's name
	Ref     string
	Owned   bool // hataori started it; false for a window taken in
	Process int  // the process that holds it, recorded as the session's host_pid; 0 when not known
}

type StopResult = host.StopResult

type Liveness struct {
	State  State        // Unknown, Running, Exited or Gone
	Status *host.Status // how it ended, when the holder recorded it
}
```

hataori's own host writes a paste as `ESC [200~`, the text, `ESC [201~`, and the keys from a fixed table: Enter `\r`, Escape `ESC`, Up `ESC [A`, Down `ESC [B`, each part in one write, the bytes the Claude Code codec wrote itself before it returned parts (`holder.Encode`, tested). Its `Alive` reads the state directory and never takes the host's lock: Exited once the status is recorded and the socket gone, Gone when the host's process no longer exists and recorded no end, Unknown before the host recorded its process id, else Running. Beyond `Holder` it offers what only it does so far, and a later holder that needs one of them adds it to the interface then: `Write` (input on pipes), `Since` (the raw screen by position, for a codec watching the start), `Tail` (the end of the raw log, for a start that timed out), and `WaitReleased` and `Reset` (a state directory whose host ended, made ready for the next).

The holder for a tmux window a person started is `internal/holder/tmux` (#399). Its `Window` is built from a hook's `TMUX` and `TMUX_PANE`, with the Claude Code process that reported as `Process`; its `Ref` holds the server's socket path and process id and the pane id. `Caps` is empty, `Start` returns `ErrCannotStart` and `Stop` `ErrNotOwned`. `Alive` is Gone for a pane `list-panes` no longer lists, for a server whose process id is not the reference's, and when no server answers on the socket and the reference's server process is gone (a socket file removed under a running server is Unknown), Running while that Claude Code process leads the foreground process group of the pane's terminal, Exited (with no status) once the process is gone and the pane stays, and Unknown otherwise. Beyond `Holder` it has `CheckTakeIn`, the check before a take-in (section 4.8). `Screen` sends a snapshot from `capture-pane` each time it changes.

### Control API: taking in a window

Section 4.8. `HostTerminal.Reported` and `ReportedSession` are declared above, since protocol 3.15 (#397). So are `TakeIn`, `Release` and `TakeInRequest`, since protocol 3.17 (#400).

## Appendix E: acceptance criteria after phase 1

Every phase here also has this criterion: the change touches no core package, except to add an operation to the control API or to register an adapter; the supervisor phase may also let the control socket bind a connection to a limited `Caller` (which plugins use too, section 7.3) and let a session be started confined (no shell, only the listed tools, section 7.1); the plugin phase may also limit a `Caller` to the operations and event kinds a plugin was granted, and add hook points (section 7.3).

### Codex

Choosing the agent per scope (`agent`) or order (`agent:`), with `model` and `effort` per adapter section, counts as registering an adapter under the phase criterion (#318).

- [ ] A channel or thread can be set to Codex; start, continue, sleep and wake work as in phase 1.
- [ ] Approvals and questions from Codex appear in the thread and can be answered. Until the buttons of #248, chat allows approvals automatically, so for approvals this is read as: answered in the codec and the end-to-end test (#321).
- [ ] After an interrupt, no command started by that turn is still running.
- [ ] A turn of 30 minutes completes.
- [ ] Captures of real `app-server` output are in `testdata/`, and the codec passes on them.

### People in chat, and cleanup

- [ ] Slash commands start a thread, stop a turn and show status. A message, a command and a button press from a user who is not allowed each do nothing and are logged.
- [ ] A question appears with buttons; pressing one delivers the answer, also when the daemon was restarted in between.
- [ ] The sender's message carries a lamp for working, done and error, and the sender is pinged once when the turn ends.
- [ ] `hataori attach` opens the conversation in the agent's terminal UI, and a turn typed there is known to the next turn from chat.
- [ ] Removal refuses, with the reason, for each of: modified file, untracked file, ignored file not listed as disposable, unpushed commit, stash, commit only in the reflog, submodule with an unpushed commit, submodule with no checkout whose commit is only under `.git/modules`, nested repository, remote that is another workspace or lies under a hataori root, process using the directory, git error, timeout. It removes a clean, pushed workspace. Each test fails when its check is removed.
- [ ] The owner asks for a dashboard sign-in link in chat and gets it in a direct message only; the session belongs to the owner's chat identity. Anyone else who asks gets no link (section 10). The link is found in no channel, database or log file.

### Dashboard, read-only

- [ ] Without a signed-in session every route but the sign-in routes and the page's own files, including the event stream and the terminal streams, answers 401. A request with an unknown `Host` or a foreign `Origin` is refused. With a listening address other than loopback and neither TLS nor a named reverse proxy, the dashboard does not start.
- [ ] Each thread in the user's scopes is in the tree and in the "All threads" table, and its state and last output follow the events within 2 seconds. A thread outside those scopes is never sent to the browser.
- [ ] After a daemon restart the open page catches up with no gap and no repeat (the end-to-end test with a fake agent).
- [ ] Every request that is not a read is refused, except on the routes of passkeys and the operate connection (section 7.2), and no byte from the page reaches a thread's terminal.
- [ ] Terminals hataori does not own are listed only for the owner and only when the config turns them on, on Linux and on macOS, and no key is sent to them unless the owner operates a tmux pane (below).
- [ ] `go build` works without Node, and CI fails when the committed build of the page differs from a fresh one.

### Dashboard: ways in, and typing into host tmux panes

- [ ] On the loopback listener, after opening a link from `hataori signin` through a forwarded port with another number, the page shows the thread list and receives the event stream. A request whose `Host` is not a loopback name, or whose `Origin` is not exactly `http://` and its `Host`, is refused. A link or a session made on one listener is refused on the other.
- [ ] No response of the dashboard sets a cookie, and no request of the page carries the session in its address. A service on another loopback port, opened in the same browser after signing in, receives no credential of the dashboard (browser test). Two instances forwarded to two ports of one machine each keep their own session.
- [ ] `hataori connect user@host` on Linux and on macOS opens a signed-in page, and the next time `hataori connect` alone does. After the SSH connection is cut, it reconnects by itself and the open page goes on with no gap and no repeat. hataori writes no SSH key and puts the link in no file or log.
- [ ] On the web listener a passkey can be registered right after opening a fresh link, or after a check of an existing passkey, and then signs its person in. Registering with only a session, however new, is refused, and so is registering on the loopback listener. A removed passkey no longer signs in.
- [ ] With `host_input` off, no route, operation or control of the page sends a key to any pane. With it on, only the owner can operate, only a tmux pane hataori does not own, and a pane running `hataori attach` is refused.
- [ ] On the web listener, operating starts only after a passkey check, or within five minutes of a sign-in for a person with no passkey; on the loopback listener it starts at once.
- [ ] An operation ends after the idle period without a key and when the tab is closed, and the tab is read-only again.
- [ ] Each start is one log line with the person, the time and the pane, and each end one with the same and the reason. With a program in the pane that keeps nothing, a marker string typed during an operation is found afterwards in no log line, no database, no file under the instance root, no tmux buffer and no process's command line.
- [ ] A program in application cursor mode gets `ESC O A` for Up from the page, and the pane's window size and its session's attached count are the same before, during and after an operation.
- [ ] Idle shell panes are hidden by default, and the switch that shows asleep threads shows them.

### Supervisor

- [ ] With every thread in its scope idle for an hour, the supervisor's session makes no model request.
- [ ] One turn end in its scope starts one supervisor turn; ten within the quiet period start one.
- [ ] Sending to a busy thread is refused. Interrupting happens only after a person approved that call.
- [ ] Attempts by the supervisor's session to write outside its directory, to run a command, and to remove a workspace each fail (tested).
- [ ] When the budget is used up no supervisor turn starts, and a person is told once. The loop guard stops a two-thread ping-pong at the set count.

### Slack

- [ ] Phase 1's criteria for start, continue, questions, restart and catch-up pass on Slack.
- [ ] Each Socket Mode envelope, including a button press, is acknowledged with its `envelope_id` within 3 seconds while the work continues. (3 seconds is Slack's deadline for acknowledging an interaction over HTTP; that Socket Mode has the same deadline is **(unverified)**.)

### Microsoft Teams

- [ ] Phase 1's criteria for start, continue, questions, restart and catch-up pass on Teams, with every order that phase 1 sends through a webhook sent instead through the control API (`hataori spawn`, `hataori send`) and by a person in the channel. Teams never delivers posts by other bots to hataori, so none of these criteria depends on one; posts through a webhook are assumed to be the same **(unverified)**.
- [ ] An activity with an invalid or missing token is rejected.

### Plugins

- [ ] An example plugin written in a language other than Go is in the repository, and the tests below use it.
- [ ] The plugin connects, reads events from a position and calls `Threads`. A call to an operation its grant lacks is refused and logged.
- [ ] A plugin is not started when the hash of its executable differs from the config, and it is refused when its protocol major version differs. Each log line names both values.
- [ ] A plugin killed during a turn is started again, the turn finishes, and the plugin reads each event it missed exactly once.
- [ ] A `message_sending` handler rewrites one post and cancels another. A `before_agent_run` handler stops an input before it reaches the agent, and the thread names the plugin and the reason.
- [ ] Two handlers on one point run by priority, then in config order. A handler that times out gets the point's default failure policy, and a per-plugin override in the config replaces it (each tested).
- [ ] Without the `conversation` grant, nothing sent to the plugin contains message text (tested with a marker string in the input, the agent's text and a post).

### Terminal holders, and sessions people started

Not scheduled (section 8). Sections 4.3 and 4.8.

*The holder operations around today's host:*

- [ ] The runner reaches an agent's terminal only through the holder operations. Outside hataori's own holder, no package calls the session host's functions that start, write to, read, stop or check a command.
- [ ] For a paste, Enter, Escape, Up and Down, hataori's own host writes the same bytes as before the change (tested against the bytes today's codec writes).
- [ ] Every test of phase 1 passes unchanged, the end-to-end test included.

*The plugin, and taking in a tmux window:*

- [ ] Installed with the two commands that `hataori plugin` prints, in a throwaway `CLAUDE_CONFIG_DIR`, the plugin's hooks report a session started in a tmux pane. `hataori plugin` itself changes no file in any Claude Code configuration directory.
- [ ] In a session hataori started, `hataori hook --window` exits 0, prints nothing and connects to nothing.
- [ ] In a window that was not taken in, every hook, `PermissionRequest` and `PreToolUse` for `AskUserQuestion` included, exits 0 within one second and prints nothing, while the daemon runs, while it is stopped, and while it accepts the connection and never answers.
- [ ] With a marker string in a prompt, a reply and a tool input of a session that was not taken in, the marker is found in no report the daemon received. No event, database row, chat post, supervisor wake or plugin call comes from that session.
- [ ] `hataori take` refuses a window whose foreground process is not the Claude Code process that reported, and a session that is not interactive, and then changes nothing.
- [ ] After `hataori take`, a message in the scope's chat thread reaches the window's session and its final answer is posted once; a prompt typed in the window appears in chat marked as typed at the terminal; a stop interrupts the turn and leaves the window's process running.
- [ ] A prompt of several lines with a leading `-`, Japanese, quotes and backticks arrives unchanged through the tmux holder (checked against the real CLI, recorded under `testdata/`).
- [ ] After `hataori release`, the window's hooks only list it, and the window is as it was.
- [ ] When the window's pane is killed, the thread goes asleep with reason `window closed`, and the next message resumes the same conversation under hataori's own host, in the same directory.
- [ ] `hataori remove` of a thread that was taken in leaves its directory as it was, and the removal pass skips it (tested with an untracked file and an unpushed commit in it).

*hataori starting sessions in tmux:*

- [ ] With the config option set, a thread started from chat runs in a window of a tmux session of hataori's own name. Phase 1's criteria for start, continue, questions, restart, sleep and stop pass with it, including a new workspace's folder-trust dialog.
- [ ] The agent's environment in that window is exactly the one hataori's own host gives it (test that compares the two).
- [ ] The agent's exit status is recorded when it exits. When the tmux server is killed the thread goes asleep with reason `interrupted`, and a pane of a new server with the same id is never taken for the old one.

*cmux and iTerm2, on macOS:*

- [ ] Every claim about the holder marked **(unverified)** in section 4.3 was run on macOS and is listed in Appendix B, or the holder does not ship.
- [ ] The criteria for taking in a tmux window pass with a cmux window, and a permission request in it is answered by exactly one program.
- [ ] The same criteria pass with an iTerm2 session, and a prompt of several lines arrives as one paste.
