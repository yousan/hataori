# Research: remaining usage and reset times reported by Claude Code and Codex

The dashboard's status bar shows each agent's remaining usage and its reset time ([design, section 7.2](../design/overview.md#72-dashboard)). hataori does not guess a number, so this note looks for a value that the agent itself reports, run against the real CLIs (#191). It changes no hataori code.

- **Claude Code:** 2.1.327 (`claude --version`), interactive, in a tmux pane; `--model sonnet`; a claude.ai subscription whose only window is weekly.
- **Codex:** codex-cli 0.163.0 (`codex --version`), `codex app-server` over stdio; signed in with ChatGPT, plan `plus`.
- **Date:** 2026-11-18.

Scripts are in [`agent-usage-limits/`](agent-usage-limits/). Captures are in [`agent-usage-limits/captures/`](agent-usage-limits/captures/); each was passed through [`scrub.py`](agent-usage-limits/scrub.py), which replaces session ids, paths, account ids, e-mail, host and time zone. Usage numbers and reset times are kept, since they are what the note is about. Timestamps are Unix seconds unless marked.

## Claude Code

Run:

```sh
cp log.py <dir>/ && python3 make-settings.py <dir>
cd <dir>/work && claude --settings <dir>/settings.json --model sonnet     # in a tmux pane
```

[`make-settings.py`](agent-usage-limits/make-settings.py) writes a settings file that sends every hook and the status line command's input to [`log.py`](agent-usage-limits/log.py), with the status line re-run every 5 s (`refreshInterval`). The user's own settings were not changed. In the pane: one prompt ("Reply with the single word: pong"), then `/usage`, then Escape, then about 15 minutes with no turn.

| Where | Remaining usage or reset time | Fields |
|---|---|---|
| Hooks (`SessionStart`, `UserPromptSubmit`, `Stop` observed) | **No.** No field about usage or limits ([capture](agent-usage-limits/captures/claude-hooks.jsonl)). | |
| `StopFailure` hook (not triggered) | Only that a limit was hit, not how much is left or when it resets. From the docs: `error` is `rate_limit`, `error_details` e.g. `"429 Too Many Requests"`, `last_assistant_message` e.g. `"API Error: Rate limit reached"` ([hooks, StopFailure input](https://code.claude.com/docs/en/hooks#stopfailure-input)). | `error`, `error_details` |
| `Notification` hook (not triggered) | Only that a wait for a usage limit ended: `notification_type` `quota_auto_resume_fired`, `quota_auto_resume_stale`, `quota_auto_resume_disabled` ([hooks, Notification](https://code.claude.com/docs/en/hooks#notification)). | `notification_type` |
| **Status line command's input** | **Yes**, see below. | `rate_limits.<window>.used_percentage`, `rate_limits.<window>.resets_at` |
| Transcript (`transcript_path`) | **No.** `grep -i 'seven_day\|rate_limit\|utilization'` on the session's transcript found nothing. | |
| `claude agents --json --all` | **No.** No key containing `rate`, `usage` or `limit` ([keys](agent-usage-limits/captures/claude-agents-keys.json)). | |
| `/usage` typed in the pane | Yes, as text on screen ([capture](agent-usage-limits/captures/claude-usage-screen.txt)): `Current week (all models) … 22% used`, `Resets Nov 20, 5pm (<time zone>)`, and a per-model week. | screen text only |
| `~/.claude.json`, key `cachedUsageUtilization` | Yes, undocumented, next to the account id. `fetchedAtMs` (1794986802088) is 38 s after the turn's `Stop` (`ts` in the [hook capture](agent-usage-limits/captures/claude-hooks.jsonl)), and `/usage` was the only thing done in the session after it, so `/usage` most likely wrote it (not confirmed) ([excerpt](agent-usage-limits/captures/claude-cached-usage-excerpt.json)). | `utilization.seven_day.utilization`, `.resets_at` (ISO 8601), `utilization.limits[].percent` |
| `claude -p --output-format stream-json --verbose` | Yes: one `rate_limit_event` in the one run made ([capture](agent-usage-limits/captures/claude-print-rate-limit-event.jsonl)). Print mode only; hataori runs Claude Code interactively. | `rate_limit_info.unifiedWindows.<window>.utilization` (0–1), `.resetsAt` |

### The status line command's input

Example ([capture](agent-usage-limits/captures/claude-statusline-with-rate-limits.json), other fields cut):

```json
"rate_limits": {
 "seven_day": {
  "used_percentage": 21,
  "resets_at": 1795161600
 }
}
```

`used_percentage` is a whole number from 0 to 100; `resets_at` is Unix seconds (here 2026-11-20 08:00 UTC, the same time `/usage` showed). The docs list two windows, `five_hour` and `seven_day`, and `spend_limit` behind a Claude apps gateway; each may be absent, the object is present only for claude.ai Pro, Max and Ultra subscribers (or behind such a gateway), and a window is dropped once its `resets_at` passes ([status line, available data](https://code.claude.com/docs/en/statusline#available-data)). This account had only `seven_day`.

**While no turn runs:** yes. The first run of the command, 1.7 s after `SessionStart`, had no `rate_limits` ([capture](agent-usage-limits/captures/claude-statusline-first.json)); the second, 4.8 s later, had it, before any prompt was sent and while `cost.total_api_duration_ms` was still 0 ([timeline](agent-usage-limits/captures/claude-statusline-timeline.jsonl)). The docs say it appears "only after the first API response in the session"; here it came before the first prompt, and what supplied it at start-up was not determined. After that, every run in the idle period carried the same value.

**How often it changes:** the command runs on events, on the `refreshInterval` timer, and when a window's `resets_at` passes ([when the status line updates](https://code.claude.com/docs/en/statusline#when-the-status-line-updates)). In this capture the value never changed: 213 runs over 1064 s, one turn among them; every run after the first (which had no `rate_limits`) had `used_percentage` 21 ([timeline](agent-usage-limits/captures/claude-statusline-timeline.jsonl)). It stayed at 21 for the 13 minutes after `/usage` showed 22 % in the same session ([screen](agent-usage-limits/captures/claude-usage-screen.txt)). So it can lag behind the account; an unchanged value cannot tell whether the CLI refreshed it with a stale number or not at all. Whether each API response refreshes it was not observed: one short turn does not move a weekly percentage. Print mode, run once, gave one `rate_limit_event` for its one turn.

**Cost to hataori:** a status line is one command per settings file. hataori would set `statusLine` in the settings it passes with `--settings`, which replaces any status line the user configured: a key set with `--settings` overrides the same key in the user's and the project's settings files ([settings precedence](https://code.claude.com/docs/en/settings#settings-precedence)); its command would have to forward the input to hataori and print a line itself.

## Codex

Run [`codex-drive.py`](agent-usage-limits/codex-drive.py), which starts `codex app-server` as it is and speaks JSON-RPC over its stdin and stdout:

```sh
python3 codex-drive.py idle.jsonl <work>            # initialize, account/rateLimits/read, no thread
python3 codex-drive.py turn.jsonl <work> turn       # the same, then one turn, then read again
POLLS=13 python3 codex-drive.py poll.jsonl <work>   # read every 60 s across a reset
```

The method and field names come from `codex app-server generate-json-schema --out <dir>` of the same binary (`v2/GetAccountRateLimitsResponse.json`, `v2/AccountRateLimitsUpdatedNotification.json`).

### `account/rateLimits/read`

Request: `{"method": "account/rateLimits/read", "id": 2}` with no params. Response ([capture](agent-usage-limits/captures/codex-ratelimits-read.json), cut):

```json
{
 "ordinaryUsageAllowed": true,
 "rateLimits": {
  "limitId": "codex",
  "primary":   {"usedPercent": 1,  "windowDurationMins": 300,   "resetsAt": 1794989084},
  "secondary": {"usedPercent": 26, "windowDurationMins": 10080, "resetsAt": 1794987561},
  "credits": {"hasCredits": false, "unlimited": false, "balance": "0"},
  "individualLimit": null,
  "spendControlReached": false,
  "planType": "plus",
  "rateLimitReachedType": null
 },
 "rateLimitsByLimitId": {"codex": { "...": "same as rateLimits" }}
}
```

From the schema: `rateLimits` is required ("backward-compatible single-bucket view"); `rateLimitsByLimitId` is a map keyed by metered limit id, may be null; in a window only `usedPercent` (integer) is required, `windowDurationMins` and `resetsAt` (Unix seconds) may be null; `primary` and `secondary` may each be null; `rateLimitReachedType` names why usage is blocked (`rate_limit_reached`, `workspace_member_usage_limit_reached`, …). The schema says of `ordinaryUsageAllowed`: "clients must not infer recovery from percentages or reset times."

**While no turn runs:** yes. It was answered 1.1 s after the request with no thread started ([capture](agent-usage-limits/captures/codex-idle.jsonl)).

### `account/rateLimits/updated`

A notification with params `{"rateLimits": <the same RateLimitSnapshot>}`. It came once in the turn, right after `thread/tokenUsage/updated` and before `turn/completed` ([capture](agent-usage-limits/captures/codex-turn.jsonl)). No such notification came while no turn ran: none in the 794 s of the poll run ([capture](agent-usage-limits/captures/codex-poll.jsonl)).

**How often it changes:** the notification said `primary.usedPercent` 1; a read 3 s after `turn/completed` said 2. So the notification can be older than a read made after the turn. What raised the value between them was not established: the poll run below shows the account's usage rising with no turn from this client.

The poll run read every 60 s for 794 s with no turn of its own ([capture](agent-usage-limits/captures/codex-poll.jsonl)):

| Seconds | `primary` (300 min) | `secondary` (10080 min) |
|---|---|---|
| 2 – 367 | 2 %, resets 1794989084 | 26 %, resets 1794987561 |
| 428 – 551 | 3 % | 26 % |
| 612 – 733 | 5 % | **0 %, resets 1795592364** |
| 794 | 6 % | 0 % |

- The value is the account's, not the connection's: `primary` rose from 2 % to 6 % while this client ran no turn. The source of that usage was not identified.
- At the reset, the next read (the first after `resetsAt` 1794987561) showed `secondary` at 0 % with a `resetsAt` 604803 s (7 days) later. No notification announced it.
- `resetsAt` varied by one second between reads (1794989084 and 1794989085); compare with a tolerance.
- Each read took 0.68 – 1.39 s, which suggests it asks the backend each time; the source was not read to confirm this.

## Recommendation

- **Claude Code:** use the status line command's input, `rate_limits.five_hour` and `rate_limits.seven_day` (`used_percentage`, `resets_at`), taken from the settings hataori already passes with `--settings`. Show a window only when it is present, and show the time it was last received, because it was seen to lag the account (21 % against `/usage`'s 22 %) and no change was observed while idle. Do not use `/usage` (keys typed into the user's pane), the `~/.claude.json` cache (undocumented, shared by every session, holds the account id) or print mode's `rate_limit_event` (hataori does not run print mode).
- **Codex:** use `account/rateLimits/read` from the app-server connection hataori already holds, read at start and after each `turn/completed`, and take `account/rateLimits/updated` as a hint to read again rather than as the value. Show `primary` and `secondary` with `windowDurationMins` and `resetsAt`, leaving out a window that is null.
