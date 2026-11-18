# Configuration reference

This is everything hataori reads to configure itself: the instance root, the config file, the files the config points to, environment variables and command-line flags.
It is written from the code. When the two disagree, the code is right and this file is a bug.

## The instance root

One instance is one directory, its root. hataori reads and writes only under it.

```
<root>/
  config/
    hataori.json      the config file (you write it)
    <token file>      the Discord bot token (you write it)
  state/              created by hataori: database, lock, sessions, control socket
  workspaces/         created by hataori: one clone per thread
  mirrors/            created by hataori: one bare mirror per repository
```

- The root is given by `--root DIR`, or by the environment variable `HATAORI_ROOT` when `--root` is not given. Every command except `version`, `host` and `hook` needs one.
- `hataori run` creates `state/`, `workspaces/` and `mirrors/` (mode 0700) if they are missing, and holds a lock in `state/`. A second daemon on the same root refuses to start.
- A thread's working directory is `workspaces/<client>/<scope>/<thread>/`; for Discord, `workspaces/discord/<channel id>/<thread id>/`.
- Two instances with different roots on one host are independent. Give each its own config and its own bot; do not share or link one instance's `config/` into another.

## The config file

The config file is `<root>/config/hataori.json`. It is read once when the daemon starts; change it and restart the daemon.

- It must be a regular file that only its owner can read: a mode with any group or other bit set is refused (`chmod 600`).
- It is one JSON object. Unknown keys are refused, a `null` value is refused, and anything after the object is refused.
- Spaces around string values are ignored.
- Every error names the key it is about.

A complete example for one Discord channel and Claude Code, with the owner, one more person and a webhook that posts orders:

```json
{
  "owner": "ada",
  "people": [
    {"name": "ada",      "ids": ["discord:<ada's Discord user id>"]},
    {"name": "grace",    "ids": ["discord:<grace's Discord user id>"]},
    {"name": "dispatch", "ids": ["discord:<the webhook's id>"]}
  ],
  "trigger": "!hataori",
  "idle": "30m",
  "scopes": [
    {"key": "<channel id>", "client": "discord", "enabled": true, "people": ["grace", "dispatch"]}
  ],
  "drivers": {
    "discord": {"bot_id": "<the bot's user id>", "token_file": "discord-token"}
  },
  "adapters": {
    "claude-code": {}
  }
}
```

### Top-level keys

