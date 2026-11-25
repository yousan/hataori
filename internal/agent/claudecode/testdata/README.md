# Claude Code captures

Real output of the interactive `claude` CLI, for testing the adapter's codec. These files are not a contract: when the CLI changes, add a capture of the new version next to this one.

- `2.1.322/`: Claude Code 2.1.322, captured on 2026-11-12 (also in `2.1.322/VERSION`), on Linux, with the model `haiku` (it reports itself as `claude-haiku-5-5`).
- `2.1.326/`: Claude Code 2.1.326, captured on 2026-11-17 (also in `2.1.326/VERSION`), on Linux, with the model `haiku`: `subagent_tools`, taken for #142, and `trust_answered`, taken for #56 (see below).
- `2.1.327/`: Claude Code 2.1.327, on 2026-11-18 (also in `2.1.327/VERSION`), on Linux: `first_enter_lost`, a real start of a session that hataori ran, taken for #178, `usage`, taken by `capture.py` for #195, and the `confine_*` scenarios, taken by `confine.py` for #268 (see below).
- `2.1.328/`: Claude Code 2.1.328, on 2026-11-20 (also in `2.1.328/VERSION`), on Linux: `paste_tabs`, taken for #393, and the `trust_files_*` scenarios, taken for #423 (see below).
- `2.1.329/`: Claude Code 2.1.329, on 2026-11-21 (also in `2.1.329/VERSION`), on Linux: `trust_hooks_*` and `trust_permissions`, taken for #592 (see below).
- `2.1.330/`: Claude Code 2.1.330, on 2026-11-23 (also in `2.1.330/VERSION`), on Linux: `usage`, taken by `capture.py` for #675, with the transcript's line count at each hook call (see below).
- `2.1.331/`: Claude Code 2.1.331, on 2026-11-25 (also in `2.1.331/VERSION`), on Linux: `held_notification`, taken by `capture.py` for #593, `failure_background`, `interrupt_background`, `failure_resumed` and `failure_monitor`, taken by it for #801 (see "Turns that end without the list of background tasks"), `trust_notice_*`, taken by `TestLiveTrustFiles` for #707 (see "The folder-trust dialog below a notice"), and `trust_permissions_*`, taken by it for #596 (see "The folder-trust dialog with a permission warning").
- `2.1.332/`: Claude Code 2.1.332, on 2026-11-25 (also in `2.1.332/VERSION`), on Linux: `trust_notice_off_*`, taken by `TestLiveTrustFiles` for #728 (see "The folder-trust dialog below a notice"), `trust_files_env`, taken by it for #730 (see "The folder-trust dialog that names files"), and `trust_warnings_*`, taken by it for #752 (see "The folder-trust dialog with several warnings").
- `capture.py` took them (`confine.py`, the `confine_*` ones); `scrub.py` made them committable. `captures_test.go` in the package fails when a capture holds a home directory path, an e-mail address, an unscrubbed UUID or something shaped like a token.

## How they were taken

```sh
python3 capture.py RAW_DIR            # every scenario; or name some: capture.py RAW_DIR plain tools
python3 scrub.py RAW_DIR 2.1.322
```

`capture.py` starts `claude` on a pseudo-terminal of 50×160, one session per scenario, each in a new empty directory under the temporary directory. The command line is:

```
claude --settings <file> --setting-sources project,local --model haiku --permission-mode default --session-id <uuid>
```

and its environment holds only `PATH`, `HOME`, `USER`, `LANG`, `SHELL`, `TERM=xterm-256color`, plus:

- `CLAUDE_CODE_DISABLE_CLAUDE_MDS=1`, `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1` and `ENABLE_CLAUDEAI_MCP_SERVERS=false`, so that the user's own instructions, memory and connectors stay out of the transcripts;
- `BROWSER=true`, so that nothing opens a browser.

`--setting-sources project,local` keeps the user's settings (their hooks, plugins, permissions) out. The settings file passed with `--settings` registers one command for 14 hook events. The command stores each payload byte for byte, one file per call, and, since #675, next to it how many complete lines the session's transcript held when it was called (`NN-<event>.lines`; `-1` before the transcript exists). For `PermissionRequest` it prints `{"behavior": "allow"}`. For `PreToolUse` of `AskUserQuestion` it prints `permissionDecision: "allow"` with `updatedInput.answers` set to the first option of each question. Nothing is written to the working directory or to the user's settings.

Input is typed one character at a time and then Enter, or, for the paste scenario, sent as one bracketed paste and then Enter. Each scenario ends with `/exit`.

`scrub.py` reads the machine's and account's values at run time (home directory, user name, host name, e-mail addresses) and replaces them, consistently across all files:

- paths become `/home/user` and `/tmp/capture-work`;
- UUIDs (session ids, prompt ids, message uuids) become `00000000-0000-4000-8000-NNNNNNNNNNNN`, numbered in order of first appearance, so a session id still equals its transcript's file name;
- e-mail addresses become `userN@example.com`;
- tokens, signatures and OAuth link parameters become `REDACTED`.

In transcripts, lines that carry the session's context rather than the conversation keep their shape and keys, with every string replaced by `REDACTED`: `attachment` lines (except their `attachment.type`), `api-request`, `api-request-blob` and `api-request-shape`. Hook payloads are renamed `NN-<event>.json` in call order.

## What each scenario holds

