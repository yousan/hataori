# Research: where a thread's pull request can come from

The dashboard shows a thread's pull request ([design, section 7.2](../design/overview.md#72-dashboard)). hataori does not call a code host's API itself, and a field with no source is left out. This note checks, by running them, the places that could name the pull request without hataori calling a code host: what Claude Code reports, what Codex reports, and what a workspace's git data holds. It changes no code. Issue #192.

- **Date:** 2026-11-18.
- **Versions:** Claude Code 2.1.327, codex-cli 0.163.0, git 2.43.0, gh 2.74.2.
- **Pull requests used:** #224 (this note's own, opened as a draft by a Claude Code session with `gh pr create --draft`), #131 (merged), both in this repository.
- **Login:** the CLIs' normal logins; `gh` was logged in to the code host.

Captures are in [`pull-request-source/captures/`](pull-request-source/captures/). Accounts, paths, session ids and the repository's owner and name are replaced (`OWNER/REPO`, `<path>`, `<uuid>`); usage, cost and rate-limit values are omitted.

## Setup

A scratch clone of this repository, outside any real workspace. Three small scripts recorded what each tool was given:

- a `gh` wrapper first on `PATH`, which appends its arguments to a log and runs the real `gh`, to see which code-host calls each CLI makes;
- a hook command for `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Stop`, `Notification` and `SessionEnd`, which appends its stdin to a log;
- a status line command, which appends its stdin to a log.

Claude Code ran interactive in a tmux pane with a scratch settings file:

```sh
tmux -f /dev/null -L pr192 new-session -d -s a -x 160 -y 45 \
  "env PATH=<wrapper dir>:\$PATH claude --model sonnet --settings <scratch>/settings.json"
```

Codex ran as:

```sh
PATH=<wrapper dir>:$PATH codex exec --json -s workspace-write \
  -c sandbox_workspace_write.network_access=true \
  "Run exactly this one command and nothing else, then reply done: gh pr view 224 --json number,url,state,isDraft"
```

## Claude Code

### Status line input: `pr`

The status line command's stdin has a `pr` object. With the scratch clone on #224's branch, it arrived on the first status line run after start ([capture](pull-request-source/captures/claude-statusline-open-draft.json)):

```json
"pr": {
  "number": 224,
  "url": "https://github.com/OWNER/REPO/pull/224",
  "review_state": "draft"
}
```

The footer showed `PR #224` at the same time. That session had not created the pull request and had not run any `gh pr` command: it found the pull request for the checked-out branch by itself. The `gh` wrapper logged only `gh auth token` and `gh auth token --hostname github.com` at start, so the lookup does not go through `gh pr` or `gh api`; Claude Code reads `gh`'s token. How it then reaches the code host was not traced. Either way it is Claude Code calling the code host, not something the workspace holds.

After `gh pr ready 224`, a new session on the same branch had `"review_state": "pending"` ([capture](pull-request-source/captures/claude-statusline-open-ready.json)).

On the branch of #131, which is merged, `pr` was absent from all three status line runs, including one after a turn ([capture](pull-request-source/captures/claude-statusline-merged-branch.json)). The [status line docs](https://code.claude.com/docs/en/statusline) say the same: `pr.number`, `pr.url` are the "Open pull request for the current branch … Absent … until a pull request or merge request is found, or once it merges or closes", and `pr.review_state` is one of `approved`, `pending`, `changes_requested` or `draft`, and "may be independently absent even when `pr` is present".

A branch change inside a session was not picked up. In one running session the clone was switched from #224's branch to #131's (`pr` disappeared), then back, first by `git switch` outside the session and then by the session itself running `git checkout research/192-pull-request-source`. For about four minutes and five status line runs after that, `pr` stayed absent and the wrapper logged no `gh` call. A new session in the same directory had `pr` again on its second status line run. Whether a running session ever looks again (after a longer wait, or when the pull request is merged) was not tested.