| Key | Type | Default | Meaning |
|---|---|---|---|
| `owner` | string | required | The name of the person, under `people`, who owns the instance. The owner may start threads in every enabled scope and post into every thread, and gets the login alert by direct message. In the old form (no `people`), the owner's id itself; see [People](#people). |
| `people` | array | none | The people hataori knows, one object each (below). Without this key the config is read in the old form. |
| `trigger` | string | required | The word that starts a thread. A message in an enabled channel that starts with this word, followed by a space or a line break, is an order (see [Orders](#orders)). |
| `idle` | duration | required | How long a session may sit without a turn before its agent process is stopped. The next message resumes the same conversation. A positive Go duration, such as `"30m"` or `"2h"`. |
| `model` | string | empty | The model of a thread whose order and scope name none. Empty leaves it to the agent's own default. |
| `effort` | string | empty | The effort of a thread whose order and scope name none. Empty leaves it to the agent's own default. |
| `context_bytes` | integer | `8192` | When the agent no longer has a thread's conversation and a new session replaces it, the thread's earlier inputs and final answers are given to the new session, cut to this many bytes. Must be greater than 0. |
| `login_relay` | boolean | `true` | When turns fail because the agent is logged out, the owner gets one direct message about it. With `true`, hataori also runs the agent's login command, sends the owner the sign-in link, and passes the code the owner replies with back to it. `false` keeps the alert and switches the relay off. |
| `scopes` | array | empty | The channels hataori serves, one object each (below). A channel that is not listed, or not enabled, starts nothing. |
| `drivers` | object | empty | One section per chat surface, by name. This binary has one: `discord` (below). A name it does not have is refused at start. |
| `adapters` | object | empty | One section per coding agent, by name. This binary has one: `claude-code` (below). Exactly one adapter must be named. |

Model and effort are chosen in this order: the order's `model:` / `effort:`, then the scope's, then the top-level key, then the agent's own default.

Names in `drivers`, `adapters` and `scopes[].client` start with a lowercase letter and contain only lowercase letters, digits and `-`.

### `scopes[]`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `key` | string | required | The scope's id. For Discord, the channel id. Each key may be listed once. |
| `client` | string | required | The driver that owns the scope: `"discord"`. It must also be configured under `drivers`. |
| `enabled` | boolean | `false` | Whether orders in this scope start threads. |
| `model` | string | empty | The model of threads started here whose order names none. |
| `effort` | string | empty | The effort of threads started here whose order names none. |
| `people` | array of strings | empty | Names of the people, under `people`, who may start threads here and post into them besides the owner. Empty strings are ignored. |
| `posters` | array of strings | empty | The old form only: ids, on this scope's client, of who may post here besides the owner. For Discord, user ids, or the id of a webhook for posts made through that webhook. Refused in a config with `people`. |

### `people[]`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `name` | string | required | The person's name, as `owner` and `scopes[].people` write it. Any word without a colon. Each name may be listed once. |
| `ids` | array of strings | empty | The ids by which chat services know the person, each `<client>:<id>`: the driver's name, a colon and the service's own id of the author. For Discord, `discord:<user id>`, or `discord:<webhook id>` for posts made through a webhook. One id may be listed once, under one person. Empty strings are ignored. |

### People

Who may act is written by person. A person has a name and ids, one per account on each chat service; a webhook or another automated poster is a person too, with the webhook's id.

- The owner may act in every scope; anyone else only in the scopes whose `people` name them.
- An author whose id no person lists is a **guest**. A guest's messages never reach the agent: a guest starts no thread, sends no input, answers nothing and stops nothing, and gets no reply. Each refusal writes one log line, `control call refused` with `reason="a guest"`, the client and the id; a person acting outside their scopes gets `reason="the person may not act in the scope"` and their name.
- hataori decides this in one place, its core, for every chat service; a second service needs no list of its own, only its ids under the people.
- Ids are linked to people only here, by whoever edits this file. Nothing said in chat changes who a person is.

**The old form.** A config without `people` is read as before: `owner` is the owner's id, and each scope's `posters` are ids on that scope's client. It is converted when the daemon starts, so it allows the same authors: the owner becomes a person named `owner` with the owner's id on every configured driver, and each poster becomes a person named `<client>/<id>`, allowed in the scopes that list it. The daemon logs one line saying the config uses the old form. To move, add `people`, name the owner by person, and replace each scope's `posters` by `people`:

```json
"owner": "1234",
"scopes": [{"key": "<channel id>", "client": "discord", "enabled": true, "posters": ["5678"]}]
```

becomes

```json
"owner": "ada",
"people": [
  {"name": "ada",      "ids": ["discord:1234"]},
  {"name": "dispatch", "ids": ["discord:5678"]}
],
"scopes": [{"key": "<channel id>", "client": "discord", "enabled": true, "people": ["dispatch"]}]
```

### `drivers.discord`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `bot_id` | string | required | The bot's user id. At start hataori checks that the token belongs to this bot and refuses to run as any other. |
| `token_file` | string | required | The name of a file in `<root>/config/` that holds the bot token. A plain file name: an absolute path or a path out of `config/` is refused. Spaces and line breaks around the token are ignored; an empty file is refused. |

Unknown keys are refused. The token is read from this file only, never from the environment. Keep the file readable by its owner only (`chmod 600`); hataori checks the mode of `hataori.json` but not of the token file.

### `adapters.claude-code`

The section may be empty (`{}`).

| Key | Type | Default | Meaning |
|---|---|---|---|
| `command` | string | `"claude"`, found on `PATH` | The Claude Code command to run. |
| `hataori` | string | this binary | The hataori binary that Claude Code's hooks run as `<hataori> hook`. |
| `hook_timeout` | duration | `"24h"` | How long a hook that waits for an answer (a question from the agent, a permission request) waits. At least one second. |
| `input_timeout` | duration | `"30s"` | How long input pasted into Claude Code may go unreported by its `UserPromptSubmit` hook before it counts as not delivered. At least one second. |
| `pass_env` | array of strings | empty | Names of variables in the daemon's environment to pass to Claude Code besides the fixed list below, for example an API key it logs in with. |

Unknown keys are refused.

## Environment variables

hataori reads these:

| Variable | Meaning |
|---|---|
| `HATAORI_ROOT` | The instance root when `--root` is not given. |
| `XDG_RUNTIME_DIR` | Used for the control socket when `<root>/state/control.sock` is too long for a Unix socket. Then the system temporary directory is tried. The chosen path is written into `<root>/state/control.sock.path`, where the commands find it. |
| `HATAORI_HOST_SOCKET` | Internal. hataori sets it for Claude Code so that `hataori hook` reaches its session. Do not set it yourself. |

Claude Code does not inherit the daemon's whole environment. It gets only `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TZ`, `TMPDIR`, `XDG_RUNTIME_DIR`, `XDG_CONFIG_HOME`, `CLAUDE_CONFIG_DIR`, the variables named in `pass_env`, and `TERM=xterm-256color`.

`git` gets the daemon's environment without the variables that would point it at another repository (`GIT_DIR`, `GIT_WORK_TREE`, `GIT_COMMON_DIR`, `GIT_INDEX_FILE`, `GIT_OBJECT_DIRECTORY`, `GIT_ALTERNATE_OBJECT_DIRECTORIES`, `GIT_NAMESPACE`, `GIT_CEILING_DIRECTORIES`), and with `GIT_TERMINAL_PROMPT=0`: a clone that would ask for a password fails instead. Credentials for private repositories must work without a prompt, for example through an SSH key or a credential helper.

## Commands and flags

```
hataori run     [--root DIR]
hataori threads [--root DIR]
hataori thread  [--root DIR] KEY
hataori events  [--root DIR] [--after N] [--follow]
hataori stop    [--root DIR] KEY
hataori version
```

| Command | Meaning |
|---|---|
| `run` | Runs the daemon in the foreground until SIGINT or SIGTERM. Logs go to standard error. Agent sessions keep running when the daemon stops, and the daemon picks them up when it starts again. |
| `threads` | Prints every thread, one JSON object per line. |
| `thread KEY` | Prints one thread and its state. Exits with 3 when no thread has that key. |
| `events` | Prints the event log, one JSON object per line. |
| `stop KEY` | Interrupts the thread's running turn. |
| `version` | Prints the version. |

| Flag | Default | Meaning |
|---|---|---|
| `--root DIR` | `$HATAORI_ROOT` | The instance root. Required by every command above except `version`. |
| `--after N` | `0` | `events` only: print the events after position `N`. |
| `--follow` | off | `events` only: keep printing new events as they happen. |

Flags may come before or after `KEY`. Exit codes: 0 success, 1 error (for example, no daemon runs for the root), 2 usage error, 3 no such thread.

`threads`, `thread`, `events` and `stop` talk to the running daemon over its control socket, and only the OS user that runs the daemon may use them. The format of what they print is in [protocol.md](protocol.md).

Two more commands are internal, run by hataori itself, and not for people: `hataori host <state-dir>` runs one agent session, and `hataori hook [--wait] [--timeout DURATION]` is the command Claude Code's hooks call.

## Orders

An order is a message in an enabled channel (not in a thread):

```
<trigger> repo:<url> [model:<model>] [effort:<effort>] <prompt>
```

- The options are `name:value` words on the first line, after the trigger word. The prompt is everything after the last option, and may span many lines.
- `repo:` is required. It is a URL with the scheme `https`, `http`, `ssh`, `git` or `file`, or the `user@host:path` form.
- `effort:` is one of `low`, `medium`, `high`, `xhigh`, `max`. Any other value gets a reply saying nothing was started.
- The author must be the owner or one of the scope's `people`. Posts by bot accounts are ignored; posts through a webhook are accepted when the webhook's id belongs to one of the scope's people.

hataori opens a thread on the message, clones the repository into the thread's working directory, starts the agent there and posts its replies into the thread. Later messages in that thread from the owner or one of the scope's people go to the same agent session. An order that is refused for any reason other than the effort value starts nothing and writes one log line.
