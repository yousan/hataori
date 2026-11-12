# Claude Code captures

Real output of the interactive `claude` CLI, for testing the adapter's codec. These files are not a contract: when the CLI changes, add a capture of the new version next to this one.

- `2.1.322/`: Claude Code 2.1.322, captured on 2026-11-12 (also in `2.1.322/VERSION`), on Linux, with the model `haiku` (it reports itself as `claude-haiku-5-5`).
- `capture.py` took them; `scrub.py` made them committable. `captures_test.go` in the package fails when a capture holds a home directory path, an e-mail address, an unscrubbed UUID or something shaped like a token.

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

`--setting-sources project,local` keeps the user's settings (their hooks, plugins, permissions) out. The settings file passed with `--settings` registers one command for 14 hook events. The command stores each payload byte for byte, one file per call. For `PermissionRequest` it prints `{"behavior": "allow"}`. For `PreToolUse` of `AskUserQuestion` it prints `permissionDecision: "allow"` with `updatedInput.answers` set to the first option of each question. Nothing is written to the working directory or to the user's settings.

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