| Scenario | Prompt | Hooks, in order | Transcript |
|---|---|---|---|
| `plain` | typed: reply with one word | `SessionStart` (`source: startup`), `UserPromptSubmit` (typed), `Stop`, `SessionEnd` | a plain turn |
| `resume` | `claude --resume <plain's id>`, one typed turn | `SessionStart` (`source: resume`), `UserPromptSubmit`, `Stop`, `SessionEnd` | the resumed session (the same file as `plain`, continued) |
| `tools` | write a file with the Write tool | `PreToolUse`, `PermissionRequest` (answered allow), `PostToolUse`, `Stop` | a turn with tools |
| `question` | ask with `AskUserQuestion`, two options | `PreToolUse` for `AskUserQuestion` (answered by the hook), `PostToolUse` with the answers, `Stop` with the chosen option | |
| `background` | start `sleep 8` in the background, do not wait | `Stop` with one task in `background_tasks` (`running`), then `UserPromptSubmit` with a `<task-notification>` prompt, then `Stop` with an empty list | |
| `subagent` | start one general-purpose subagent | `PreToolUse`/`PostToolUse` of the Agent tool, `SubagentStart`, `SubagentStop`, `Stop`, then a `<task-notification>` `UserPromptSubmit` | a turn with a subagent, and the subagent's own transcript (`subagent-agent-<id>.jsonl`) |
| `held_notification` (2.1.331) | start `sleep 8` in the background; 3 s after the turn's `Stop`, one bracketed paste without Enter; Enter 1.5 s after the next turn's `Stop` | `Stop` with one task running, a `SubagentStop` of the CLI's own, `UserPromptSubmit` with a `<task-notification>` prompt (the turn that starts while the paste is in the input box; it runs an 8 s command), its `Stop`, then one `UserPromptSubmit` with the pasted text, under a prompt id of its own, and its `Stop` | the paste is the next user message after the agent's own turn |
| `paste` | one bracketed paste of 4,463 characters in 102 lines (leading `-`, trailing `;`, Japanese, backticks, quotes, `$HOME`) | `UserPromptSubmit` with the text inside a `<pasted_content>` tag | |
| `interrupt` | run a 45-second foreground command; Escape 3 s after its `PreToolUse` | `PreToolUse`, `PermissionRequest`, then nothing until `SessionEnd`: no `PostToolUse`, no `Stop` | an interrupted turn, ending with the `[Request interrupted by user for tool use]` line |
| `compact` | one typed turn, then `/compact` | `PreCompact` (`trigger: manual`), `SubagentStop` (the summarising agent, empty `agent_type`), `SessionStart` (`source: compact`), `PostCompact` | a compaction |
| `failure` | one typed turn with `--model claude-does-not-exist-0` | `StopFailure` with `error: model_not_found`, without spending a request | |
| `trust` | none: a new `git init` directory, never opened before | none in 12 s | `terminal.raw`: the raw bytes of the folder-trust dialog, cursor on "No, exit". It was not answered; the process was killed. |
| `login` | `claude auth login` on pipes, with `CLAUDE_CONFIG_DIR` set to a new empty directory | | `status-before.json` (`loggedIn: false`), `stdout-before-code.txt` (up to `Paste code here if prompted >`), `stdout.txt` and `stderr.txt` after the wrong code `not-a-valid-code` (`Invalid code. ...` on stderr; the command kept waiting and was killed) |

Notes:

- The model ran a bare `sleep` in the background by itself, and the CLI refused a standalone foreground `sleep`. The interrupt scenario therefore waits with `python3 -c 'import time; time.sleep(45)'`.
- The real login was never touched: the login scenario only ever sees the throwaway directory, which is deleted afterwards.

## The login command on a terminal (taken for #73, same day and version)

`login_terminal/terminal.raw`: `claude auth login` on a terminal of 50×160 with a new, empty `CLAUDE_CONFIG_DIR`, by `capture.py RAW_DIR login_terminal`. The link is printed inside a terminal hyperlink (`ESC]8;;<link> BEL`, the link again in colour, `ESC]8;; BEL`). After `Paste code here if prompted > ` the wrong code `not-a-valid-code` was typed, and the CLI answered `Invalid code. ...` on the same line. The process was killed and the throwaway directory deleted.

## Missing

- **A successful login.** It needs a person to sign in with a real code. Add `login/` captures of the output and exit status after a correct code when someone next logs in. Until then the adapter's success case rests on the command's exit status and `claude auth status` alone.

## Resume, clear and sleep (taken for #61, same day and version)

Taken by `capture.py RAW_DIR resume_missing clear sleep` with the setup above.

| Scenario | What was done | Hooks, in order |
|---|---|---|
| `resume_missing` | `claude --resume <a new random id>` in a new directory; no request is sent | only `SessionEnd` (`reason: other`), with the id asked for, and no `SessionStart`. The terminal (`terminal.raw`, kept) says `No conversation found with session ID: ...`. |
| `clear` | one typed turn, `/clear`, one more typed turn | `SessionEnd` (`reason: clear`), then `SessionStart` (`source: clear`) with a **new** session id and transcript (`transcript-2.jsonl`), and no model |
| `sleep` | one typed turn asking to remember a word, then the process group stopped as the session host stops a session: SIGINT, SIGTERM after 1 s, SIGKILL after 2 s more | `SessionEnd` (`reason: other`) after the SIGINT |
| `sleep_resumed` | `claude --resume <sleep's id>` in the same directory, asking for the word | `SessionStart` (`source: resume`, the same id, no model); the `Stop` answer is the word: the conversation survived the stop |



