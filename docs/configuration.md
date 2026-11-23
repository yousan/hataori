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
  state/              created by hataori: database, lock, sessions, control socket,
                      the dashboard's session key and passkeys
  workspaces/         created by hataori: one clone per thread
  confined/           created by hataori: one directory per scope with a supervisor
  mirrors/            created by hataori: one bare mirror per repository
  trash/              created by hataori: removed workspaces, kept for the retention
```

- The root is given by `--root DIR`, or by the environment variable `HATAORI_ROOT` when `--root` is not given. Every command except `version`, `host` and `hook` needs one.
- `hataori run` creates `state/`, `workspaces/` and `mirrors/` (mode 0700) if they are missing, and holds a lock in `state/`. A second daemon on the same root refuses to start.
- A supervisor's thread (`scopes[].supervisor`) has no clone: its working directory is its scope's directory in `confined/`, outside `workspaces/`, which every supervisor thread of the scope works in, one after another. A scope's first supervisor thread gets a new, empty one, named from the scope's key: `scope-<key>` for a key of lowercase letters, digits, `-` and `_`, `scopex-<the key in hex>` for any other, and `scopeh-<its SHA-256 in hex>` for a key over 100 bytes. A scope whose supervisor thread existed before hataori kept a directory per scope keeps that thread's directory, `confined/<number>/`, with everything in it. hataori makes the directory empty only when it makes it; one that is there is used as it is, never emptied, moved or written to by hataori. Nothing in hataori removes it, `hataori remove` of any of the scope's supervisor threads included: the removal is refused.
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
  "terminal_size": {"cols": 160, "rows": 60},
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
| `agent` | string | the only adapter | The agent of new threads whose order and scope name none: a name under `adapters`. Required when `adapters` names more than one; refused at start when it names an adapter `adapters` does not configure. |
| `model` | string | empty | The model of a thread of the default agent (`agent`) whose order, scope and adapter section name none. Empty leaves it to the agent's own default. |
| `effort` | string | empty | The effort of a thread of the default agent (`agent`) whose order, scope and adapter section name none. Empty leaves it to the agent's own default. |
| `context_bytes` | integer | `8192` | When the agent no longer has a thread's conversation and a new session replaces it, the thread's earlier inputs and final answers are given to the new session, cut to this many bytes. Must be greater than 0. |
| `login_relay` | boolean | `true` | When turns fail because the agent is logged out, the owner gets one direct message about it. With `true`, hataori also runs the agent's login command, sends the owner the sign-in link, and passes the code the owner replies with back to it, or, for a login that takes no code back (Codex), shows the code to enter on the page. `false` keeps the alert and switches the relay off. |
| `scopes` | array | empty | The channels hataori serves, one object each (below). A channel that is not listed, or not enabled, starts nothing. |
| `drivers` | object | empty | One section per chat surface, by name. This binary has one: `discord` (below). A name it does not have is refused at start. |
| `forges` | object | empty | One section per forge, by name. This binary has one: `github` (below). A name this binary does not have is refused at start. |
| `adapters` | object | empty | One section per coding agent, by name. This binary has two: `claude-code` and `codex` (below). One or more adapters must be named; with more than one, `agent` names the default. |
| `terminal_size` | object | absent | The size of the terminal an agent session runs in, for an agent that runs in one (Claude Code): `{"cols": 160, "rows": 60}`, which suits a wide dashboard pane. Give both keys: `cols` from 40 to 500 and `rows` from 10 to 200; anything else is refused at start. Each session the daemon starts or resumes takes it, so a change applies to a thread's next session; a running session keeps its size, except that while its owner types into it from the dashboard it takes the pane's size, within these bounds, and keeps it afterwards (design, section 7.2). Without this key a session's terminal is 80 columns by 24 rows. |
| `host_terminals` | boolean | `false` | With `true`, the owner may list the terminals on the host that hataori does not own (tmux panes and other terminals) and read the screens of the panes of the default tmux server, for display only, through the control API's `HostTerminals` and `HostScreen`. hataori never sends these terminals anything, unless `host_input` is on. With `true`, the daemon also takes the reports of hataori's Claude Code plugin, on a socket in `state/windows/` that only the instance's OS user can open (when that path is too long for a Unix socket, in a directory `/tmp/hataori-windows-<hash>` of the root's path, which must also be that user's alone; if it is not, the daemon logs a warning and lists the panes without these reports): a tmux pane in which a Claude Code session a person started runs then shows that session's id, directory and last hook event. `hataori plugin` writes the plugin into `claude-plugin/` under the root and prints the two `claude plugin` commands that install it; it writes nothing into Claude Code's configuration. The reports are kept in memory only and hold no text of the session. With `false`, both are refused for everyone, and the plugin's hooks find no daemon and do nothing. |
| `host_input` | boolean | `false` | With `true`, the owner may type into a tmux pane on the host that hataori does not own, through the control API's `OperateHost` (design, section 7.2). It needs `host_terminals`: `true` without it is refused at start, naming both keys. hataori's own session terminals, and a pane in which one of hataori's processes runs, are never typed into. Keys go by `tmux send-keys`, and text through a tmux buffer of the operation's own, never on a command line; no tmux client is attached, so no window changes size. Each start and end is logged with the person and the pane; what is typed is not. At start the daemon deletes the tmux buffers an earlier run of the same instance left; when tmux answers that with an error, the daemon does not start. The dashboard's tab of a pane switches to typing on the loopback listener at once, and on the web listener only after a passkey check made for it, or, for a person with no passkey, within five minutes of signing in with a link, after the daemon's start and after the person last removed a passkey. With `false`, `OperateHost` is refused for everyone, and the dashboard's operate connection refuses at once. |
| `host_input_idle` | duration | `"5m"` | How long an operation of a host pane lasts without a key before it ends: a whole number of seconds, at least one, such as `"90s"`. |
| `holder` | string | `"host"` | What holds the terminal of the agents hataori starts in one ([design, section 4.3](design/overview.md#43-the-session-host-and-terminal-holders)). `"host"` is hataori's own session host. With `"tmux"`, each such session runs in a window of a tmux session of hataori's own name on the user's default tmux server (`hataori-<instance>-<session>`), which the person can switch to; what they type there is reported as typed at the terminal. hataori's own host still relays the session's hook calls, and starts first. The agent is started by `respawn-pane` with `hataori exec`, which reads the agent's arguments and environment from the session's state directory, so no value of the environment is on a command line; its raw output goes through `pipe-pane -O` to `hataori pipe`, which keeps the bounded log of the agent's terminal. Stopping the session ends the agent's process group as the own host does, records how it ended from `#{pane_dead_status}`, and removes the window. An agent on pipes, such as Codex, always runs under hataori's own host. Any other value is refused at start. |
| `dashboard` | object | absent | The web dashboard (below). Without this section the daemon serves none. |
| `cleanup` | object | absent | How workspaces are removed (below). Without this section no ignored file is disposable and trash is kept for seven days. |
| `plugins` | array | empty | The plugins of the instance, one object each (below). Each is checked against its manifest when the daemon starts, and an error stops the start. Then hataori runs them ([plugins.md, How a plugin runs](plugins.md#how-a-plugin-runs)). |

A new thread uses the order's `agent:`, then the scope's `agent`, then the top-level `agent`. It keeps that agent when waking, resuming after a restart, or starting a new session after the agent cannot resume, even after a config change. Removing its adapter section leaves the thread unable to start.

Model and effort are chosen in this order: the order's `model:` / `effort:`, the scope's when the thread runs the scope's agent, the agent's adapter section, the top-level key for the default agent, then the agent's own default.

Names in `drivers`, `adapters` and `scopes[].client` start with a lowercase letter and contain only lowercase letters, digits and `-`.

### `scopes[]`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `key` | string | required | The scope's id. For Discord, the channel id. Each key may be listed once. |
| `client` | string | required | The driver that owns the scope: `"discord"`. It must also be configured under `drivers`. |
| `name` | string | empty | What the dashboard calls the scope. Without it, the dashboard shows the channel's name as the chat service shows it (for Discord, `#name`, looked up each time the driver connects), else the key. |
| `enabled` | boolean | `false` | Whether orders in this scope start threads. |
| `agent` | string | the default agent | The agent of new threads started here whose order names none: a name under `adapters`. Refused at start when it names an adapter `adapters` does not configure. |
| `model` | string | empty | The model of threads started here that run the scope's agent and whose order names none. |
| `effort` | string | empty | The effort of threads started here that run the scope's agent and whose order names none. |
| `people` | array of strings | empty | Names of the people, under `people`, who may start threads here and post into them besides the owner. Empty strings are ignored. |
| `show_in` | array of strings | empty | Other channels where this scope's threads may be shown, each as `<client>:<channel>`, such as `"discord:<channel id>"` (for another client, its name under `drivers`). A person allowed in this scope attaches a thread there with `/attach` (see "Showing a thread in another channel"). Without an entry, an attach in that channel makes no link, shows nothing there and writes one log line. The scope's own channel is never listed. |
| `trust_files` | array of strings | empty | Files that the folder-trust dialog of a new workspace may say trusting the folder loads, as it writes them, such as `".claude/settings.json"`, `".claude/settings.local.json"` or `".mcp.json"`. hataori answers the dialog yes for this scope's threads only when the list holds every file the dialog names: a repository whose project settings run commands then runs them. Without an entry, such a thread fails at any start that shows that dialog (the first in a new workspace) as `blocked at start`, and the chat thread gets a "This thread could not start" message, which the dashboard also shows. It says in plain words why hataori stopped: it does not answer that dialog for a person when it names files, since trusting the folder lets the repository's settings run commands without asking, and it stops for any such file without judging the repository. It quotes the dialog as Claude Code drew it, with the files it names, and says what to do next: on a chat service with buttons it offers "Start without this repository's settings" (every launch of the thread leaves the project's and the local settings files out, so their hooks never run; anyone who may start a thread here may choose it), "Trust and start" (the owner only: it trusts exactly the files named for this scope's threads on that repository, kept in the instance's database, not in this file, and taken with this list at every later launch; `hataori trusts` and the dashboard list them, and `hataori untrust NUMBER` withdraws one) and "Do not start" (the thread is closed); it also names this list with its scope, which is all it says without buttons. The dialog that names no file is answered as before, and a supervisor's session, which ignores those files, gets it answered whatever it names (design, section 3.5). Each launch takes the list from the config the daemon loaded. |
| `posters` | array of strings | empty | The old form only: ids, on this scope's client, of who may post here besides the owner. For Discord, user ids, or the id of a webhook for posts made through that webhook. Refused in a config with `people`. |
| `supervisor` | object | absent | The scope's supervisor (below). Without it the scope has none, and nothing changes. |
| `archive_when_asleep` | boolean | `false` | Whether the chat thread of each of this scope's threads is archived when the thread goes asleep or is closed, so that a long-running instance's chat client and the dashboard's tree, which mirrors it, show the threads that are awake. It applies on a chat service that can archive threads (Discord); on one that cannot, the threads stay open. Archiving never locks the chat thread and deletes nothing. A new message in it un-archives it, as the chat service does for any message, and resumes the thread as before, also when it was written while hataori was down. A thread woken from the dashboard or by `hataori attach` has its chat thread un-archived first. An archive that fails is logged once, and nothing else changes. Only threads that go asleep or close after the option is on are archived. |

### `scopes[].supervisor`

A supervisor is one more thread of the scope, started by the daemon (design, section 7.1). Its agent has no shell, file tools only in its scope's directory, and as its only other tools the control API of its own scope and read-only reads of its repositories on a forge, through `hataori mcp`. The section may be empty (`{}`).

| Key | Type | Default | Meaning |
|---|---|---|---|
| `agent` | string | the default agent (`agent`) | The adapter that runs it. It must be the default agent (`agent`); the scope's `agent` does not apply. Only Claude Code can start a supervisor today: with another adapter its thread fails, with that reason, and no agent process starts. |
| `model` | string | the adapter's cheapest | Its model. Claude Code's cheapest is `haiku`. If the adapter names no cheapest model, the default agent's model order above applies: the scope's model when it runs the default agent, then the adapter section, then the top-level key. |
| `effort` | string | the default agent's effort | Its effort follows the default agent's effort order above: the scope's effort when it runs the default agent, then the adapter section, then the top-level key. |
| `instructions` | string | empty | The config's instructions: given at each start of its session, a resume included, at the end of the config's layer of its instructions (below, *Its instructions*). |
| `quiet` | duration | `"30s"` | The quiet period: after the first turn end that wakes the supervisor, how long the waker waits, folding what comes until then into one message. |
| `output_max` | integer | `1024` | The most bytes of each thread's output in a wake message, keeping the end. |
| `threads_max` | integer | `20` | The most threads a wake message shows; the others are counted in one line. |
| `turns_per_hour` | integer | `12` | Its budget: the most turns its thread runs in a clock hour before it is no longer woken. `0` turns the limit off. |
| `sends_per_hour` | integer | `20` | Its budget: the most sends and starts it makes in a clock hour. Past it they are refused, and it is no longer woken. A proposal (`autonomy` `"propose"`) counts once, when it is made; what it runs once approved is not counted again. `0` turns the limit off. |
| `tokens_per_day` | integer | `2000000` | Its budget: the most tokens its turns use in a day, counted as the thread's tokens today are. `0` turns the limit off. |
| `loop_cycles` | integer | `3` | Its loop guard: after this many rounds with the supervisor and no person's message in between, a thread no longer wakes it. A round is a send or an answer of the supervisor that starts a turn in the thread, that turn's end, and the wake that tells it of it. `0` turns the guard off. |
| `repos` | array of strings | empty | The repositories it works on, each written as an order's `repo:` names it, such as `"https://github.com/<owner>/<name>"`; an address an order would refuse is refused at start. Its `start_thread` starts a thread only in one of them (below, *Its starts*). Without the key it starts no thread. |
| `autonomy` | string | `"propose"` | What it does on its own: `"watch"`, `"propose"` or `"act"` (below, "Its autonomy"). Any other value is refused at start. A config without the key gets `"propose"`: a scope whose supervisor should go on sending and starting on its own, as before the key existed, needs `"act"`. |
| `report` | string | `"every"` | Which of a wake's final answers its thread posts (below, *What a wake posts*). `"every"`: each one that is not `(quiet)`. `"needed"`: only one that names a person with `@<name>`; the others stay in the thread's events, which the dashboard and `hataori thread` show. `"digest"`: one that names a person at once, and the others together, as one message, once per `report_every`. A turn that a person started is posted whatever this says. |
| `report_every` | duration | `"1h"` | Under `"digest"`, how long after the first answer it holds a digest is posted, with every answer held until then. The held answers are kept in the store, so a restart neither loses nor repeats them. |
| `mention` | string | `"named"` | `"named"`: `@<name>` of a person of the scope mentions them (below, *Who it mentions*). `"never"`: no post of its thread mentions anyone, the pings of a failed turn or of a question included; `@<name>` stays text. |
| `quiet_hours` | object | absent | `{"from": "HH:MM", "to": "HH:MM", "zone": "<time zone name>"}`, `zone` optional (the daemon's time zone): a daily span during which no post of its thread mentions anyone, the pings included. A span across midnight, such as `22:00` to `07:00`, works; `from` equal to `to` is refused. When it ends, one message mentions each person who was named during it, with how many times. Posts go on as `report` says. |
| `trusted_authors` | array of strings | empty | Logins on a forge whose text its forge tools give it although they have no write access to the repository (below, *Its forge tools*). A login's case does not matter. An empty entry, or one with a space, a `/` or an `@`, is refused at start. |
| `files` | array of strings | empty | Files on the host that the supervisor reads, such as a design or a project's rules, each an absolute path. Before each launch of its session (start and resume) and before each wake, the daemon copies each into `docs/` of the scope's directory under its base name, readable and not writable (below, "Its docs"). Two entries with the same base name, also when told apart by case only, are refused at start, naming both. Without the key nothing is copied and no `docs/` is made. |

- **Its thread.** When the daemon starts, it opens the supervisor's thread in the scope's own channel, as the owner, with the title "Supervisor of this channel" and a first message that asks the agent to say it is ready. After a restart it finds that thread again and starts no second one. People write to it there like in any thread. A scope that is not enabled gets no supervisor thread.
- **Its posts.** Its thread is a conversation: the reply is the answer. A person's message there gets its lamps, and no `Done.` ping when the turn completes. A turn that fails, or that ends waiting for an answer, still pings as in any thread: its sender once, and nobody for a turn a person stopped or a post through a webhook, under `mention` `"never"` or during `quiet_hours`. Worker threads ping as before.
- **What a wake posts.** A turn that a wake started, a reminder's included, posts its final answer only, not the text before it, and posts nothing when that final answer, trimmed, is exactly `(quiet)`; the rest of its text stays in the thread's events, which the dashboard and `hataori thread` show. A question it asks and a file it hands over are posted as in any thread, since they need a person. A turn that a person started is posted whole, `(quiet)` or not. Which of the other final answers are posted, and when, is `report`'s.
- **Who it mentions.** `@<name>` in its posts, where `<name>` is the name of a person who may act in the scope (the owner, or one in its `people`), mentions that person by their ids on the chat service it is posted to. `@<name>` of a person not allowed in the scope, of a name no person has, or right after a letter or digit (as in `x@name`) stays text. Where names overlap, the longest that fits is taken: `@ann.smith` names `ann.smith`, not also `ann`. When the thread is shown in more than one conversation, the person is mentioned in one of them: where the post goes first. Nothing else the supervisor writes mentions anyone; the pings above (a failed turn, a question) are the chat front end's, as in any thread. Worker threads mention nobody this way. Under `mention` `"never"` and during `quiet_hours` nobody is mentioned (see the table).
- **Its keys are checked at start.** A value of `report`, `report_every`, `mention` or `quiet_hours` other than those above stops the daemon at start, with an error that names the key.
- **A new one.** A supervisor's thread that failed, as when its agent could not start, stays failed, also across restarts; the daemon logs it at each start. Close it (`hataori close NUMBER`), and the next start of the daemon opens a new one. The same holds for a supervisor's thread the owner closed for any other reason. Until then, `hataori reopen NUMBER` (or the Reopen button in the old thread) takes it back instead; once a newer supervisor thread is open, a reopen of an older one is refused with a message naming the open one. To have no supervisor, remove the section instead.
- **Its directory.** Every supervisor thread of the scope works in the scope's directory (see "The instance root"), which outlives each of them: a new one finds what the ones before it wrote there. `notes/` there is for the supervisor's notes. The daemon logs the directory once, when it first records it for the scope. Removing the section, or every supervisor thread of the scope, leaves the directory as it is; a section added again later uses it again.
- **Its instructions.** Each start of its session, a resume included, appends to the agent's own instructions three layers, in this order: hataori's layer, built in and the same for every scope; the config's layer, which states the keys of this section the daemon acts on as the loaded config sets them (`autonomy`; `repos`, each by its forge name, or its address without the user and password of a URL; `turns_per_hour`, `sends_per_hour` and `tokens_per_day`; `report`, with `report_every` under `"digest"`; `mention`; `quiet_hours` when set), then `instructions`; then the user's files. An earlier layer wins over a later one, and nothing in any of them grants a tool, a repository or more autonomy: the daemon checks those on the supervisor's connection, whatever the instructions say.
  - The user's files are `AGENTS.md` (how to work here: orders, repositories, who deploys, what to check), `SOUL.md` (tone, and how to decide), `USER.md` (the people and their preferences: language, time zone, how much they want to hear) and `IDENTITY.md` (its name), at the top level of the scope's directory, each optional. They are given in that order, each under a heading with its name. Each is read as text: nothing in it is followed, so a line such as `@../other.md` is given as it is and the file it names is not read.
  - A file that is a symbolic link, not a regular file (such as a directory), or larger than 32 KB is not given, and the daemon logs one line naming it and why at that start; the start goes on with the others.
  - The supervisor may write these files: that is how a person in its thread tunes how it responds. A write to a path with a part that starts with a dot is still refused.
  - Each wake says which of the files changed since its session was last given them or told of them, so that it reads them again; a change made between two wakes is named in the next one only. After each turn of the supervisor's thread, one line there names each of the files that changed by the end of the turn, posted once, also across restarts. A file edited on the host before a wake is named by that wake, not after its turn; one edited before a person's message in the supervisor's thread is named after that turn.
  - Claude Code's session loads none of them by itself: it loads no CLAUDE.md files, and does not look for these.
- **Its docs.** `docs/` in its directory is hataori's. The copies there are made at each launch and each wake, so the supervisor reads a listed file as it was then; a change between two wakes reaches it at the next one. A listed path that is missing, not a regular file (a link to one is followed), or larger than 1 MB is skipped with one log line, and the launch or the wake goes on; a copy made of it before stays as it was. The daemon writes over or removes only a file there that it wrote itself and that has not changed since: it keeps each copy's name, size, SHA-256 and inode in its database, keeps a second link of its own to each copy in `docs/`, named `.hataori-` and some letters, so that no other file gets its inode, and moves a copy aside to check it again before it writes over or removes it. A daemon that dies while it copies, or a file it could not put back after it moved it aside (logged), can leave one more `.hataori-` file in `docs/`, which stays there. Any other file there, a file of the same content that someone else put there included, and a copy someone changed, made writable, or whose second link was removed, is left as it is, with one log line, and the listed file of that name is not copied; a copy whose path is no longer listed is removed under the same rule. So copies an operator put in `docs/` by hand stay, and keep the daemon's copies of the same names out: remove them once to let the daemon's copies in. The session may not write into `docs/`, with or without `files`: its permission hook refuses that as it refuses a path with a part that starts with a dot, also when a link in its directory leads there. The scope's `AGENTS.md` there can point at these files.
- **How it is woken.** It never polls. When a thread of the scope, other than its own, ends a turn (completed, blocked on a question, or failed, a usage limit included), the daemon waits for the quiet period and then sends it one message: each such thread once, by number, with its title, how its turn ended, its background tasks and the end of its output since the last message that gave it (or one line saying there is none). The first message that names a thread, and the first one to a new session of the supervisor, give the end of its last output instead. Turn ends during the quiet period, and while its own turn runs, go into the next message; a message is never sent into its running turn. The daemon keeps its place in the event log, so a restart wakes it once for each turn end, not twice and not never. Turn ends from before the section was added wake nothing. A turn that completes with background tasks still running, such as a watch on CI, is not the end of the thread's work and wakes nothing: the thread's next turn end wakes it, and if the thread starts no turn within an hour, it is woken once for it, with a line saying that the background work has not come back. Such a turn end spends no turn of its budget and no round of its loop guard. A turn end that is blocked on a question or failed wakes it at once, background tasks or not. This is read from the event log, so a restart neither loses nor repeats such a wake.
- **Its budget.** Hours and days are those of the daemon's clock, in its time zone. Turns started by a person's message in its thread count too, but are never held back. When a limit is reached, the turn ends wait until the hour or the day is over, then wake it once. The thread gets one line saying what is used up and until when, once, also across restarts. The counts are read from the event log, so a restart keeps them.
- **Its loop guard.** When the supervisor and a thread keep answering each other, the thread stops waking it at `loop_cycles`, and the supervisor's thread gets one line naming it. A person's message in that thread, a turn typed at its terminal, or a person's message in the supervisor's thread, a decision of one of its proposals or interrupt requests included, lets it wake the supervisor again. Other threads are not affected.
- **Its starts.** A start of its tools names a repository of `repos` by the address listed there, or by the entry's forge name: `<owner>/<name>`, the path of an address with a host (`https`, `http`, `ssh`, `git`, or `[user@]host:path` with a path that does not start with `/`) when that path has two parts, without a trailing `.git`; its case does not matter. A forge name is replaced by the entry's address, which is what is cloned. Any other repository is refused as not allowed, with a message that names the scope, nothing is cloned, and the daemon logs one `control call refused` line without the repository. An entry with no forge name, such as a `file://` address, can be started but not read on a forge. Two entries with the same forge name are refused when the daemon starts, since the forge tools name a repository by it. A scope with no `repos` gets no start from its supervisor, and the daemon logs one line at start saying so. This is checked by the daemon on the supervisor's own connection, whatever its tools send; a start by the owner or by a person of the scope is not limited by it. Under `autonomy` `"propose"` it is checked when the start is proposed, and again, against `repos` as they are then, when a person's approval makes it: a repository the scope no longer lists is not cloned, and the outcome says it was not allowed. `list_threads` and `read_thread` show each thread's repository, without the user and password of a URL, its model and its effort.
- **Its forge tools.** `list_issues`, `read_issue`, `list_pulls`, `read_pull` and `read_checks` read, read only, the issues (open, closed or all), one issue with its comments, the pull requests, one pull request with its reviews, comments, changed files, merge state, merge commit and the checks of its head, and the checks and commit statuses of a branch, a tag or a commit, of a repository of `repos`, named by its forge name or by its address as listed. The entry must have a forge name, and a forge of `forges` must serve its address (for `github`, an address on `github.com`); any other repository is refused as not allowed, with a message that names the scope, nothing is read, and the daemon logs one `control call refused` line without the repository. The daemon makes the request with the forge's token: the token is in no session's environment or MCP config, and in no event or log line. Text that someone without write access to the repository wrote (an issue's or a pull request's title and body, a pull request's branch, a comment, a review) is left out of the answer, which names the author and says that the text was withheld, unless `trusted_authors` lists the login; in such a pull request the paths of its changed files and of the comments on them, and the names of its head's checks, are left out too, since its author wrote the files and the workflows that run on them. `read_checks` gives the names of the checks of a branch or a tag, which someone with write access put there, and withholds them, with their addresses, for a commit (4 to 40 hexadecimal digits) and for a pull request's ref (any ref with a part `pull`), since a stranger's pull request may have made that commit; a person with write access is the forge's to say (for `github`, the `author_association` `OWNER`, `MEMBER` or `COLLABORATOR`). No CI log is read. An answer longer than 16 KB is cut, with a last line that says so. A forge's refusal or rate limit is a tool error with the forge's reason. The tools write nothing: hataori has no code that writes to a forge.
- **Is it in the running instance.** Its `deployed` tool takes a pull request's number, or a commit (7 to 40 hexadecimal digits), of a repository of `repos`, refused as the forge tools refuse one. It answers with the commit the running daemon was built from, as `hataori version` shows it, `modified` when that build had changes not in its commit, and, for the commit or the pull request's merge commit, `in: true` when it is that commit or an ancestor of it and `in: false` when not, as the forge compares the two. An unmerged pull request is answered `merged: false`, and nothing is compared. When the build info records no commit, as for a binary built outside a git checkout or with `-buildvcs=false`, or the forge cannot tell (it does not know one of the commits, as for a build of a commit never pushed), the answer says so and why and has no `in`. It needs a forge that serves the repository, as the forge tools do, and reads, for GitHub, the pull request and one comparison (`/compare/`).
- **Its autonomy.** `autonomy` says how much it does without a person. The daemon reads it when it binds the supervisor's token, at each start of its session and when the daemon starts, so a change takes effect at the daemon's next start, also for a session that keeps running. In every mode it reads the scope's threads, and a request to interrupt a thread is put to a person, who approves or refuses it in the supervisor's thread; under `"watch"` it may not even ask.
  - `"watch"`: it reads only. Its tools to send, to start and to ask to interrupt answer `not allowed`, naming the mode, and reach no thread.
  - `"propose"`: a send or a start of its own runs only once a person approved it. Its tool answers at once with the proposal's number, and the supervisor's thread shows the proposal (the thread, or the repository; the text; the model and the effort) with Approve and Refuse. A person of the scope other than the supervisor's own person decides it there. Approved, the daemon makes the call as the supervisor, marked as its input, once, also when the button is pressed again or after a restart: a send is not given to a thread that is running a turn by then, nor, unless it was proposed as the answer to that question, to one waiting on a question. One line in the thread says what came of it. A send to a thread waiting on a question is proposed as its answer.
  - `"act"`: its sends and starts run at once, as before the key existed.
  - What came of each proposal and each interrupt request (run, refused and by whom, or not run, and why) wakes the supervisor as a turn end does, in its next wake. A person's decision there counts as a person's message in its thread for the loop guard.
- **Its tools need a person.** Its tools act as the id `supervisor:<scope key>`. List that id under a person of its own and that person in the scope's `people`; until then it may read the scope's threads but not start or send, and the daemon logs one line at start saying so. Removing the entry turns it off again.
- **Its token.** Each of its sessions gets a token of its own for the control socket, kept in the session's state directory and in the agent's MCP config there, outside its working directory. The daemon binds it again after a restart, so a supervisor that keeps running keeps its tools.
- Its session is kept from what it could write in its own directory for its next start: it loads no settings file, neither the project's and the local ones nor the user's own (Claude Code's `--restricted` ignores all three; seen on 2.1.328), only hataori's own settings and managed settings; it shows hataori's status line alone, not the user's; it loads no CLAUDE.md files, keeps no automatic memory and gets no claude.ai connectors; and git finds no repository there (`GIT_DIR=/dev/null`).
- So nothing in the user's own settings reaches its session: not their `env`, hooks, plugins, model or status line. A variable the CLI needs to run, such as a proxy, comes from the daemon's environment through `pass_env` (see the Claude Code section below), which a supervisor's session gets like any other.
- Its session sleeps and resumes like any other; every start and resume is confined. When a scope no longer names a supervisor, its thread stays, and a message to it fails the resume instead of starting it unconfined.

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
| `command_prefix` | string | empty | Put before the name of every command the bot registers: with `"stg-"`, the commands are `/stg-new`, `/stg-stop`, `/stg-status`, `/stg-attach` and `/stg-dashboard`. Up to 31 of `a-z`, `0-9`, `-` and `_`; each name with it must still fit Discord's 32 characters. Empty registers `/new` and the others. Changing it replaces the bot's commands at the next start. |

Unknown keys are refused. The token is read from this file only, never from the environment. Keep the file readable by its owner only (`chmod 600`); hataori checks the mode of `hataori.json` but not of the token file.

**Several instances in one server.** Each instance runs its own bot, and Discord shows every bot's commands in every channel, with the bot's name in small print only, so `/stop` of one instance is easily picked in a channel another one serves. Give each instance but one a `command_prefix`, such as `"stg-"` for a staging instance, so that their commands have different names. A command that does reach an instance outside its scopes (see "Commands") is answered with one line saying so.

To run one channel on Codex, add `agent` and the `codex` section, and name the agent in that scope:

```json
"agent": "claude-code",
"scopes": [{"key": "<channel id>", "client": "discord", "enabled": true, "agent": "codex"}],
"adapters": {"claude-code": {}, "codex": {"effort": "high"}}
```

### `forges.github`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `token_file` | string | required | Name of a file under `config/` containing the token. Absolute names and names outside that directory are refused. |

Example: `"forges": {"github": {"token_file": "github-token"}}`.

Unknown keys are refused. A token file that cannot be read or is refused stops the daemon's start with an error that names the file. The token is read once at start from this file only, never from the environment. Like `hataori.json`, the file must be regular and is refused when group or others may read it; use `chmod 600`. The supervisor's forge reads use this token. Agent sessions run as the same OS user and can read this file, so a token with read access only to the scope's repositories limits the harm.

The adapter reads issue lists and individual issues with comments; pull request lists and individual pull requests with reviews, conversation and review comments, changed files, merge state and head checks; and check runs and commit statuses for a ref; and, for the supervisor's `deployed` tool, a pull request's merge state alone and the comparison of two commits. It sends GET requests only to `https://api.github.com`, follows no redirects, takes at most 100 issues or pull requests per list, at most 300 comments, reviews, review comments, files or check runs per read, and at most 100 commit statuses. It reads no CI log. When the environment names an HTTPS proxy (`HTTPS_PROXY`), requests go through it, tunnelled with CONNECT; TLS still ends at `api.github.com` and is verified, and nothing else is sent to the proxy.

Use a fine-grained personal access token with **Only select repositories** set to the scope's repositories (public repositories need no repository access to be readable). Grant these repository permissions **Read-only**: Metadata (present on every fine-grained token), Issues, Pull requests and Commit statuses, and Contents for the comparison of two commits (`GET /repos/{owner}/{repo}/compare/{basehead}`), which the `deployed` tool reads: the same page lists it under Contents, read (read again on 2026-11-23 for it), and this too has not been checked with a real token. GitHub's documentation, [Permissions required for fine-grained personal access tokens](https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens), read on 2026-11-23, names no fine-grained permission for listing check runs (`GET /repos/{owner}/{repo}/commits/{ref}/check-runs`), so check runs on a private repository may not be readable with such a token. These permissions come from that documentation and have not been checked with a real fine-grained token.

### Keys of every adapter section

| Key | Type | Default | Meaning |
|---|---|---|---|
| `model` | string | empty | The model of threads that run this agent, when their order names none and their scope's `model` does not apply (see the order above). Empty leaves it to the next default. |
| `effort` | string | empty | The effort of threads that run this agent, when their order names none and their scope's `effort` does not apply (see the order above). Empty leaves it to the next default. |

hataori takes these two keys out before the adapter reads its section.

### `adapters.claude-code`

The section may be empty (`{}`). The common `model` and `effort` keys are also allowed.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `command` | string | `"claude"`, found on `PATH` | The Claude Code command to run. |
| `hataori` | string | this binary | The hataori binary that Claude Code's hooks run as `<hataori> hook`. |
| `hook_timeout` | duration | `"24h"` | How long a hook that waits for an answer (a question from the agent, a permission request) waits. At least one second. |
| `input_timeout` | duration | `"30s"` | How long input pasted into Claude Code may go unreported by its `UserPromptSubmit` hook before it counts as not delivered. At least one second. |
| `pass_env` | array of strings | empty | Names of variables in the daemon's environment to pass to Claude Code besides the fixed list below, for example an API key it logs in with. |

Unknown keys are refused; `model` and `effort` are hataori's (see [Keys of every adapter section](#keys-of-every-adapter-section)).

### `adapters.codex`

The section may be empty (`{}`). The common `model` and `effort` keys are also allowed.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `command` | string | `"codex"`, found on `PATH` | The Codex command to run. |
| `pass_env` | array of strings | empty | Names of variables in the daemon's environment to pass to Codex besides the fixed list below, for example an API key it logs in with. |
| `sandbox` | string | `"danger-full-access"` | Sandbox for every `thread/start` and `thread/resume`: `read-only`, `workspace-write`, or `danger-full-access`. |
| `approval_policy` | string | `"never"` | Approval policy for every `thread/start` and `thread/resume`: `untrusted`, `on-request`, or `never`. |

Unknown keys are refused; `model` and `effort` are hataori's (see [Keys of every adapter section](#keys-of-every-adapter-section)). The defaults, like Claude Code's today, never ask for approval and allow commands to write anywhere and use the network.

Codex writes session files (rollouts) under `sessions/` and caches under `CODEX_HOME` (`~/.codex` by default), as for any thread. Codex would also record the workspace's trust in `config.toml`; hataori prevents that write by supplying `-c projects={...}` on the command line. Codex treats the workspace as a trusted project and reads the workspace's `.codex/` configuration, as it would after recording the trust itself. hataori never sets `CODEX_HOME`, runs a login, or writes the user's Codex configuration.

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

There is no session cookie: a browser sends a cookie to every port of its host, so another service on the same machine could receive it. The page keeps its session in its own origin's storage and sends it in a header with every request. Sessions made by an earlier version, one that used a cookie or one from before passkeys, are not recognised: sign in again. The page's own files and the sign-in routes (trading a link, and signing in with a passkey) are served without a session; every other route needs one. Only `GET` and `HEAD` are served, except on the passkey routes below, whose other requests must carry an `Origin` that is exactly the listener's address.

A link's token is in the part after `#`, which a browser does not send: the page trades it for the session with a request of its own. So a chat app or a link checker that fetches the link, for a preview, uses nothing up.

**Passkeys on the web listener.** On the web listener, after the first sign-in with a link, the page offers to add a passkey on that device, and from then on its sign-in page offers "Sign in with a passkey", which asks for no name. A passkey is for the web listener's name (`web.url`): a web listener named by an IP address has no passkeys, since browsers take them only for a domain name, and the daemon logs one line saying so. The browser verifies the person (a fingerprint, a face, a PIN) every time.

- A passkey is added only right after a proof made for it: within five minutes of opening a fresh sign-in link on that device, or after checking one of your own passkeys (the page does that when you add one later, and offers only yours). A session alone, however new, adds none, so whoever takes a session cannot keep a way back in. A proof from a passkey stops working when that passkey is removed. On the loopback listener SSH or the host is already the proof, so passkeys are neither added nor used there.
- One person may have 32 passkeys.
- Signing in with a passkey opens a session like a link does, twelve hours, and the session records when the passkey was checked. A passkey of a person who may no longer sign in opens nothing, and the daemon logs that. A passkey belongs to the person's name in the config: it works again if that name may sign in again, so remove a person's passkeys (or the file) when the name goes to someone else.
- Removing a passkey does not end a session already opened with it, as with a link: the session lasts its twelve hours.
- A passkey whose signature counter does not go up, a sign that it was copied, is refused and the refusal is logged. Synced passkeys keep no counter and are not affected.
- "Passkeys" in the status bar lists yours, when each was made and last used, and removes one, on either listener. A removed passkey no longer signs in; to add it back, open a new sign-in link.

The passkeys are kept in `state/dashboard-passkeys.json`, next to the session key, mode 0600, rewritten whole for each change: for each, its credential id, public key, signature counter, person, user handle, and when it was made and last used. It holds no secret, but whoever may write it may add a passkey: a file that other users may open stops the daemon, as the session key does. Removing the file removes every passkey, at the latest at the next start.

The loopback listener refuses a request that came through a reverse proxy (one that carries `Forwarded`, `X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto`, `X-Real-IP` or `Via`): put a proxy in front of the web listener instead.

**On another machine, over SSH, with `hataori connect`.** On your machine (Linux or macOS), run

```sh
hataori connect user@host --root /path/to/the/instance
```

It uses your `ssh`, with your keys, agent and SSH config, and asks for a password or passphrase as `ssh` would, once per connection. It opens one shared SSH connection, runs `hataori signin` on the host through it, listens on a local port on `127.0.0.1` and passes each connection to it through SSH to the dashboard's loopback listener, and opens your browser at the link on `http://127.0.0.1:<local port>`. Then it keeps running and keeps the connection up: when the connection drops, it connects again, and since it holds the local port the whole time, nobody else can take it in between, and an open page goes on by itself. Stop it with Ctrl-C. One `hataori connect` runs per host at a time.

It remembers the host, the root, `--hataori` and the local port, so next time `hataori connect` alone is enough. Run it again while it runs for the same host to have the running one open a fresh sign-in link, for example after the session ended. It writes no SSH key and no SSH config, and keeps the hosts in `hataori/connect.json` in your configuration directory, readable by you only.

| Flag | Meaning |
|---|---|
| `--root DIR` | The instance root on the host; without it, the host's `$HATAORI_ROOT` in a non-interactive SSH session, which is often unset. |
| `--hataori PATH` | The `hataori` binary on the host, when a non-interactive SSH session's `PATH` does not have it. |
| `--port N` | The local port. Without it: the one remembered for the host if it is free, else the dashboard's own number if that is free, else any free port. |

The link is written to no file and no log, and passed to no process but your browser's launcher (`open` on macOS, `xdg-open` on Linux). While that launcher runs, other users of the same machine can read its command line, the link included. The link works once and only for a few minutes. If someone else uses it first, the page tells you the link was already used instead of signing you in: ask for a new one. On a machine you share with people you do not trust, wait for the native window.

The host's sshd must allow forwarding (`AllowTcpForwarding`); `hataori connect` says so when it does not.

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
| `trash_retention` | duration | `"168h"` | How long a removed workspace stays in `trash/` before it is deleted. A positive Go duration. Only entries the removal put there are ever deleted, by the time in their names; one still on its way in (`pending-…`) and anything else in `trash/` are left alone. The daemon purges when it starts and then every quarter of the retention, at least every second and at most every ten minutes. It logs one line for each entry it deletes; an entry it cannot delete is logged and tried again on the next pass. |
| `other_roots` | array of strings | empty | Absolute paths of the roots of other hataori instances on this host. A remote under one of them does not count. |
| `remove_closed_after` | duration | absent: off | Turns on the automatic removal pass: the workspace of a thread that has been closed for longer than this is removed, with the same proof as `hataori remove`. A positive Go duration, such as `"720h"`. A refused removal leaves the thread closed, writes one `removal_refused` event, and is tried again only after the same duration. |

A workspace is removed only when its thread is closed: `hataori close NUMBER` closes it, and `hataori remove NUMBER` removes it (see "Commands and flags" below). Nothing in chat removes a workspace, and no thread is closed by its idle age.

`hataori reopen NUMBER` takes a closed or removed thread back, in the same chat thread: its next message wakes it. A closed thread resumes its conversation in its directory. A removed thread's directory is taken back from `trash/` while it is there, and its conversation resumes; once the trash pass has deleted it, the directory is cloned again at the same place and a new conversation starts, told that the chat thread holds the earlier exchange. Nothing at the directory's place is ever overwritten: a place that something else took refuses the reopen. In chat, the owner's message in a closed thread gets a Reopen button, which does the same and then sends the message; it does nothing for anyone else. A supervisor's thread is reopened only while no other supervisor thread of its scope is open.

Processes are found with `lsof`, on Linux and on macOS alike; without `lsof` every removal is refused. Unless hataori runs as root, `lsof` sees only the processes of the user hataori runs as.

### `plugins[]`

A plugin is a separate program that adds to what hataori does ([design, section 7.3](design/overview.md#73-plugins)). Its manifest, which its author writes, says what it asks for: [plugins.md](plugins.md). Its entry here says where it is and what the operator grants it. The plugin gets what its manifest asks for and its entry grants, and nothing more: an entry that names anything the manifest does not ask for is refused, not ignored.

When the daemon starts, it reads each plugin's manifest, `hataori-plugin.json` in the directory of `path`, and checks it with the entry. It reads no other file of the plugin and runs nothing then. Any error stops the start with one line that names the key, and for an error in a manifest the manifest's key too.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `id` | string | required | The plugin's name, the same as its manifest's `id`. It follows the rule of driver names. Each id may be listed once. |
| `path` | string | required | The absolute path of the plugin's executable; its manifest is in the same directory. For an `external` plugin, which hataori does not start, only the directory is used, for the manifest: the file itself need not exist. |
| `sha256` | string | required unless `external` | The SHA-256 of the executable: 64 hexadecimal digits. Refused with `external`, since hataori does not start that program. |
| `external` | boolean | `false` | The plugin is a program that hataori does not start, which connects to the control socket by itself with the token in `token_file`. |
| `token_file` | string | required with `external` | The file in `<root>/config/` that holds an external plugin's token, relative to `config/`: an absolute path, a path out of `config/` and `config/` itself are refused. Spaces and line breaks around the token are ignored; a file that cannot be read, or is empty, stops the daemon's start with one line naming the key. Refused without `external`: a plugin that hataori starts gets a token at each start. |
| `scopes` | array of strings | required | The scopes the plugin may see and act in, by their keys under `scopes`; at least one. |
| `operations` | array of strings | the manifest's | The control operations it may call: some or all of its manifest's `operations`. |
| `events` | array of strings | the manifest's | The event kinds it may read: some or all of its manifest's `events`. An empty list grants none. |
| `hooks` | array | the manifest's | The hook points it handles, one object each (below): some or all of its manifest's `hooks`. Without this key, every hook point of the manifest with its defaults. |
| `conversation` | boolean | `false` | Whether it may read message text: inputs, the agent's text, posts and thread titles. Only for a manifest that asks for `conversation`; without this grant, what the plugin gets has the text left out. |
| `settings` | object | `{}` | The plugin's own settings. hataori checks only that they are an object and gives them to the plugin as written, `null` values included. The manifest's `config` schema is not applied to them. |

No plugin is given `SignInLink`, `HostTerminals`, `HostScreen`, `HostView`, `OperateHost` (with `HostKeys` and `End`), `AttachTerminal`, `Close`, `Remove`, `TakeIn` or `Release`: a manifest or an entry that names one is refused with one line naming the key. A plugin calls as the id `plugin:<id>`; to let it `Start`, `Send`, `Answer`, `Stop` or `Link`, list that id under a person and the person in the scope's `people`.

#### `plugins[].hooks[]`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `point` | string | required | A hook point of the manifest's `hooks`, listed once. |
| `timeout` | duration | the manifest's, else `"15s"` | How long the plugin's handler has at this point. A positive Go duration. |
| `on_failure` | string | the point's | `block` or `skip`: whether what passes the point is stopped, or the handler skipped, when the handler fails or times out. `before_agent_run` blocks unless this says otherwise; the other points skip. |

For example, a plugin that reads posts before they are sent, made to block a post when it fails:

```json
"plugins": [
  {
    "id": "secret-scan",
    "path": "/opt/hataori-plugins/secret-scan/secret-scan",
    "sha256": "<the executable's SHA-256>",
    "scopes": ["<channel id>"],
    "hooks": [{"point": "message_sending", "on_failure": "block"}],
    "conversation": true,
    "settings": {"patterns": ["AKIA[0-9A-Z]{16}"]}
  }
]
```

## Environment variables

hataori reads these:

| Variable | Meaning |
|---|---|
| `HATAORI_ROOT` | The instance root when `--root` is not given. |
| `XDG_RUNTIME_DIR` | Used for the control socket when `<root>/state/control.sock` is too long for a Unix socket. Then the system temporary directory is tried. The chosen path is written into `<root>/state/control.sock.path`, where the commands find it. |
| `HATAORI_HOST_SOCKET` | Internal. hataori sets it for Claude Code so that `hataori hook` reaches its session. Do not set it yourself. |
| `HATAORI_TOKEN` | Internal. The token of a supervisor's session, which hataori puts in that session's MCP config for `hataori mcp`. Do not set it yourself. |

Claude Code does not inherit the daemon's whole environment. It gets only `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TZ`, `TMPDIR`, `XDG_RUNTIME_DIR`, `XDG_CONFIG_HOME`, `CLAUDE_CONFIG_DIR`, the variables named in `pass_env`, and `TERM=xterm-256color`.

Codex gets only `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TZ`, `TMPDIR`, `XDG_RUNTIME_DIR`, `XDG_CONFIG_HOME`, `CODEX_HOME`, and variables named in `pass_env`, when present.

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
hataori reopen  [--root DIR] NUMBER
hataori signin  [--root DIR] [--web]
hataori connect [[user@]host] [--root DIR] [--hataori PATH] [--port N]
hataori version
```

| Command | Meaning |
|---|---|
| `run` | Runs the daemon in the foreground until SIGINT or SIGTERM. Logs go to standard error. Agent sessions keep running when the daemon stops, and the daemon picks them up when it starts again. |
| `threads` | Prints every thread, one JSON object per line: its number (`thread`), its scope, its links (the chat conversations it is shown in), its state, title and repository. For each chat client that is not connected to its chat service, it also prints one line on standard error, with the time since when: no message from that client reaches hataori meanwhile, and the daemon keeps trying to connect it. |
| `thread NUMBER` | Prints one thread and its state, by its number, with or without `#` (`7` or `#7`). Exits with 3 when no thread has that number, and with 2 when the argument is not a number. |
| `events` | Prints the event log, one JSON object per line. |
| `stop NUMBER` | Interrupts the running turn of the thread with that number, as `thread` takes it. |
| `attach NUMBER` | Connects this terminal to the terminal of the thread's agent, on the host, for the owner. It shows the current screen, then passes every key to the agent; Ctrl-\ detaches and leaves the agent running. An asleep thread is woken first; a closed thread, and an agent without a terminal, are refused with a message. While attached the thread is not put to sleep, and a daemon restart does not end the attachment. A turn typed there is posted in chat after a line "(typed at the terminal)". Chat input is still delivered while you are attached, into the same input line: a chat message that arrives while you have typed part of a line is mixed with it, so finish or clear the line before you stop typing. The current screen is replayed from the end of the terminal's log and is exact only once the agent draws again. Output is the agent's terminal, not JSON. |
| `trusts` | Prints one line per file the owner trusted from a blocked start in chat: `{"id", "scope", "repo", "file", "by", "at"}`. The config's `trust_files` are not listed. |
| `untrust ID` | Withdraws one file `trusts` lists, by its `id`: the next start whose folder-trust dialog names it is blocked again. A session already running is not changed. Prints `{"id", "trusted": false}`. |
| `close NUMBER` | Ends the agent process of an idle, asleep or failed thread and closes the thread; its workspace stays. A message in a closed thread starts nothing and gets one line saying the thread is closed. A thread with a turn running, a question open, or background tasks left running by its last turn is refused: stop it first. Prints `{"thread", "scope", "state": "closed"}`. |
| `remove NUMBER` | Moves the workspace of a closed thread into `<root>/trash/`, only when nothing in it would be lost (see `cleanup` above), and the thread becomes `removed`. Otherwise it changes nothing, prints the reason on standard error and exits with 1. Prints `{"thread", "scope", "state": "removed"}`. |
| `signin` | Prints `{"link": ...}`: a link that signs the owner in to the dashboard's loopback listener, or with `--web` its web listener, once, within five minutes. Fails when the config has no `dashboard`, or when it has no such listener. |
| `connect` | Runs on your own machine, not on the host: the dashboard of the instance on `host`, over SSH, until stopped (see "On another machine, over SSH, with `hataori connect`" above). Without a host, the one it connected to last. |
| `version` | Prints the version, and the commit the binary was built from as its build info records it, with `modified` when the source had changes not in that commit, or that it records none (a binary built without version control information). |

| Flag | Default | Meaning |
|---|---|---|
| `--root DIR` | `$HATAORI_ROOT` | The instance root. Required by every command above except `version`. |
| `--after N` | `0` | `events` only: print the events after position `N`. |
| `--follow` | off | `events` only: keep printing new events as they happen. |
| `--web` | off | `signin` only: a link for the dashboard's web listener instead of its loopback listener. |

Flags may come before or after `NUMBER`. Exit codes: 0 success, 1 error (for example, no daemon runs for the root), 2 usage error, 3 no such thread.

`threads`, `thread`, `events`, `stop`, `close`, `remove` and `signin` talk to the running daemon over its control socket, and only the OS user that runs the daemon may use them. The format of what they print is in [protocol.md](protocol.md).

Three more commands are internal, run by hataori itself, and not for people: `hataori host <state-dir>` runs one agent session, `hataori hook [--wait] [--timeout DURATION]` is the command Claude Code's hooks call, and `hataori mcp --scope SCOPE --root DIR` is a supervisor's MCP server, which its agent starts with its token in `HATAORI_TOKEN`.

## Orders

An order is a message in an enabled channel (not in a thread):

```
<trigger> repo:<url> [agent:<name>] [model:<model>] [effort:<effort>] <prompt>
```

- The options are `name:value` words on the first line, after the trigger word. The prompt is everything after the last option, and may span many lines.
- `repo:` is required. It is a URL with the scheme `https`, `http`, `ssh`, `git` or `file`, or the `user@host:path` form.
- `agent:` is a name under `adapters`, such as `codex`: the thread runs that agent instead of the scope's. An unknown name starts nothing and writes one log line.
- `effort:` is one of `low`, `medium`, `high`, `xhigh`, `max`. Any other value gets a reply saying nothing was started.
- The author must be the owner or one of the scope's `people`. Posts by bot accounts are ignored; posts through a webhook are accepted when the webhook's id belongs to one of the scope's people.

hataori opens a thread on the message, clones the repository into the thread's working directory, starts the agent there and posts its replies into the thread. Later messages in that thread from the owner or one of the scope's people go to the same agent session. A question from the agent is shown with one button per option, or a menu when several may be chosen; pressing one answers it, also after a restart, and the question then loses its buttons. A question with more options than the service shows as buttons is shown with its options numbered. Either way, the next message in the thread answers an open question. A press by anyone but the owner or one of the scope's people, or on a question that is no longer open, answers nothing and writes one log line. The message that started a turn shows a lamp while the turn runs (working) and when it ends (done, error, or waiting for an answer), and its sender is pinged once at the end, in the thread; a post through a webhook gets the lamps and no ping. In a supervisor's thread a turn that completes pings nobody (see `scopes[].supervisor`). A turn a person stopped (with `/stop`, the Stop button or `hataori stop`) is not shown as a failure: the thread gets one line, "Stopped by <name>.", with the name of the person who stopped it (for `hataori stop`, the owner), the message loses its working lamp and gets no other, and nobody is pinged. Its `turn_ended` event still says `failure`, with reason `interrupted`, or `stopped` when the agent did not end the turn in time and its session was stopped. A turn the agent starts by itself lamps the message of the last turn someone started, and pings that sender only when it fails or waits for an answer. A turn typed at the terminal gets neither. While a turn has been quiet for a while, its progress line ("Still working: …") carries a Stop button: pressing it stops the running turn, as `/stop` does, for the owner or one of the scope's people; the line then says the turn is being stopped, without the button, until the turn ends. A press by anyone else, a second press, and a press of a turn's button after that turn ended stop nothing and write one log line; a button stops only its own turn, also when that turn ends and the next one starts just as it is pressed. The button keeps working after a restart. An order that is refused for any reason other than the effort value starts nothing and writes one log line.

## Commands

```
/new repository:<url> prompt:<prompt> [model:<model>] [effort:<effort>]
/stop
/status
/dashboard
```

The names are these unless the driver registers them with a prefix (`command_prefix` under `drivers.discord`). On a service without commands, the same words follow the trigger: `<trigger> new repo:<url> [agent:<name>] [model:<model>] [effort:<effort>] <prompt>`, `<trigger> stop`, `<trigger> status` and `<trigger> dashboard`.

- `/new` in an enabled channel starts a thread as an order does: the thread is opened in the channel, and the full prompt is its first message. The same command delivered again starts nothing more. `<trigger> new` is the order itself, opened on the message. `/new` has no agent option: its thread runs the scope's agent; use the order or `<trigger> new` with `agent:` for another.
- `/stop` in a hataori thread stops its running turn; the thread then says who stopped it.
- `/status` in a hataori thread shows its state, model and last output; in a channel, the open threads of the channel's own scope with their states. Threads of other scopes attached in the channel are not listed.
- The answers are shown only to the person who used the command; the words after the trigger get a reply.
- A command used in a channel that is no scope's key and in no scope's `show_in`, or in a thread of such a channel, does nothing and is answered, to the person who used it only, with one line saying that this instance does not serve the channel; it writes one log line. This is `/dashboard` and `/attach` too. It happens when several instances share one server and the person picked another instance's command (see `command_prefix` under `drivers.discord`).
- The person must be the owner or one of the scope's `people`, for `/status` too. From anyone else, a command shows nothing and writes one log line.
- `/dashboard` sends a dashboard sign-in link in a direct message to the person who used it, for the web listener when the instance has one. Only the owner gets one; from anyone else it sends nothing anywhere and writes one log line. The link is posted at once: it is in no conversation, no database row and no log, and the log says only that a link was sent and to whom. The conversation the command was used in gets nothing. An instance without a dashboard answers with one line saying so.

## Showing a thread in another channel

A thread can be shown in more than one chat conversation, on one chat service or on several. Each is a link of the thread; every link gets the agent's answers, and input given in any of them goes to the same session.

```
/attach <number>
<trigger> attach <number>
```

- `/attach <number>` in a channel opens a chat thread there, titled like the hataori thread, and links it. Used inside a chat thread, it links that chat thread. Its answer is shown only to the person who used it. `<trigger> attach <number>`, posted in a channel, does the same on a service without commands, and answers nothing.
- The channel must be the scope's own or listed in the scope's `show_in`. The person must be the owner or one of the scope's `people`, as for posting into the thread.
- Attaching a thread where it is shown already changes nothing. A chat thread that shows one hataori thread is not linked to another.
- A refused attach links nothing, opens nothing and writes one log line.
