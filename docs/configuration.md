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
  trash/              created by hataori: removed workspaces, kept for the retention
```

- The root is given by `--root DIR`, or by the environment variable `HATAORI_ROOT` when `--root` is not given. Every command except `version`, `host` and `hook` needs one.
- `hataori run` creates `state/`, `workspaces/` and `mirrors/` (mode 0700) if they are missing, and holds a lock in `state/`. A second daemon on the same root refuses to start.
- A thread's working directory is `workspaces/<number>/`, by the thread's number. A thread started before thread numbers keeps the directory it was made in, `workspaces/<client>/<scope>/<thread>/`; hataori reads each thread's directory from its database and never moves one.
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
| `terminal_size` | object | absent | The size of the terminal an agent session runs in, for an agent that runs in one (Claude Code): `{"cols": 200, "rows": 50}`. Give both keys: `cols` from 40 to 500 and `rows` from 10 to 200; anything else is refused at start. Each session the daemon starts or resumes takes it, so a change applies to a thread's next session; a running session keeps its size. Without this key a session's terminal is 80 columns by 24 rows. |
| `host_terminals` | boolean | `false` | With `true`, the owner may list the terminals on the host that hataori does not own (tmux panes and other terminals) and read the screens of the panes of the default tmux server, for display only, through the control API's `HostTerminals` and `HostScreen`. hataori never sends these terminals anything. With `false`, both are refused for everyone. |
| `dashboard` | object | absent | The web dashboard (below). Without this section the daemon serves none. |
| `cleanup` | object | absent | How workspaces are removed (below). Without this section no ignored file is disposable and trash is kept for seven days. |

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

### `dashboard`

The web dashboard ([design, section 7.2](design/overview.md#72-dashboard)). It serves the page, the thread list, one thread, the event stream, each session's live terminal and, with `host_terminals` on, the screens of tmux panes, read-only, to a signed-in session. It has up to two listeners, one for each way of reaching it, and each knows who is at the other end in its own way:

- **The loopback listener** (`listen`) serves a browser on the host and one on another machine through a port forwarded over SSH. It is plain HTTP on a loopback address only. Logging in to the host by SSH as the user hataori runs as is the proof: the sign-in link is printed on the host.
- **The web listener** (`web`) serves a phone or a machine without SSH, over HTTPS with a stable name: TLS of its own, or a reverse proxy in front. An address that is neither loopback nor served over HTTPS gets no dashboard: the daemon does not start.

The section may be empty (`{}`): the loopback listener then listens on `127.0.0.1:7466`. Leave `listen` out and give `web` for the web listener alone; give both for both.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `listen` | string | `"127.0.0.1:7466"`, unless `web` is given | The loopback listener's host and port. An address that is not loopback is refused, and the daemon does not start. |
| `web` | object | absent | The web listener (below). |

`web`:

| Key | Type | Default | Meaning |
|---|---|---|---|
| `listen` | string | required | The host and port to listen on. An address that is not loopback needs `tls_cert` and `tls_key`, also behind a proxy. |
| `url` | string | made from `listen` | The address people open, such as `"https://dash.example.com"`: `https`, a host, nothing else. Its sign-in links start with it, and a request whose `Host` or `Origin` is any other address is refused. Required with `proxy`, or when `listen` names every address. |
| `tls_cert` | string | empty | The name of a PEM certificate file in `<root>/config/`. With `tls_key`, the listener serves HTTPS itself. |
| `tls_key` | string | empty | The name of its PEM private key file in `<root>/config/`. |
| `proxy` | string | empty | The IP address of a reverse proxy in front that serves the dashboard over HTTPS at `url`. Connections from any other address are refused. The proxy must pass the `Host` it was asked for. Without `tls_cert` and `tls_key` the proxy must run on this machine, with `listen` on loopback, so that links and sessions never cross the network in the clear. |

One of `tls_cert` and `proxy` is required.

**Signing in.** `hataori signin` on the host prints a link for the loopback listener, and `hataori signin --web` one for the web listener. A link works once, within five minutes, and only on its own listener; the session it opens works only there too, and belongs to the person the link was made for: it shows only the scopes that person may act in, every scope for the owner. Only the owner may get a link, on the host or, once the chat command exists, in chat; a session ends early when its person may no longer get one. A session lasts seven days on the loopback listener and twelve hours on the web listener, across restarts of the daemon. Sessions are signed with a key the daemon keeps in `state/dashboard-session.key`; removing that file ends every session at the next start.

There is no session cookie: a browser sends a cookie to every port of its host, so another service on the same machine could receive it. The page keeps its session in its own origin's storage and sends it in a header with every request. Sessions made by a version that used a cookie are not recognised: sign in again. The page's own files are served without a session; every other route needs one. Only `GET` and `HEAD` are served.

A link's token is in the part after `#`, which a browser does not send: the page trades it for the session with a request of its own. So a chat app or a link checker that fetches the link, for a preview, uses nothing up.

