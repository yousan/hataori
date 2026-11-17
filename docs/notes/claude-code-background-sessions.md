# Spike: handing input to a `claude --bg` session

Can hataori give input to a session that Claude Code runs in the background under its own supervisor, so that hataori no longer needs to own a terminal ([design, section 3.4](../design/overview.md#34-decision))? This note records what happened against the real CLI. It changes neither the adapter nor the design.

- **CLI:** Claude Code 2.1.326.
- **Date:** 2026-11-17.
- **Model:** `--model sonnet`, which the CLI showed as Sonnet 5.5.
- **Login:** a claude.ai subscription.

Every command ran with a clean environment (`env -i HOME PATH LANG`), from a script with no terminal (stdin from `/dev/null`) unless a terminal is named. No supervisor was running before the first `--bg`. Every supervisor and session below was started by these runs and stopped by id at the end.

Scripts are in [`claude-code-background-sessions/`](claude-code-background-sessions/). Run directories and hook settings come from [`claude-code-channels/mkrun.sh`](claude-code-channels/mkrun.sh) and [`hooklog.py`](claude-code-channels/hooklog.py) (the `work/.mcp.json` it writes was removed for these runs). [`timeline.py`](claude-code-channels/timeline.py) prints the hooks. The quoted captures are in [`claude-code-background-sessions/captures/`](claude-code-background-sessions/captures/).

## Starting a session with nobody at the terminal

```sh
cd <run>/work && claude --bg --model sonnet --session-id <uuid> \
  --settings <run>/settings.json "<prompt>" </dev/null
```

([bg-start.sh](claude-code-background-sessions/bg-start.sh), [capture](claude-code-background-sessions/captures/start-unattended.txt))

| Asked for | Result |
|---|---|
| A given directory | Yes. The session runs in the directory `claude --bg` was started from (`cwd` in `agents --json`). |
| A given session id | **No.** The CLI printed `warning: --bg manages the session id; ignoring --session-id (use --resume <id> to continue an existing session)`. The session got its own id. The short id that `--bg` prints (`dc635feb`) is the start of the full UUID, which `SessionStart` reports as `session_id`. |
| Hooks from `--settings` | Yes. `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Stop` and the rest all reached the scratch hook log. The session keeps these options when it is woken later (below). |
| Nobody at the terminal | Yes. `--bg` printed the short id and the management commands and returned at once with status 0. The first call printed `Starting background service…` and started the supervisor. The turn ran and ended with `Stop`. |

**An untrusted directory** ([capture](claude-code-background-sessions/captures/untrusted-directory.txt)): run the same way in a fresh directory that had never been trusted (and is not inside a trusted one), `claude --bg` printed `Workspace not trusted. Run \`claude\` in <dir> once and accept the trust prompt, then retry.` and exited with status 1. No session was created, no hook ran, and no trust entry was written. This matches the docs: "Where no dialog can appear, such as in a script, the command exits with a `Workspace not trusted` error instead" ([agent view](https://code.claude.com/docs/en/agent-view#dispatch-an-agent-from-your-shell)). So background sessions do not get around the folder-trust problem ([section 3.5](../design/overview.md#35-what-hooks-do-not-cover-and-other-risks)). They fail cleanly instead of waiting at a dialog.

## Giving input to a running background session

| Way tried | Worked | Same session id |
|---|---|---|
| `claude --resume <uuid> --bg "<prompt>"` while the session's process runs | It ran the prompt, but **in a copy**: `note: session dc635feb is already running in the background, so this started a copy as 9d7e1719.` | **No.** `SessionStart` had `source: fork` and the new id. |
| The same after `claude stop <id>`, passing `--settings` again | Again **a copy**: `note: background session dc635feb keeps its own saved options, so the flags you passed started a copy as 604f53f6. Without flags, the same command continues dc635feb itself.` | **No** |
| `claude --resume <uuid> --bg "<prompt>"` after `claude stop <id>`, no other flags | **Yes:** `note: woke session dc635feb with its saved options (--settings, --model).` | **Yes.** `SessionStart` had `source: resume` and the same id, followed by `UserPromptSubmit` with the prompt. |
| `claude attach <id>` in a terminal (a tmux pane), the prompt given by bracketed paste, then Enter | **Yes** | **Yes.** `UserPromptSubmit` and `Stop` came from the same session. |

(Captures: [resume-bg.txt](claude-code-background-sessions/captures/resume-bg.txt), [attach.txt](claude-code-background-sessions/captures/attach.txt).)

So the only way without a terminal that keeps the session is to stop the session, then wake it with `--resume <uuid> --bg "<prompt>"` and no other flags. That costs a process restart and a `SessionStart` per message. It also cannot deliver into a turn that is running, or answer a session that is blocked on a question or a permission prompt, because those must be running to be answered. While a session runs, the only way in is a terminal, through `claude attach`. That is the paste path again, with Claude Code's supervisor between hataori and the terminal.

Not tried: replying from agent view's peek panel (also a terminal, driven by keys), and an interactive `claude --resume <uuid>` while the background process runs. The docs say that opens the running session itself ([sessions](https://code.claude.com/docs/en/sessions)).

## What `claude agents --json` reports, compared with the hooks

[`poll-agents.sh`](claude-code-background-sessions/poll-agents.sh) ran `claude agents --json --all` every 0.5 s. [`agent-states.py`](claude-code-background-sessions/agent-states.py) prints each change.

| Moment | `state` / `status` / `waitingFor` | Hook | Seconds after `--bg` |
|---|---|---|---|
| Just started | `working` / no `status`, no `pid` | | 0.6 |
| Process up | `working` / `idle` | `SessionStart` | 5.6 / hook 5.7 |
| Turn running | `working` / `busy` | `UserPromptSubmit` | 7.3 / hook 7.2 |
| Turn finished | `done` / `idle` | `Stop` | 22.3 / hook 22.2 |
| Waiting on a permission | `blocked` / `waiting` / `permission prompt` | `PermissionRequest` | 9.2 / hook 9.5 |
| Waiting on a question | `blocked` / `waiting` / `input needed` | `PreToolUse` + `PermissionRequest` for `AskUserQuestion` | 11.2 / hook 10.7 |

([start-unattended.txt](claude-code-background-sessions/captures/start-unattended.txt), [waiting-states.txt](claude-code-background-sessions/captures/waiting-states.txt))

The two agree to within the 0.5 s polling step, so neither is earlier in practice. `agents --json` gives state only. The final text, the questions and the permission details are only in the hooks. Interactive sessions are listed too (`kind: interactive`, with `pid` and `status`, without `state` or `id`), as the docs say ([agent view, list sessions as JSON](https://code.claude.com/docs/en/agent-view#list-sessions-as-json)).

## Surviving the process that started it, and a reboot

**The end of the starting process:** the `claude --bg` that started the first session exited at once. The session's process runs under the supervisor: supervisor → `claude bg-pty-host` → `claude bg-spare`, in its own session and process group. It kept working, and five more turns ran in it and its copies after that. Closing a `claude attach` terminal (killing its tmux pane) also left the session running (`done`/`idle`, same pid).

**The supervisor** was shown by `claude daemon status` as `origin: transient — started on-demand by \`claude --bg\``. It exited by itself once its last session was stopped.

**A reboot** was not done. What the run did instead ([supervisor-stop.txt](claude-code-background-sessions/captures/supervisor-stop.txt)) was stop the supervisor these runs had started. At that point it held only their sessions. `claude daemon stop --any` printed `stopped (ended 6 background sessions)`:

- Every session process ended. Each fired `SessionEnd` with `reason: other`.
- `agents --json --all` then showed the sessions that had finished their turn as `done`, and the two waiting on a permission or a question as `stopped`, all without a `pid`. Running `agents --json` did not start a supervisor again.
- `claude --resume <uuid> --bg "<prompt>"` with no other flags printed `Starting background service…` and `woke session dc635feb with its saved options (--settings, --model)`. The session ran the prompt under the same id (`SessionStart` with `source: resume`).

For an actual reboot, the docs say: "Shutting down or restarting your machine stops running background sessions." Within 48 hours a session shows as failed and restarts from its saved conversation when you attach or reply to it. After 48 hours it shows as stopped ([agent view, sessions show as failed or stopped after shutdown](https://code.claude.com/docs/en/agent-view#sessions-show-as-failed-or-stopped-after-shutdown)). Nothing restarts the supervisor or the sessions by itself. **unverified** by a run: the reboot itself, and the 48-hour rule.

## A person attached while hooks report elsewhere

Yes, both work at once ([attach.txt](claude-code-background-sessions/captures/attach.txt)). While `claude attach 9d7e1719` was open in a terminal, the prompt typed there produced `UserPromptSubmit` and `Stop` in the hook log, which belongs to another program, from the same session. `agents --json` went `busy`, then `done`. `←` on the empty prompt detached and opened agent view in that terminal. The background session kept running. The docs say detaching never stops a background session ([agent view, attach](https://code.claude.com/docs/en/agent-view#attach-to-a-session)).

## What Argon said, checked

Argon (Antigravity CLI, `agy -p`) was asked one question per criterion before anything ran. Its quotes were checked against the docs pages above, read on 2026-11-17.

- Correct, and confirmed by the run: `--bg` returns at once and prints the management commands; it checks workspace trust and exits with `Workspace not trusted` where no dialog can appear; the fields and values of `agents --json`, including interactive sessions; detaching never stops a session; sessions do not survive a shutdown and are not resumed by themselves.
- It said the docs do not say whether `--session-id` works with `--bg`. True of the docs. The run shows that it is ignored, with a warning.
- **Wrong:** asked whether the session id stays the same when input is given with `claude --resume <id> --bg`, it answered "Yes, resuming … keeps the same session ID". As its source it quoted a sentence from `https://code.claude.com/docs/en/manage-sessions`. That page does not exist ("Page Not Found"), and the sentence is in no page read. The run shows a copy with a new id whenever the session is running or flags are passed. The [sessions](https://code.claude.com/docs/en/sessions) page says such a resume becomes a background dispatch, and [agent view](https://code.claude.com/docs/en/agent-view#dispatch-an-agent-from-your-shell) says it may start a copy.

## Recommendation

**Do not use background sessions, neither as the main path nor as a fallback.**

- Input cannot be handed to a running background session without a terminal. `--resume <id> --bg` either makes a copy with another id, or works only after the session was stopped, and then costs a restart per message. It cannot answer a question or a permission prompt, and it cannot add to a running turn. The working way in is `claude attach` in a terminal, which is the paste path with an extra layer.
- `--session-id` is ignored, so hataori could not choose the session id. It would have to learn the id from `--bg`'s output or from `SessionStart`.
- It does not help with folder trust. An untrusted workspace fails at once with `Workspace not trusted`.
- The session lives under a supervisor that is shared by every background session of the same user and config directory. Stopping it (`claude daemon stop --any`) ends all of them. hataori would share its sessions' lifetime with whatever else the operator runs in the background.
- What it does give, state from `agents --json`, the hooks already give at the same moment and with more detail.

Keep the terminal that hataori owns ([section 3.4](../design/overview.md#34-decision)). The one useful finding is that `claude attach` and hooks work together. hataori's own `hataori attach` can follow the same model.