`2.1.322/probe/` holds the two answers `Probe` reads, from the real configuration: `version.txt` (`claude --version`) and `auth-status-logged-in.json` (`claude auth status` while logged in, scrubbed with `scrub.py`'s `Scrubber`; `orgName` replaced by hand). Both commands only read. The logged-out answer is `login/status-before.json`.

## Questions and waiting hooks (taken for #60, same day and version)

Taken with the same setup as above, but a check script in place of the recorder's answers. Only the hooks and the transcripts are kept, scrubbed by `scrub.py`.

| Scenario | What the hook did | What the CLI did |
|---|---|---|
| `question-free` | answered `AskUserQuestion` with `answers` holding a free text that is none of the options | accepted it; `PostToolUse` and the final answer carry the text |
| `question-multi` | answered a `multiSelect` question with the labels joined by `, ` | accepted it; the final answer repeats `red, blue, green` |
| `question-timeout-q` | the `PreToolUse` hook for `AskUserQuestion` slept past its 5-second `timeout` | ignored the hook, asked `PermissionRequest` (also left to time out), sent `Notification`, and showed its own question dialog in the terminal |
| `question-timeout-p` | the `PermissionRequest` hook slept past its 5-second `timeout` | sent `Notification` and showed its own permission dialog in the terminal |

In both timeout scenarios the hooks after `Notification` came from the script's `/exit` keystrokes landing in the dialog, not from an answer.

## A subagent's tools (2.1.326, taken for #142)

`2.1.326/subagent_tools/`, by `capture.py RAW_DIR subagent_tools` with the setup above: the same command line, environment and recorder hook, one tiny turn. The prompt asks for one general-purpose subagent, with the model `haiku`, that runs `echo one` and `echo two` with the Bash tool as two tool calls.

| Hooks, in order | What they show |
|---|---|
| `PreToolUse` (Agent), `SubagentStart`, `PostToolUse` (Agent), `Stop` | The CLI ran the subagent in the background: the Agent tool ended and the turn stopped before the subagent's tools ran. The main agent's tool calls carry no `agent_id`. |
| `PreToolUse` (Bash) ×2, `PostToolUse` (Bash) ×2, `SubagentStop` | The subagent's two Bash calls, started together. Each carries `agent_id` and `agent_type` (`general-purpose`), the same as `SubagentStart` and `SubagentStop`. No `PermissionRequest`: `echo` ran without one. |

There is no `SessionEnd`. `/exit` opened a dialog ("Background work is running", with the choices "Exit and stop tasks", "Move to background and exit" and "Stay"), because the subagent was still running when `/exit` was typed. Nobody answered it, so `capture.py` killed the process (its own child, by PID) after the usual waits. The terminal output is not kept.

`scrub.py` now also replaces an interpreter path under the home directory (a virtual environment) in the hook command with `/usr/bin/python3`.

The 2.1.322 files were rescrubbed with that rule alone, in place, on 2026-11-17 (`scrub.py --again 2.1.322`): only the hook command's interpreter changed. `captures_test.go` fails on such a path.

## The folder-trust dialog answered (2.1.326, taken for #56)

`2.1.326/trust_answered/`, taken by `TestLiveFolderTrust` in the package. It runs only with `HATAORI_REAL_CLAUDE=1 HATAORI_CAPTURE_DIR=<raw dir>`, and its output was scrubbed with `scrub.py`. The privacy setup is the same as above: only `--settings` and the project's own settings, no CLAUDE.md, no memory, no connectors, the user's plugins switched off, model `haiku`. Hooks were registered for `SessionStart`, `UserPromptSubmit`, `Stop` and `SessionEnd`.

The workspace was a clone of a new local repository, in a new temporary directory, never opened on the machine. The terminal was 24×80, the session host's default size. The codec itself answered the dialog. No prompt was sent, so no turn ran. After `SessionStart` the test typed `/exit`.

| File | What it is |
|---|---|
| `screen-01.raw` … `screen-08.raw` | The terminal output, chunk by chunk as the codec was given it, up to the chunk that made it confirm. `screen-04` is the dialog. `screen-05` to `-07` are the CLI's questions to the terminal (`ESC[>0q`, `ESC[?u`, an OSC 7501 query, `ESC[c`), which no terminal answered. `screen-08` redraws the choices with the cursor on "Yes, I trust this folder". |
| `keys.txt` | The keys and when they were sent: Down once the screen had settled after `screen-07`, then Enter once it had settled after `screen-08`. |
| `hooks/` | `SessionStart` (`source: startup`) right after the Enter, and `SessionEnd` (`prompt_input_exit`) after `/exit`. |

The screen after the Enter is not kept: it is the CLI's main screen, which the tests do not need, and it may show details of the account.

The dialog's text in 2.1.326 is the 2.1.322 text, wrapped to 80 columns. The first try sent Down as soon as the dialog appeared, before the CLI's questions to the terminal. The cursor then moved down and back up, as if the key had been taken twice, and the Enter after it was lost: no trust was recorded and nothing started. So the codec now waits until the screen has stopped changing before each key.

### Trusting a directory in advance: what was tried (2.1.326)

None of these trusts a directory for an interactive session in advance:

- `command claude --help`: only `-p` says the trust dialog is skipped, and hataori does not use `-p` (design, section 3.4). No flag trusts a directory.
- The settings documentation (code.claude.com/docs/en/settings): trust decisions are per-project state in `~/.claude.json`, which Claude Code "writes for itself; you don't need to edit it". No settings key sets trust.
- The security documentation (code.claude.com/docs/en/security): the trust dialog appears in an interactive session for a folder not trusted. In the home directory, trust is held for the session only, and "there is no setting to persist it".
- The permissions documentation (code.claude.com/docs/en/permissions): trusting a parent folder does not cover a nested repository, so a clone inside a trusted directory still gets the dialog.
- The environment-variable documentation (code.claude.com/docs/en/env-vars): the only trust-related variable is `CLAUDE_CODE_DISABLE_ULTRAREVIEW_TRUST_CHECK`, for `claude ultrareview` alone. The binary also holds `CLAUDE_CODE_IGNORE_PERSISTED_TRUST`, which does the opposite, and variables of background sessions; none of them is documented for this.

## The first Enter lost (2.1.327, taken for #178)

`2.1.327/first_enter_lost/` is not from `capture.py`. It is a session that hataori ran with the CLI under a person's own settings: no `--setting-sources`, so their status line, hooks, plugins, Remote Control and the full-screen interface (`tui: fullscreen`) were all loaded; the model `opus`; the session host's 24×80 terminal; a new workspace, so the trust dialog came first. Its terminal log and its `SessionStart` hook were cut and scrubbed by hand: the workspace path became `/tmp/capture-work/sessions/first_enter_lost`, the chat ids became zeros, and the plan's name, the usage figures and the Remote Control link were replaced. Nothing else was changed.

| File | What it is |
|---|---|
| `screen-before.raw` | The terminal up to the moment `SessionStart` was reported: the trust dialog, answered by the codec, and taken down; bracketed paste switched off (`ESC[?2004l`). |
| `echo.raw` | What came next: hataori's paste, written at `SessionStart`, echoed by the terminal's line discipline (`^[[200~…^[[201~`, and the Enter as `\r\n`). The CLI did not read keys yet. |
| `screen-after.raw` | The rest: the CLI switches bracketed paste on, enters the full-screen interface and draws its input box, holding the pasted text and an empty line below it, where the Enter went. No `UserPromptSubmit` followed; the turn ended "not delivered". |
| `hooks/01-SessionStart.json` | The `SessionStart` hook (`source: startup`). |

## Usage per turn (2.1.327, taken for #195)

`2.1.327/usage/`, by `capture.py RAW_DIR usage` with the setup above, model `haiku`, and `TMPDIR` set to a directory of its own; `scrub.py` now maps that directory back to `/tmp`, in its dashed form too.

| Turn | Prompt | Hooks | Transcript |
|---|---|---|---|
| 1 | run `echo one` and `echo two` with the Bash tool, as two calls, then reply `done` | `UserPromptSubmit`, `PreToolUse` ×2, `PostToolUse` ×2, `Stop` | two model messages: the first, with both tool calls, written as two lines that carry the same `message.id` and the same `usage`; the second, the answer |
| 2 | reply with the word `ok` | `UserPromptSubmit`, `Stop` | one model message |

What it shows:

- Usage is in the transcript only, on assistant lines (`message.usage`: `input_tokens`, `output_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`, with `message.model`). Assistant lines carry no `promptId`; the user line before them does.
- No hook payload carries usage, the model of a message, or the context window, and the transcript gives no context window either. The `cost-state` line holds the session's totals per model, larger than the sum of the messages: it also counts the CLI's own requests.
- The final message's line was written 130 ms (turn 1) and 180 ms (turn 2) before the `Stop` hook was called.
- The line the CLI writes for an API error (`2.1.322/failure`) has the model `<synthetic>` and zero counts.
- The times above come from the lines' `timestamp`, which is when the message was made, not when the line reached the file. 2.1.330 (below) shows that the line can reach it after the `Stop` hook.

## Usage per turn, written after the end (2.1.330, taken for #675)

`2.1.330/usage/`, by `capture.py RAW_DIR usage` with the setup above, model `haiku`, and `TMPDIR` set to a directory of its own: the same two turns as `2.1.327/usage/`. The recorder also wrote, for each hook call, how many complete lines the transcript held at that moment (`hooks/NN-<event>.lines`).

| Hook | Lines in the transcript | What it shows |
|---|---|---|
| `03-PreToolUse`, `04-PreToolUse` | 28 | The message with both tool calls is lines 31 and 32: not yet written when its tools' `PreToolUse` hooks ran. |
| `05-PostToolUse`, `06-PostToolUse` | 32 | It is written by then. |
| `07-Stop` | 43 | The first turn's answer, the message after the tool results, is line 46: written after the turn's `Stop`. |
| `08-UserPromptSubmit` | 48 | It is written by the next prompt. |
| `09-Stop` | 53 | The text-only turn's one message is line 55: written after its `Stop` too. |
| `10-SessionEnd` | 60 | |

So a reader of both, as the runner is, reads the last message of a turn, and its usage, after the turn's end, and a turn that calls no tool has all of its usage there. The lines' `timestamp` puts those two messages 168 and 227 ms before their `Stop`. 2.1.330 also writes lines of types the codec does not read (`mode`, `permission-mode`, `session-owner`, `atis-latch`, `data-flow-records`, `last-prompt`, `ai-title`, `tools-used`); their ids are scrubbed as all UUIDs are.

## A confined session (2.1.327, taken for #268)

`2.1.327/confine_*/`, by `confine.py RAW_DIR`, then `scrub.py`. `confine.py` uses `capture.py`'s pseudo-terminal, environment and recorder hook, which answers every `PermissionRequest` with allow, so that a refusal is the CLI's confinement and not an unanswered prompt. Each attempt runs in a new empty temporary directory. "Outside" is a second one. Both become `/tmp/capture-work` when scrubbed. The command line is:

```
claude --settings <file> --setting-sources project,local --model haiku --permission-mode default \
  --restricted --strict-mcp-config --mcp-config <file> [--tools <list>] --session-id <uuid>
```

The MCP config names one stdio server, `sv`, written by the script, with one tool, `ping`, that only answers. The flag sets:

| Prefix | `--tools` |
|---|---|
| `confine_section71_` | not given: the set that section 7.1 of the design checked with `-p` |
| `confine_mcp_only_` | `""`: no built-in tool |
| `confine_files_` | `Read,Write,Edit,Glob,Grep` |

Each attempt is one typed turn that names one tool and says to reply `no-tool`, and to use nothing else, if that tool is missing. Each scenario directory holds `hooks/` and `transcript.jsonl` as above, plus:

- `tools.json`: every tool name sent to the model (`api-request-shape` lines, read before scrubbing blanks them), the deferred tools, skills and agent types the session announced (`deferred_tools_delta`, `skill_listing`, `agent_listing_delta` attachments). Names that neither Claude Code nor the script brings would only be counted, under `other_*`. All counts are 0.
- `outcome.json`: the tool calls in the hooks, the final answer, and whether the file the attempt was asked to write exists.

| Scenario | What was asked | Result |
|---|---|---|
| `*_tools` | reply `ok` | `section71`: `Agent`, `AskUserQuestion`, `Edit`, `Glob`, `Grep`, `ListAgents`, `Read`, `ScheduleWakeup`, `Skill`, `ToolSearch`, `Write`, `Artifact`, `ReportFindings`, `SendFeedback`, `DeferredToolPlaceholder` and `advisor`, 16 deferred tools (`WebSearch`, `CronList`, `CronDelete`, `PushNotification`, `SendMessage`, `TaskStop`, `EnterWorktree`, `NotebookEdit` among them), 17 skills and 6 agent types; `sv` was still pending at the first request. `mcp_only`: `mcp__sv__ping` only. `files`: the five file tools and `mcp__sv__ping`. Both `--tools` sets also list `DeferredToolPlaceholder` and `advisor`, and announce no deferred tool, skill or agent type. |
| `*_shell`, `*_webfetch`, `*_subagent`, `*_wakeup` | `Bash` `echo confined`; `WebFetch`; `Agent`; `CronCreate`, `ScheduleWakeup` or `Monitor` | `no-tool`, no tool call, in both `--tools` sets |
| `*_write_outside`, `*_read_outside` | `Write` or `Read` of a file in the outside directory | `mcp_only`: `no-tool`. `files`: `PreToolUse`, then the tool's error in the transcript, `<path> is outside <cwd>; --restricted confines the file tools to the working directory.`; no `PostToolUse` or `PostToolUseFailure` hook; no file written |
| `*_write_inside` | `Write` of `a.txt` in the working directory | `mcp_only`: `no-tool`. `files`: `PermissionRequest` (answered allow), the file written |
| `*_mcp` | `mcp__sv__ping` | `PermissionRequest` (answered allow), `pong from sv`, in both sets |
| `confine_files_resume_first`, `_resume_same_flags`, `_resume_no_flags` | one turn; `--resume` with the same flags and a `Bash` attempt; `--resume` without them and the same attempt | same flags: `no-tool`. Without them: `Bash` ran. The confinement belongs to the launch, not to the session. |
| `confine_files_model_switch` | `/model sonnet` typed, the CLI's "Switch model?" dialog confirmed with Enter, then a `Bash` and an `Agent` attempt | `no-tool` twice, answered by `claude-sonnet-5-5`. The tool list did not change. `EndConversation` was announced as deferred, and nothing could load it, since `ToolSearch` was not there. **Confirming the dialog made the CLI save the choice in the user-level settings file** (`"model": "sonnet"` in place of the user's own value), outside the session's temporary directories, even with `--restricted` and `--setting-sources project,local`. The maintainer put the file back. `confine.py` now runs this scenario only with `CLAUDE_CONFIG_DIR` set to a directory other than the user's own, and otherwise records it as not run. |
| `confine_files_model_resume_first`, `_model_resume_sonnet` | one turn; `--resume` with the same flags but `--model sonnet`, and an `Agent` attempt | `no-tool`; the same tools, and `EndConversation` deferred, as above |
| `confine_files_project_settings` | list tools and skills, then `Bash`, a skill or `mcp__project__ping` | The working directory held `.claude/settings.json` (allowing `Bash`, `WebFetch` and `Agent`; `enableAllProjectMcpServers`; a `UserPromptSubmit` hook that touches a file outside), `.claude/settings.local.json` (allowing `Bash`), `.mcp.json` (server `project`), a skill, an agent and a command, each naming `Bash`. None was loaded: the tool list was unchanged, no skill or agent type was announced, and the hook did not run. |
| `confine_files_settings_flag` | `Bash` | The `--settings` file also allowed `Bash`, `WebFetch`, `Agent` and `CronCreate` and set `defaultMode: bypassPermissions`: `no-tool`, the tool list unchanged |
| `confine_files_user_config` | list tools and skills | No `--setting-sources`, and the user's plugins not switched off, so the user's own settings, skills and plugins could load: the tool list unchanged, nothing from them listed |
| `confine_files_plugin_dir` | list tools and skills | A plugin given with `--plugin-dir`, holding an MCP server and a skill: neither was loaded |