The loopback listener refuses a request that came through a reverse proxy (one that carries `Forwarded`, `X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto`, `X-Real-IP` or `Via`): put a proxy in front of the web listener instead.

**On another machine, over SSH, by hand.** Forward a local port to the loopback listener, with the local address given so that your `GatewayPorts` setting cannot open the port to the network:

```sh
ssh -L 127.0.0.1:7466:127.0.0.1:7466 user@host
```

Then run `hataori signin` there, and open the link it prints on your machine with the host and port changed to the local ones, such as `http://localhost:7466/signin#t=...`. The local port may differ from the daemon's: the loopback listener answers `localhost`, `127.0.0.1` and `[::1]` with any port. Browsers treat `http://localhost` as a secure origin, so no certificate or name is needed. Each local port keeps its own session, so two instances forwarded to two ports stay apart.

**The web listener through a VPN or a tunnel**, as one example. A product that gives this machine an HTTPS address with a stable name inside a private network, such as a mesh VPN's built-in HTTPS proxy, can be the reverse proxy: set `url` to the name it serves and `proxy` to the address it connects from, with `listen` on loopback.

```json
"dashboard": {
  "listen": "127.0.0.1:7466",
  "web": {"listen": "127.0.0.1:7467", "url": "https://dash.vpn.example", "proxy": "127.0.0.1"}
}
```

hataori does not require or detect such a product; any reverse proxy that serves HTTPS and passes the `Host` works the same way.

**The old form.** A section with `url`, `tls_cert`, `tls_key` or `proxy` directly in it, from before the web listener, is read as one listener: with `tls_cert` or `proxy` it is the web listener and there is no loopback listener; otherwise it is the loopback listener, whose links start with `url`. The daemon logs one line saying the config uses the old form. Move those keys into `web` to have both listeners.

### `cleanup`

