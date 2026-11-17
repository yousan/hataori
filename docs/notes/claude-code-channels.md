# Spike: input and replies through a Claude Code channel

Can a channel replace pasting into the terminal as the way into a Claude Code session ([design, section 3.4](../design/overview.md#34-decision))? This note records what happened against the real CLI. It changes neither the adapter nor the design.

- **CLI:** Claude Code 2.1.326, interactive, in a terminal (a tmux pane started with `tmux -f /dev/null`, so no tmux configuration was loaded).
- **Date:** 2026-11-17.
- **Model:** `--model sonnet`, which the CLI showed as Sonnet 5.5.
- **Login:** a claude.ai subscription. No API key was used.

Scripts are in [`claude-code-channels/`](claude-code-channels/), and the captures quoted below are in [`claude-code-channels/captures/`](claude-code-channels/captures/).

## Setup

- [`channel.py`](claude-code-channels/channel.py) is a two-way channel: an MCP server on stdio, written with the Python standard library only. It declares `claude/channel`, a `reply` tool, and optionally `claude/channel/permission`. It takes events to push on a Unix socket ([`push.py`](claude-code-channels/push.py)). It appends every reply-tool call, permission request and protocol message to a log.
- [`mkrun.sh`](claude-code-channels/mkrun.sh) makes a run directory. It holds a scratch settings file that sends every hook to [`hooklog.py`](claude-code-channels/hooklog.py), and a `work/.mcp.json` that starts the channel. The settings file sets `enableAllProjectMcpServers: true`, so the project-server consent dialog does not appear, and turns off a chat plugin that the user settings enable. With `ALWAYS_LOAD=1` the reply tool carries `"_meta": {"anthropic/alwaysLoad": true}`. With `ALLOW_REPLY=1` the settings allow `mcp__spike__reply`.
- [`start.sh`](claude-code-channels/start.sh) starts the CLI with a clean environment:

  ```sh
  claude --model sonnet --session-id <uuid> --settings <run>/settings.json \
    --dangerously-load-development-channels server:spike [--permission-mode default]
  ```

- [`timeline.py`](claude-code-channels/timeline.py) prints hooks and channel events in time order. [`busy-test.sh`](claude-code-channels/busy-test.sh) and [`twenty-test.sh`](claude-code-channels/twenty-test.sh) are the two tests below.

The server must be named in `.mcp.json` (or in user configuration). With `--mcp-config <file> --strict-mcp-config` the server started and pushes arrived. But the start-up notice said `server:spike · no MCP server configured with that name`, and Claude saw neither the tool nor the instructions.

## Starting: the dialog, and nobody at the terminal

`--dangerously-load-development-channels server:spike` first shows a full-screen dialog ([capture](claude-code-channels/captures/dev-channels-dialog.txt)):

```
  WARNING: Loading development channels
  --dangerously-load-development-channels is for local channel development only. Do not use this option to run channels you have downloaded off the internet.
  Please use --channels to run a list of approved channels.
  Channels: server:spike
  ❯ 1. I am using this for local development
    2. Exit
  Enter to confirm · Esc to cancel
```

Nothing goes past it. In 26 seconds there was no hook call, the channel server was not started, and no push could be delivered. Pressing Enter in the terminal started the session (`SessionStart`) and the channel server. The banner then showed `Channels (experimental) messages from server:spike inject directly in this session · restart without --dangerously-load-development-channels to stop`.

**Result:** a session with a custom channel cannot start with nobody at the terminal. Someone, or a program sending keys, has to answer this dialog on every start, like the folder-trust dialog ([section 3.5](../design/overview.md#35-what-hooks-do-not-cover-and-other-risks)). The docs agree: the flag "prompts for confirmation, so it takes effect in interactive sessions. With `-p`, Claude Code ignores the flag" ([CLI reference](https://code.claude.com/docs/en/cli-reference)). The other flag, `--channels`, only takes plugins on the approved allowlist ([channels](https://code.claude.com/docs/en/channels#research-preview)). So a channel of hataori's own would need the development flag, and this dialog, for as long as the preview lasts. **unverified:** whether `--channels` with an allowlisted plugin starts without a dialog. It was not run.

## A message pushed into the session

Each event was pushed as `notifications/claude/channel` with `content` and `meta: {chat_id}`.

**Idle session** ([capture](claude-code-channels/captures/idle-push.txt)): the push started a turn at once. `UserPromptSubmit` fired, and its `prompt` was the channel tag with the text unchanged:

```
<channel source="spike" chat_id="1">
What is 2+3? Answer in one short sentence.
</channel>
```

A `Stop` followed. The terminal showed the push as `← spike: What is 2+3? …`.

**During a turn** ([capture](claude-code-channels/captures/busy-push.txt), second run): a push started a 25-second foreground command. Two more pushes came 8 and 10 seconds later. Neither started a turn while the command ran. When it ended, both were delivered into the same turn, in the order sent, each with its own `UserPromptSubmit`. Claude then answered all three, and one `Stop` closed the turn.

```
    1.8  PreToolUse   Bash   python3 -c "import time; time.sleep(25)"
    8.1  chan pushed  Second message …
   10.2  chan pushed  Third message …
   27.3  PostToolUse  Bash
   27.4  UserPromptSubmit  <channel … chat_id="11"> Second message …
   27.7  UserPromptSubmit  <channel … chat_id="12"> Third message …
   29.3–31.3  reply tool for 10, 11, 12
   32.8  Stop
```

In the first run Claude moved the command into the background. A push that came between two tool calls was likewise delivered into the running turn, as another `UserPromptSubmit` without a `Stop` in between. The background command ended later. Its task notification then started a turn of its own (`UserPromptSubmit` with a `<task-notification>` prompt), exactly as in the paste path.

The docs say: "If several notifications arrive while Claude is busy, they're delivered together on the next turn" ([channels reference, notification format](https://code.claude.com/docs/en/channels-reference#notification-format)). That was not what happened. The pushes were held only while a tool call ran, and joined the turn already running.

**While a dialog is open** (a permission prompt or a question), a push is shown on screen but held. It was delivered, as its own `UserPromptSubmit`, when the dialog closed.

## Does the sender learn that a message arrived?

Not from the channel. "Claude Code doesn't acknowledge notifications. The `await` on `mcp.notification()` resolves when the message is written to the transport, not when Claude has processed it" ([channels reference](https://code.claude.com/docs/en/channels-reference#notification-format)). The run agrees: the server gets nothing back.

It can learn from the hooks, as with paste. Every push showed up as a `UserPromptSubmit` whose `prompt` is the `<channel>` tag, `meta` included, so a `chat_id` (or any id) put in `meta` identifies the message. This was true for an idle session, during a turn, and after a dialog. hataori can therefore keep its rule: "delivered means the `UserPromptSubmit` hook reported it". A reply-tool call is no proof of arrival, because Claude may not call it (next section).

## Replies through the reply tool, over 20 turns

[`twenty-test.sh`](claude-code-channels/twenty-test.sh) pushes twenty neutral questions ("Question *n*: what is *n* times 7? Answer in one short sentence."), each after the previous `Stop`. Nothing in the questions asks for the reply tool. The server's `instructions` say to answer every message with it.

| Reply tool | Answered through the reply tool | Capture |
|---|---|---|
| deferred (default) | **0 of 20** | [twenty-turns-deferred.txt](claude-code-channels/captures/twenty-turns-deferred.txt) |
| `"_meta": {"anthropic/alwaysLoad": true}` | **20 of 20** | [twenty-turns-alwaysload.txt](claude-code-channels/captures/twenty-turns-alwaysload.txt) |

With the default, MCP tools are deferred behind tool search ([MCP, tool search](https://code.claude.com/docs/en/mcp#scale-with-mcp-tool-search)). Asked to quote its MCP server instructions, Claude said it had been given none and had no `mcp__` tools. It called the tool only when a message said "send … back over this channel". The docs' own example server, the full `webhook.ts` with the reply tool from the [channels reference](https://code.claude.com/docs/en/channels-reference#expose-a-reply-tool), run with the official MCP SDK 1.32.2 under Bun, behaved the same ([capture](claude-code-channels/captures/sdk-example-deferred.txt)). The docs say "only tool names and server instructions load at session start". With this CLI, neither reached Claude for a channel server until the tool was marked to load upfront. With `alwaysLoad`, Claude quoted the channel's instructions verbatim.

What `Stop` carries changes with the reply tool. With the reply tool used, `last_assistant_message` was a note about the reply ("2 times 7 is 14. I sent that reply to q2."). Once it was a comment on the turn instead ("I've already answered question 1 …"). The answer itself was in the reply tool's `text`.

**Result:** with `alwaysLoad` the final answer can be taken from the reply tool. `Stop` is still needed, because it is the only sign that the turn ended. The reply tool also gives no sign of text produced before the final answer, or of an answer Claude did not route through the tool. The `Stop` hook gives the turn's end and its last text whatever Claude chose. So the reply tool adds nothing that hataori needs.

## Permission requests and questions

These were run with `--permission-mode default`, with `CHANNEL_PERMISSION=1`. The user settings put the session in auto mode, which decides most calls itself.

**Permission relay works** ([capture](claude-code-channels/captures/permission-relay-allow.txt)). For `touch allowed.txt`, the channel received `notifications/claude/channel/permission_request` with `request_id` (five letters), `tool_name`, `description` and `input_preview`. That was within 0.1 s of the `PermissionRequest` hook, while the terminal showed the dialog. A verdict sent back as `notifications/claude/channel/permission` closed the dialog and ran the command (`PostToolUse`), with `behavior: allow`. With `behavior: deny`, the call was rejected with "Denied via channel spike". Claude reported that the command did not run ([capture](claude-code-channels/captures/permission-deny-and-question.txt)).

Two catches:

- **The reply tool needs permission itself** in the default mode (`mcp__spike__reply` got a request of its own). A deny meant for the next command hit the reply instead. After that, Claude declined to act on the next channel message: "It came from the channel rather than from you, so I haven't acted on it." With `permissions.allow: ["mcp__spike__reply"]` in the settings, no request was made for the reply, and the deny reached the command.
- **Claude may treat channel text as coming from someone other than the user.** It did so once, after the deny above. In every other run it followed channel instructions, including shell commands. **unverified:** how often this happens.

**A question cannot be answered through the channel** ([capture](claude-code-channels/captures/permission-deny-and-question.txt)). For `AskUserQuestion`, `PreToolUse` and `PermissionRequest` fired, but nothing was sent to the channel. The docs say relay covers tool approvals such as `Bash`, `Write` and `Edit` ([relay permission prompts](https://code.claude.com/docs/en/channels-reference#relay-permission-prompts)). A channel message "blue" sent while the question was open did not answer it. It was held. After Escape it arrived as a new `UserPromptSubmit`, and Claude took the answer from it. The question path in [section 3.4](../design/overview.md#34-decision) (the `PreToolUse` hook replies with the answers) is still needed.

## Logins

| Login | Result |
|---|---|
| claude.ai subscription | Everything above. |
| API key (Console) | **Not run.** It costs money and no key was to be used on this host. The docs say channels "require Anthropic authentication through claude.ai or a Console API key" and are "permitted by default" for Console with an API key ([channels](https://code.claude.com/docs/en/channels)). |

On claude.ai Team and Enterprise, channels are blocked until an Owner turns on `channelsEnabled`. When that setting is off, the development flag is blocked too ([enterprise controls](https://code.claude.com/docs/en/channels#enterprise-controls)). The account used here had no such block. Bedrock, Vertex and Foundry are not supported.

## What Argon said, checked

Argon (Antigravity CLI, `agy -p`) was asked one question per criterion before anything ran. Its quotes were checked against the docs pages above, read on 2026-11-17.

- Correct, and confirmed by the run: the dialog and its two choices; the flag is ignored with `-p`; `server:<name>` and `plugin:<name>@<marketplace>` entries; the method names and fields for push, permission request and verdict; no acknowledgement; relay covers tool approvals only; claude.ai or Console logins; no Bedrock, Vertex or Foundry.
- It said the docs do not state what happens to a push into an idle session, or whether `UserPromptSubmit` fires. Both are true of the docs. The run answers both: a turn starts, and `UserPromptSubmit` fires with the `<channel>` tag.
- It repeated the docs' "delivered together on the next turn". The run showed delivery into the running turn instead.
- It did not mention that MCP tools are deferred and that server instructions do not reach Claude without `alwaysLoad`. That was the deciding fact for the reply tool.

## Recommendation

**Do not use channels, neither instead of paste nor beside it.**

Channels get input into a session: a push starts a turn and is reported by `UserPromptSubmit`, as a paste is. They also relay permission verdicts. But:

- A custom channel cannot start unattended. The development-channels dialog must be answered on every start, which adds a second terminal dialog to the one this design already struggles with (folder trust). Only allowlisted plugins avoid it.
- Everything hataori needs still comes from hooks. Turn end and final text come from `Stop`, questions from `PreToolUse`, and arrival from `UserPromptSubmit`. Permission requests are already answered by the `PermissionRequest` hook without a dialog.
- The reply tool is used only if it is marked `alwaysLoad`. It adds a permission request of its own unless allowed. Claude may also treat channel text as third-party input.
- It is a research preview. The flag syntax and the protocol "may change" ([channels, research preview](https://code.claude.com/docs/en/channels#research-preview)). The CLI already behaves differently from the docs in two places: busy-turn delivery, and deferred instructions.

Look again when a channel of hataori's own can be registered without the development flag. Until then, paste stays the way in.