Notes:

- `advisor` stays in the list under `--tools`. It is a server-side tool (it consults another model) and has no access to the machine. `DeferredToolPlaceholder` is not a tool that the model called in any attempt.
- The model kept to the prompts except in `confine_mcp_only_shell`: it called `mcp__sv__ping` before answering `no-tool`.
- Every session also leaves the CLI's own state in the user's configuration: a transcript under `projects/`, its prompts in `history.jsonl`, and a project entry in `~/.claude.json`. A full run of `confine.py` adds about 65 of each, one per session. They are not settings, and the script does not remove them.
- A person at the terminal could still type commands such as `/add-dir`, `/cd` or `!`. Those were not tried: the confinement is about what the model can do.

## Pasted tabs (2.1.328, taken for #393)

`2.1.328/paste_tabs/`, by `capture.py RAW_DIR paste_tabs`, then `scrub.py`. The command line, environment and recorder hook are the ones above, except that `CLAUDE_CONFIG_DIR` is a new, empty temporary directory, so there is no login. The script writes only a `.claude.json` into it, with `hasCompletedOnboarding`, a theme and `hasTrustDialogAccepted` for the session's directory, so that no first-run screen comes before the input box; the directory is deleted afterwards. `UserPromptSubmit` is called before any request is made, so no login is needed.