A workspace is removed only when nothing in it exists nowhere else ([design, section 4.6](design/overview.md#46-cleanup-without-losing-work)), and removal is a rename into `<root>/trash/` on the same file system. The removal is refused, naming the reason, when any of these holds or cannot be told: a modified, staged or untracked file; a file marked assume-unchanged or skip-worktree; an ignored file that no `disposable` pattern matches; a linked worktree; a commit reachable from a ref, `HEAD`, the stash or a reflog that no remote has when it is asked at that moment (remote-tracking refs are not trusted); the same in every submodule, also one with no checkout, and in every nested repository; a process using the directory. A remote in `workspaces/`, or under this root or one of `other_roots`, does not count as a remote, nor does a local address that cannot be resolved the way git resolves it (symlinks, `..`, `~/`, a missing `.git`, a `.git` file, a worktree's `commondir`), or that `insteadOf` would rewrite again. A local remote whose `objects/info/alternates`, or a link below its objects directory that git reads through (`pack/`, the fan-out directories), lead into a root or cannot be followed, does not count either.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `disposable` | array of strings | empty | Patterns of ignored files that are not work, such as `"node_modules"` or `"*.pyc"`. A pattern without a slash matches any path segment, so it covers everything under a directory of that name; a pattern with a slash matches the path from the top of the repository, or a leading part of it (`"build/out"` covers everything under `build/out`). Patterns use Go's `path.Match` syntax (`*`, `?`, `[...]`); `**` is not special. |
| `trash_retention` | duration | `"168h"` | How long a removed workspace stays in `trash/` before it is deleted. A positive Go duration. Only entries the removal put there are ever deleted. |
| `other_roots` | array of strings | empty | Absolute paths of the roots of other hataori instances on this host. A remote under one of them does not count. |
| `remove_closed_after` | duration | absent: off | Turns on the automatic removal pass: the workspace of a thread that has been closed for longer than this is removed, with the same proof as `hataori remove`. A positive Go duration, such as `"720h"`. A refused removal leaves the thread closed, writes one `removal_refused` event, and is tried again only after the same duration. |

A workspace is removed only when its thread is closed: `hataori close NUMBER` closes it, and `hataori remove NUMBER` removes it (see "Commands and flags" below). Nothing in chat removes a workspace, and no thread is closed by its idle age.

Processes are found with `lsof`, on Linux and on macOS alike; without `lsof` every removal is refused. Unless hataori runs as root, `lsof` sees only the processes of the user hataori runs as.

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
hataori thread  [--root DIR] NUMBER
hataori events  [--root DIR] [--after N] [--follow]
hataori stop    [--root DIR] NUMBER
hataori attach  [--root DIR] NUMBER
hataori close   [--root DIR] NUMBER
hataori remove  [--root DIR] NUMBER
hataori signin  [--root DIR] [--web]
hataori version
```

| Command | Meaning |
|---|---|
| `run` | Runs the daemon in the foreground until SIGINT or SIGTERM. Logs go to standard error. Agent sessions keep running when the daemon stops, and the daemon picks them up when it starts again. |
| `threads` | Prints every thread, one JSON object per line: its number (`thread`), its scope, its links (the chat conversations it is shown in), its state, title and repository. |
| `thread NUMBER` | Prints one thread and its state, by its number, with or without `#` (`7` or `#7`). Exits with 3 when no thread has that number, and with 2 when the argument is not a number. |
| `events` | Prints the event log, one JSON object per line. |
| `stop NUMBER` | Interrupts the running turn of the thread with that number, as `thread` takes it. |
| `attach NUMBER` | Connects this terminal to the terminal of the thread's agent, on the host, for the owner. It shows the current screen, then passes every key to the agent; Ctrl-\ detaches and leaves the agent running. An asleep thread is woken first; a closed thread, and an agent without a terminal, are refused with a message. While attached the thread is not put to sleep, and a daemon restart does not end the attachment. A turn typed there is posted in chat after a line "(typed at the terminal)". Chat input is still delivered while you are attached, into the same input line: a chat message that arrives while you have typed part of a line is mixed with it, so finish or clear the line before you stop typing. The current screen is replayed from the end of the terminal's log and is exact only once the agent draws again. Output is the agent's terminal, not JSON. |
| `close NUMBER` | Ends the agent process of an idle, asleep or failed thread and closes the thread; its workspace stays. A message in a closed thread starts nothing and gets one line saying the thread is closed. A thread with a turn running, a question open, or background tasks left running by its last turn is refused: stop it first. Prints `{"thread", "scope", "state": "closed"}`. |
| `remove NUMBER` | Moves the workspace of a closed thread into `<root>/trash/`, only when nothing in it would be lost (see `cleanup` above), and the thread becomes `removed`. Otherwise it changes nothing, prints the reason on standard error and exits with 1. Prints `{"thread", "scope", "state": "removed"}`. |
| `signin` | Prints `{"link": ...}`: a link that signs the owner in to the dashboard's loopback listener, or with `--web` its web listener, once, within five minutes. Fails when the config has no `dashboard`, or when it has no such listener. |
| `version` | Prints the version. |

| Flag | Default | Meaning |
|---|---|---|
| `--root DIR` | `$HATAORI_ROOT` | The instance root. Required by every command above except `version`. |
| `--after N` | `0` | `events` only: print the events after position `N`. |
| `--follow` | off | `events` only: keep printing new events as they happen. |
| `--web` | off | `signin` only: a link for the dashboard's web listener instead of its loopback listener. |

Flags may come before or after `NUMBER`. Exit codes: 0 success, 1 error (for example, no daemon runs for the root), 2 usage error, 3 no such thread.

`threads`, `thread`, `events`, `stop`, `close`, `remove` and `signin` talk to the running daemon over its control socket, and only the OS user that runs the daemon may use them. The format of what they print is in [protocol.md](protocol.md).

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