- **Number and URL:** yes.
- **Open, draft, merged:** open and draft yes: `draft` for #224 as a draft, `pending` after it was marked ready. Other values were not seen, and the docs allow `review_state` to be absent, which leaves draft unknown. Merged and closed no: `pr` is absent, the same as "no pull request".
- **Opened outside the session:** yes, for the branch checked out when the session started. Not seen after a branch change inside the session.
- **Only interactive:** `claude -p --settings <scratch>/settings.json "Say ok."` printed `ok` and never ran the status line command (its log stayed empty). The user settings on the test host have their own `statusLine`; the one passed with `--settings` ran instead, so hataori using this would replace the user's status line.

### Transcript: `pr-link` records

When the session that wrote this note ran `gh pr create --draft`, its transcript gained a record ([capture](pull-request-source/captures/claude-transcript-pr-link.json)):

```json
{"type": "pr-link", "sessionId": "<uuid>", "prNumber": 224,
 "prUrl": "https://github.com/OWNER/REPO/pull/224", "prRepository": "OWNER/REPO",
 "timestamp": "2026-11-18T07:26:15.730Z"}
```

Found with `grep '"type":"pr-link"' <transcript>.jsonl`. The same record, with a new timestamp, was appended again 17 times in the next 14 minutes, during turns that did not touch the pull request. The scratch session, which showed `PR #224` in its footer and ran `gh pr view 131`, has no `pr-link` record in its transcript (`grep -c pr-link` printed `0`). The record is not in the [hooks](https://code.claude.com/docs/en/hooks) or status line docs; it is an undocumented part of the transcript format.

- **Number and URL:** yes, and the repository.
- **Open, draft, merged:** no. The record has no state, and it was written for a draft.
- **Opened outside the session:** not seen. Of the two sessions tested, only the one that ran `gh pr create` has the record; the one that viewed a pull request and found another by its branch has none. Other ways a session might get the record were not tested.

### Hooks

No hook input names a pull request ([capture](pull-request-source/captures/claude-hook-inputs.json)): across `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse` and `Stop` there is no `pr` key. Only a `PostToolUse` and a `Stop` contain a `/pull/` URL, in the command's output and in the last assistant message. The [hooks docs](https://code.claude.com/docs/en/hooks) list no pull request field or event. A `PostToolUse` for `Bash` carries the command's output, so a pull request appears there only as text that `gh` printed.

- **Number and URL:** only by parsing a command's output.
- **Open, draft, merged:** only if the command printed it.
- **Opened outside the session:** no.

## Codex

`codex exec --json` emitted `thread.started`, `turn.started`, `item.started` and `item.completed` for a `command_execution`, `item.completed` for an `agent_message`, and `turn.completed` ([capture](pull-request-source/captures/codex-exec-events.json)). The pull request appears only in the command's `aggregated_output`.

The session's rollout file under `~/.codex/sessions` ([capture](pull-request-source/captures/codex-rollout-records.json)): `session_meta.git` has `branch`, `commit_hash` and `repository_url`, and nothing else. Only the tool call's output mentions `pull/224`, as a `custom_tool_call_output` and in the `item_completed` event for the same command. None of the 731 rollout files on the test host has a key with `pull` or `pr_` in it (`grep -ho '"[a-z_]*pull[a-z_]*"\|"pr_[a-z_]*"'`).

The source at tag `rust-v0.163.0` ([openai/codex](https://github.com/openai/codex/tree/rust-v0.163.0)) agrees:

- [`protocol/src/protocol.rs`](https://github.com/openai/codex/blob/rust-v0.163.0/codex-rs/protocol/src/protocol.rs), `struct GitInfo`: `commit_hash`, `branch`, `repository_url`.
- [`tui/src/branch_summary.rs`](https://github.com/openai/codex/blob/rust-v0.163.0/codex-rs/tui/src/branch_summary.rs): the TUI's `pull-request-number` status line item runs `gh pr view --json number,url,state`, then `gh api repos/{repo}/commits/{head}/pulls`. That is the TUI calling the code host, and only in the TUI's own status line; it reaches neither `codex exec` nor the app server's events.
- [`app-server-protocol/src/protocol/v2/thread_attachment.rs`](https://github.com/openai/codex/blob/rust-v0.163.0/codex-rs/app-server-protocol/src/protocol/v2/thread_attachment.rs): the app server has `thread/attachment/add`, `thread/attachment/list` and a `thread/attachment/updated` notification; a test in `app-server/src/in_process.rs` uses `attachment_type: "pull_request"`. A client adds an attachment with an `attachment_type`, an `identity_key` and a free `payload`; `grep -rln -e ThreadAttachmentAdd -e attachment_type codex-rs` finds it only in the app server, the thread store and the state database, not in the agent core. It stores what a client already knows; it does not find a pull request.

- **Number and URL:** only by parsing a command's output.
- **Open, draft, merged:** only if the command printed it.
- **Opened outside the session:** no.

## A workspace's git data

After `gh pr create` in the workspace that opened #224:

- `git config --get-regexp '^branch\.research'` printed only `remote origin` and `merge refs/heads/research/192-pull-request-source`, which `git push -u` had written.
- `git for-each-ref` listed no ref with `pull` in it.
- `git reflog` had the commit and the checkout, nothing about the pull request.

In a fresh clone, `gh pr checkout 224` added `[branch "research/192-pull-request-source"]` with `remote = origin` and `merge = refs/heads/research/192-pull-request-source`, and the reflog gained `checkout: moving from main to research/192-pull-request-source`. No number. In gh 2.74.2's [`pkg/cmd/pr/checkout/checkout.go`](https://github.com/cli/cli/blob/v2.74.2/pkg/cmd/pr/checkout/checkout.go), `cmdsForMissingRemote` sets `branch.<name>.merge` to `refs/pull/<N>/head` only when the pull request's head repository is not a configured remote (a fork) and the maintainer cannot push to it. Only then does git config carry the number, and only for a pull request checked out with `gh`.

The code host has the refs: `git ls-remote origin 'refs/pull/224/*' 'refs/pull/131/*'` listed `refs/pull/131/head` (merged), `refs/pull/224/head` and `refs/pull/224/merge` (open). That is a call to the code host. The clone's fetch refspec is only `+refs/heads/*:refs/remotes/origin/*` (`git config --get-all remote.origin.fetch`), so a fetch does not bring them, and the ref names say nothing about draft.

- **Number and URL:** no, except the fork case above (number only, no URL host or state).
- **Open, draft, merged:** no.
- **Opened outside the session:** not applicable; git records no pull request either way.

## Summary

| Source | Number and URL | Open / draft / merged | Opened outside the session |
|---|---|---|---|
| Claude Code status line `pr` | yes | open and draft (`review_state`); merged or closed look like none | yes, for the branch at session start |
| Claude Code transcript `pr-link` | yes | no state | no |
| Claude Code hooks | output text only | output text only | no |
| Codex events and rollout | output text only | output text only | no |
| Codex app-server thread attachments | only what a client stored | only what a client stored | no |
| Workspace git data | no (number only for some forks) | no | — |

## Recommendation

**No source; leave the field out.** The only source that names a pull request with any state and works for one opened elsewhere is Claude Code's status line `pr`. It is Claude Code calling the code host with `gh`'s token, it exists only for interactive Claude Code sessions (none for Codex), it was found when the session started and not after a branch change, a merged or closed pull request is indistinguishable from none, and using it means hataori replaces the user's own `statusLine`. The other sources either have no state (`pr-link`, undocumented, seen only for a pull request the session created) or are free text in a command's output. None of them meets "a field with no source is left out" for both agents. #212 can be closed as not planned unless the design accepts a Claude Code only field fed by the status line.