Two inputs were sent, each as one bracketed paste and then Enter. The text pasted is kept byte for byte next to the hooks:

| Pasted | Hook | What the prompt holds |
|---|---|---|
| `pasted-1.txt`: `tab<TAB>here, two<TAB><TAB>tabs; end`, one line | `02-UserPromptSubmit.json` | `tab    here, two        tabs; end`: each tab is exactly four spaces, not spaces up to the next tab stop |
| `pasted-2.txt`: Go source of 41 lines, indented with tabs, with Japanese, backticks, `$HOME` and a literal `\t` | `04-UserPromptSubmit.json` | the same text with each tab as four spaces and every other byte unchanged; not inside a `pasted_content` tag |

Each prompt is followed by `StopFailure` (`authentication_failed`, `Not logged in · Please run /login`); `SessionEnd` after `/exit`. The transcript shows the same prompts.

## The folder-trust dialog that names files (2.1.328, taken for #423; 2.1.332 for #730)

`2.1.328/trust_files_*/` and `2.1.332/trust_files_env/`, taken by `TestLiveTrustFiles` in the package, then `scrub.py`:

```sh
HATAORI_REAL_CLAUDE=1 HATAORI_CAPTURE_DIR=<raw dir> go test -run TestLiveTrustFiles -v ./internal/agent/claudecode
```

It runs with `TMPDIR` set to a short directory, so that the workspace's path fits on one line of the 80 columns and `scrub.py` finds it whole. Each case gets a new, empty `CLAUDE_CONFIG_DIR`, so there is no login and nothing of the user's is read or written; the test writes only a `.claude.json` there with `hasCompletedOnboarding` and a theme, and removes the directory afterwards. The workspace is a new `git init` directory holding the files below. The command line and environment are the adapter's own `Launch` for the spec, on the session host's 24×80 terminal, except that `--settings` names a recorder of `SessionStart` and `SessionEnd` in place of hataori's hooks. The codec itself handled the dialog.

| Scenario | Workspace | Spec | Dialog's extra line | What the codec did |
|---|---|---|---|---|
| `trust_files_refused` | `.claude/settings.json` | ordinary, no `TrustFiles` | `Trusting this folder loads .claude/settings.json.` | no key; one `SessionEnded`, `blocked at start`, naming the file (`events.txt`). No hook ran. |
| `trust_files_allowed` | `.claude/settings.json` | ordinary, `TrustFiles: [".claude/settings.json"]` | the same | Down, then Enter (`keys.txt`); `SessionStart` followed |
| `trust_files_confined` | `.claude/settings.local.json` with a status line, a `SessionStart` hook and `env` | confined | `Trusting this folder loads .claude/settings.local.json (runs commands and sets environment variables).` | Down, then Enter; `SessionStart` from `--settings` followed |
| `trust_files_both` | both files above | ordinary, `TrustFiles` naming both | `Trusting this folder loads .claude/settings.json and .claude/settings.local.json (runs commands and sets environment variables).` | Down, then Enter; `SessionStart` followed |
| `trust_files_mcp` | `.mcp.json` naming one stdio server | ordinary, no `TrustFiles` | `Trusting this folder loads .mcp.json here or above (MCP servers).` | no key; one `SessionEnded`, `blocked at start`, naming `.mcp.json`. No hook ran. |
| `trust_files_env` (2.1.332) | `.claude/settings.json` with `env` only | ordinary, `TrustFiles: [".claude/settings.json"]` | `Trusting this folder loads .claude/settings.json (sets environment variables).` | Down, then Enter; `SessionStart` followed |

Each holds `screen-NN.raw`, the terminal output chunk by chunk as the codec was given it up to its key or its event, `keys.txt`, `events.txt` and `hooks/`. The screen after the Enter is not kept.

Seen by hand on 2.1.328, not kept: a settings file with only hooks gives `(runs commands)`; the captures of #592 below show it. Only these notes are known behind a file (#730).

## The folder-trust dialog with a warning (2.1.329, taken for #592)

`2.1.329/trust_hooks_*/` and `2.1.329/trust_permissions/`, taken by `TestLiveTrustFiles` as above, one prefix at a time, then `scrub.py`:

```sh
HATAORI_REAL_CLAUDE=1 HATAORI_CAPTURE_DIR=<raw dir> HATAORI_CAPTURE_ONLY=trust_hooks go test -run TestLiveTrustFiles -v ./internal/agent/claudecode
HATAORI_REAL_CLAUDE=1 HATAORI_CAPTURE_DIR=<raw dir> HATAORI_CAPTURE_ONLY=trust_permissions go test -run TestLiveTrustFiles -v ./internal/agent/claudecode
```

2.1.329 draws a warning between "... execute files here." and the line naming the files when the project settings declare hooks or allow tools. On the 80 columns it wraps.

| Scenario | Workspace | Spec | Dialog's extra lines | What the codec did |
|---|---|---|---|---|
| `trust_hooks_refused` | `.claude/settings.json` with a `SessionStart` hook | ordinary, no `TrustFiles` | `⚠ This folder configures hooks that can run commands without asking, declared in .claude/settings.json`, then `Trusting this folder loads .claude/settings.json (runs commands).` | no key; one `SessionEnded`, `blocked at start`, naming the file, with the warning quoted. No hook ran. |
| `trust_hooks_allowed` | the same | ordinary, `TrustFiles: [".claude/settings.json"]` | the same | Down, then Enter; `SessionStart` followed |
| `trust_permissions` | `.claude/settings.json` allowing `Bash(ls:*)` | ordinary, `TrustFiles: [".claude/settings.json"]` | `⚠ This folder pre-approves 1 tool permission in .claude/settings.json:`, `Bash(ls:*)`, `These will apply without asking. Only proceed if you trust this configuration.`, then `Trusting this folder loads .claude/settings.json.` | nothing, as the codec of that time did not know the screen: no key and no event within 15 s. It reads it since #596. |

Each holds the screens, `keys.txt`, `events.txt` and, when the CLI started, `hooks/`.

## Turns that end without the list of background tasks (2.1.331, taken for #801)

`2.1.331/failure_background/` and `2.1.331/interrupt_background/`, by `capture.py RAW_DIR failure_background interrupt_background`, then `scrub.py`, with the setup above. In both, the first turn starts a background subagent (which runs a 60 s command in the foreground) and a background shell (90 s), and ends; its `Stop` lists both as running. Then:

- `failure_background`: a second turn while the subagent's command runs, with no connection. The session runs with `HTTPS_PROXY` set to a local proxy that passes TLS through unread, and `CLAUDE_CODE_MAX_RETRIES=1`; the proxy drops every connection before the turn and takes them again after it.
- `interrupt_background`: a second turn that runs a foreground command, interrupted with Escape.

Two more, with the same proxy:

- `failure_resumed`: a background subagent that ended and that `SendMessage` then resumed; a turn fails while it runs again.
- `failure_monitor`: a `Monitor` and a background shell; `TaskStop` stops the shell, a turn fails while the monitor runs, and `TaskStop` stops the monitor.

What they show:

- `StopFailure` (`error: server_error`) has no `background_tasks`. Neither has the interrupt, which fires no hook.
- Neither ends the background tasks: the subagent's command ends (`PostToolUse` with its `agent_id`), then `SubagentStop`, then `UserPromptSubmit` with a `<task-notification>` prompt that holds the subagent's `agent_id` as `<task-id>` and a `<status>`; its `Stop` lists the shell only, and the shell's end is notified the same way. The last `Stop` has an empty list.
- A task's id is in the result of the tool that started it: `agentId` next to `isAsync: true` for `Agent`, `backgroundTaskId` for `Bash`. `SubagentStart` comes before or after that result. `Monitor` gives `taskId`.
- A subagent that is resumed keeps its id: `SubagentStart` again, and `SendMessage`'s result has it as `resumedAgentId`.
- A task that `TaskStop` stopped is not notified: only the tool's result says so, as `task_id`, and the next list lacks it.
- The `SubagentStop` still lists its own subagent as running.
- A `queue-operation` line (`enqueue`, no `uuid`) in the transcript holds each notification, just before the turn it starts.
- The transcript's `turn_duration` line after each turn has `pendingBackgroundAgentCount`: 1, the subagent without the shell; absent when only the shell runs. The line comes after `Stop` and `StopFailure`, and not at all after the interrupt.

## The folder-trust dialog below a notice (2.1.331 and 2.1.332, taken for #707 and #728)

`2.1.331/trust_notice_*/`, taken by `TestLiveTrustFiles` as above with `HATAORI_CAPTURE_ONLY=trust_notice` (which takes the `trust_notice_off_*` below too), then `scrub.py`.

The CLI prints one line before it draws anything, "Claude Code's fullscreen renderer didn't finish starting last time on this machine, ...", when an earlier start with its full-screen interface did not get far enough. Its state (`.claude.json`) names such a start under `fullscreenBootPending`, by process, from the moment the interface starts until it has run for a while; the next start that finds the named process gone counts a strike (`fullscreenBootStrikes`), prints the line and uses the other interface. So the test (`interruptedStart`) sets `"tui": "fullscreen"` in the throwaway `CLAUDE_CONFIG_DIR`, starts the CLI once in a directory of its own, answers its dialog, and kills it as soon as the state names the start. The capture is the next start, in another new directory.

| Scenario | Workspace | Spec | What the codec did |
|---|---|---|---|
| `trust_notice_allowed` | no file | ordinary, no `TrustFiles` | Down, then Enter; `SessionStart` followed |
| `trust_notice_refused` | `.claude/settings.json` | ordinary, no `TrustFiles` | no key; one `SessionEnded`, `blocked at start`, naming the file, quoting the dialog without the notice. No hook ran. |

After a second such start the CLI prints another line once, "... has repeatedly failed to start on this machine, so it has been turned off here. ...", and uses the other interface from then on, until `/tui fullscreen` or an update. `2.1.332/trust_notice_off_*/` are such starts (taken for #728 with `HATAORI_CAPTURE_ONLY=trust_notice_off`): the test interrupts a start as above, lets the next one count it, interrupts the one after that too, and captures the fourth. 2.1.332 prints at most one of the two lines, and no other line of this kind. The workspaces, the specs and what the codec did are those of the table above.

## The folder-trust dialog with a permission warning (2.1.331, taken for #596)

`2.1.331/trust_permissions_*/`, taken by `TestLiveTrustFiles` as above with `HATAORI_CAPTURE_ONLY=trust_permissions`, then `scrub.py`.

| Scenario | Workspace | Spec | What the codec did |
|---|---|---|---|
| `trust_permissions_refused` | `.claude/settings.json` allowing `Bash(ls:*)` | ordinary, no `TrustFiles` | no key; one `SessionEnded`, `blocked at start`, naming the file, with the warning, the rule and its sentence quoted. No hook ran. |
| `trust_permissions_allowed` | the same | ordinary, `TrustFiles: [".claude/settings.json"]` | Down, then Enter; `SessionStart` followed |
| `trust_permissions_both` | both settings files, with ten rules, one of them `Trusting this folder loads x.` | ordinary, `TrustFiles` naming both | Down, then Enter; `SessionStart` followed |
| `trust_permissions_dirs` | `.claude/settings.json` allowing a tool and naming `additionalDirectories` | ordinary, `TrustFiles: [".claude/settings.json"]` | nothing, as the codec of that time did not know a second warning between the rules and the sentence: no key and no event within 15 s. It reads it since #752. |

The rules are on the line below the warning: up to eight, each cut at a width with `…`, then `, and N more`. Their text is the repository's. The warning counts every rule of every file; a rule written twice is shown once. On a short terminal the dialog is drawn without its blank lines.

Seen by hand on the same version, not kept: with hooks as well, the hooks warning follows the permission warning's sentence. The codec reads the two together since #752.

## The folder-trust dialog with several warnings (2.1.332, taken for #752)

`2.1.332/trust_warnings_*/`, taken by `TestLiveTrustFiles` as above with `HATAORI_CAPTURE_ONLY=trust_warnings`, then `scrub.py`. The dialog draws the warnings in this order: permissions, directories, `These will apply without asking. Only proceed if you trust this configuration.` below either or both, hooks. `TrustFiles` in the table names every file of the workspace.

| Scenario | Workspace | Spec | What the codec did |
|---|---|---|---|
| `trust_warnings_dirs_refused` | `.claude/settings.json` adding `/etc` with `additionalDirectories` | ordinary, no `TrustFiles` | no key; one `SessionEnded`, `blocked at start`, naming the file, with the warning, the directory and the sentence quoted. No hook ran. |
| `trust_warnings_dirs_allowed` | the same | ordinary, `TrustFiles` | Down, then Enter; `SessionStart` followed |
| `trust_warnings_permissions_dirs_refused`, `_allowed` | `.claude/settings.json` allowing `Bash(ls:*)` and adding `/etc` | as above | as above, both warnings quoted |
| `trust_warnings_permissions_hooks_refused`, `_allowed` | `.claude/settings.json` allowing `Bash(ls:*)` and declaring a `SessionStart` hook | as above | as above, both warnings quoted |
| `trust_warnings_dirs_hooks_refused` | `.claude/settings.json` adding `/etc`, `.claude/settings.local.json` declaring a hook | ordinary, no `TrustFiles` | as above, both files named, both warnings quoted |
| `trust_warnings_all_refused`, `_allowed` | both files allowing five tools and adding eight directories, the second declaring a hook too | as above | as above, both files named, all three warnings quoted |
| `trust_warnings_imitated_rule` | `.claude/settings.json` adding `/etc` and allowing a tool and a rule with the text of the directories' warning | ordinary, `TrustFiles` | nothing: a warning sign in the rules. No key and no event within 15 s. |
| `trust_warnings_imitated_dir` | `.claude/settings.json` adding `/etc` and a directory with the text of the hooks' warning | ordinary, `TrustFiles` | nothing, as above |
| `trust_warnings_headers` | `.claude/settings.json` allowing a tool, and `.mcp.json` with a `headersHelper` | ordinary, `TrustFiles` | nothing: `⚠ This folder runs commands to mint HTTP headers (headersHelper), declared in .mcp.json` is a warning the codec does not know |

The CLI writes the lines of a warning that start with ⚠ in bold, also where they wrap, and the rules, the directories, the sentence and the line naming the files not (the same in the captures of 2.1.329 and 2.1.331). It indents the first line of the rules and of the directories; where they wrap they go on at the margin, so the indent does not tell them from the dialog's own lines.
