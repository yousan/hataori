# Plugins

A plugin is a separate program, in any language, that adds to what hataori does without changing it ([design, section 7.3](design/overview.md#73-plugins)). It talks to the daemon over the control socket ([protocol.md](protocol.md)). The operator lists it in the instance's config ([configuration.md, `plugins[]`](configuration.md#plugins)); this file describes what the plugin's author writes, its manifest.

The daemon checks each plugin's manifest and config entry when it starts, then runs the plugins (below). It has the hook calls and the dispatcher that runs them (below, [protocol.md, Requests the daemon sends](protocol.md#requests-the-daemon-sends)); the core and the chat front end do not call a hook point yet.

## What a plugin's connection may do

A plugin's connection is bound, by its token, to a caller made from its grant: what the manifest asks for and the config grants, and nothing more. The core refuses any other operation with `forbidden` and one log line naming the operation, the scope and the plugin. Events of kinds it was not granted are left out of `Events`, `ThreadEvents` and `Thread`; positions are the instance's own, so a plugin that reads again after the last position it kept misses no event of its kinds. `Threads` and `Thread` still summarize a thread's state, such as its turns, usage and open requests, whatever kinds were granted. Without the `conversation` grant the text is left out of what it reads, and `Terminal` is refused.

This binds a plugin that connects with its token. It is not confinement: a process of the daemon's OS user, as a plugin is, can change the config and read the state directory, and a connection without a token from that user is the owner's. To confine a plugin, run it as another OS user. A guard catches a plugin that forgot its token: a connection without a token from a process in the process group of a plugin hataori started is refused, with a log line naming the plugin, and so is a `hataori` command the plugin starts. A process can leave its group, and the guard knows only the plugins the daemon started in its current run, so it is against mistakes, not a boundary.

A plugin calls as the id `plugin:<id>`. Its `Start`, `Send`, `Answer`, `Stop` and `Link` are refused like a guest's unless the operator lists that id under a person of the config and that person in the scope's `people` ([configuration.md, `people[]`](configuration.md#people)), as for a supervisor. `AskInterrupt` is refused in this version whatever the grant: a plugin has no thread to ask in.

## How a plugin runs

hataori starts each plugin that is not `external` as a child process when the daemon starts, once the control socket listens.

- **Before each start**, the manifest's `protocol` must have the daemon's major version and a minor version no higher than the daemon's, and the executable's SHA-256 must be the config's `sha256`. If either does not hold, nothing starts and one log line names both values (both versions, or the expected and the actual hash).
- **The file that runs is the one that was hashed.** hataori reads the executable once into memory, hashes those bytes, and writes them to a new file, under a new name at each start, in a directory of its own, `state/plugin-run/<id>/` (mode 0700). That file is the one it starts. A file put in the executable's place after it was read does not run before the next start, which hashes it again. This guards the configured path, not the daemon's own files: a process of the daemon's own user can change those as it can the config.
- **What runs is that copy**, in the executable's directory as its working directory. A binary gets its configured path as its first argument. A script finds its interpreter through its `#!` line as usual, but the interpreter gets the copy's path, so the script's own path, its directory, and what is found next to it through them (Python's `sys.path[0]` and `__file__`, Node's `require("./x")`, `$(dirname "$0")`) are the copy's. A plugin is therefore one file. A plugin that needs files next to it finds them through its working directory.
- **The environment** has, of the daemon's own environment, only `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TZ`, `TMPDIR`, `XDG_RUNTIME_DIR` and `XDG_CONFIG_HOME`, as an agent does. hataori adds:

  | Variable | Value |
  |---|---|
  | `HATAORI_SOCKET` | The control socket's path. |
  | `HATAORI_TOKEN` | The token of this start. Present it in the hello ([protocol.md](protocol.md)); it binds the connection to the plugin's grant. It is revoked when the plugin exits, and a new one is issued at the next start. |
  | `HATAORI_PLUGIN_ID` | The plugin's id. |
  | `HATAORI_PLUGIN_STATE` | Its state directory, `state/plugins/<id>/` (mode 0700), kept across starts and restarts of the daemon. Keep the event position here. |
  | `HATAORI_PLUGIN_SETTINGS` | A file holding the config entry's `settings`, a JSON object, written again at each start. |
  | `HATAORI_PROTOCOL` | The control protocol the daemon speaks, such as `3.12`. |
  | `HATAORI_PLUGIN_RUN` | A random value, new at each start, that marks the processes of this start; keep it in the environment of what the plugin starts, so that a next daemon can tell them (Stopping, below). |

- **Output.** What the plugin writes to its standard output and standard error goes to the daemon's log, a line at a time. Each line is marked with the plugin's id and cut at 4096 bytes; a longer line is logged as soon as 4096 bytes of it came. The plugin's token is shown as `[token]`.
- **Restarts.** When a plugin exits, its token is revoked and its connections close. It starts again after 1 second, then 2, 4 and so on, up to 1 minute. A plugin that exits 5 times within 10 minutes stays stopped until the daemon restarts, and so does one whose hash or protocol does not hold. The owner then gets one direct message, in the conversation of the login alert, and the log has one line. While it stays stopped, also across restarts of the daemon, the owner is not told again. A run that has lasted 10 minutes ends the outage, while it runs. If the owner cannot be reached, telling is tried again until it succeeds or the daemon stops.
- **Stopping.** When the daemon stops, each plugin it started is first sent `plugin_stopping` (below) and has 10 seconds to answer; then it gets SIGTERM, and after 10 more seconds SIGKILL. Both go to its process group: whatever it started in that group goes with it, and so it does when the plugin exits by itself. What it started outside its group goes too, when its environment still holds the run's `HATAORI_PLUGIN_RUN` (below). On Linux the plugin itself, not what it started, is also killed when the daemon dies without stopping it; on macOS it is not. What such a run leaves is killed at the next start of the daemon, before the plugin starts again: each process that runs as the daemon's user, on the same boot, and whose environment holds that run's `HATAORI_PLUGIN_RUN`, a random value recorded before the run started. A process that does not hold it, or whose environment cannot be read, is never signalled, whatever its process group or pid; so a process the plugin started and that cleared its environment is left. On Linux the process is held by a pidfd from its check to the signal, so a pid given to another process meanwhile is not signalled. macOS has no pidfd: there the process's start time is read again just before the signal, which leaves only the moment between that read and the signal. A run whose processes could not all be looked for keeps its record, and is looked for again at the next start.
- **An `external` plugin** is not started. The token in its `token_file` is bound to its grant when the daemon starts, and the program connects with it by itself. A connection with any other token is refused, and the log does not show the token. Two token files that hold the same token stop the daemon's start, naming both keys.

## The manifest

The manifest is `hataori-plugin.json`, in the same directory as the plugin's executable. hataori reads it when the daemon starts, without running the plugin. It says what the plugin is and what it asks for; the operator's config grants some or all of it, never more.

- It is one JSON object in a regular file of at most 1 MiB.
- Unknown keys are refused, a `null` value is refused (inside `config` it is kept), and spaces around string values are ignored.
- An error stops the daemon's start with one line that names the config entry, the manifest and the key.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `id` | string | required | The plugin's name: a lowercase letter, then lowercase letters, digits and `-`. The config entry's `id` must be the same. |
| `version` | string | required | The plugin's version, as its author writes it. hataori does not interpret it. |
| `protocol` | string | required | The control protocol it speaks, `"<major>.<minor>"`: the major version, and the lowest minor version it needs, such as `"3.0"` ([protocol.md, Versions](protocol.md#versions)). |
| `operations` | array of strings | empty | The control operations it calls, by their names in [protocol.md](protocol.md#operations), such as `Threads` and `Events`. `SignInLink`, `HostTerminals`, `HostScreen`, `OperateHost` (with `HostKeys` and `End`), `AttachTerminal`, `Close`, `Remove`, `TakeIn` and `Release` are refused: no plugin gets them. |
| `events` | array of strings | empty | The event kinds it reads, by their names in [protocol.md](protocol.md#events), such as `turn_started` and `turn_ended`. These are the observe hooks. |
| `hooks` | array | empty | The hook points it handles, one object each (below), each point once. |
| `conversation` | boolean | `false` | Whether it needs message text: inputs to the agent, the agent's text, outgoing posts and thread titles. Without it, and without the operator's grant, what the plugin gets has the text left out. |
| `config` | object or boolean | none | A JSON Schema for the plugin's settings, for people and tools. hataori keeps it as written and does not check the settings against it; the plugin checks its own settings. |

### `hooks[]`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `point` | string | required | One of the hook points below. |
| `priority` | integer | `0` | Handlers on one point run one after another by priority, higher first, and then in the config's order. |
| `timeout` | duration | `"15s"` | How long its handler has, a positive Go duration such as `"5s"`. The operator's config can change it. |

### Hook points

| Point | Kind | Where | When a handler fails or times out |
|---|---|---|---|
| `before_agent_run` | gate | before input from `Start` or `Send` reaches the agent; may stop it, with a reason the thread shows | the input is blocked |
| `before_prompt_build` | modify | at the same point; may add text before or after the input | the handler is skipped |
| `message_sending` | modify, gate | before a post is sent to the chat; may rewrite its text or cancel it | the handler is skipped |

The operator can change the failure policy per plugin and point (`on_failure` in the config).

### How a hook is called

Since protocol 3.16. At a point, the daemon sends a `hook` request to each plugin whose grant has the point and the thread's scope, one after another: by priority, higher first, then in the config's order. Each gets the text as the one before left it, and answers `continue`, `continue` with a new `text` where the point may change it (`message_sending`), `continue` with text to add in `before` and `after` where the point may add it (`before_prompt_build`, since protocol 3.18), or `stop` with a `reason` where the point may stop it. Without the `conversation` grant the request has no text.

A handler fails, and its policy (`block` or `skip`) applies, when:

- it does not answer within its timeout, 15 seconds unless the manifest or the config says otherwise;
- its plugin is not connected, while it starts, starts again or stays stopped: so by default a `before_agent_run` handler blocks input in its scopes until the plugin is back. An operator who prefers that work goes on sets that plugin's `on_failure` to `skip`;
- its connection closes before it answers;
- it answers with an error, `not_supported` too;
- its answer is one the point does not allow, such as `stop` or `text` at `before_prompt_build`.

### The input points

Since protocol 3.18 the core runs `before_agent_run`, then `before_prompt_build`, inside each `Start` and `Send`, before it accepts the input ([protocol.md, `Start`](protocol.md#operation-start)):

- **A stopped input is refused** with the error code `stopped`, which names the plugin and the reason, and nothing of it is kept: no thread, chat thread or workspace for a `Start`, no `input` event for a `Send`. In chat, the trigger of a stopped `Start` gets one reply naming the plugin and the reason, and a stopped message in a thread gets one line, as a reply to it. The core logs the stop once, besides the dispatcher's line for each call; neither has the text or the reason.
- **Text `before_prompt_build` adds** reaches the agent around the input, each part on lines of its own. Each handler sees the input with what the handlers before it added, and adds around that. The `input` event keeps the input as it was given in `text`, which is what the other links of the thread show, and the added text in `before` and `after`, which `hataori events` shows; a session that replaces one that cannot resume gets the earlier inputs with their added text. An answer to an open question runs `before_agent_run` only: it gets no added text, but can be stopped.
- **A supervisor's wake** that a plugin stops is dropped, and the next turn end wakes it with what came since; one blocked by a handler's failure is tried again until the plugin is back.
- **A key accepted before runs no hook**, so a `Start` or `Send` given again, after a restart too, calls no handler twice, and input that was accepted before a restart reaches the agent as it was then, with its added text.
- The caller waits for the handlers: at most their timeouts.

The daemon sends hook requests on the plugin's oldest connection that is still open and is not a stream: open a connection for requests first, and follow events on others. The project's Go client answers them with the handler given to `wire.DialTokenHandler`. When a plugin connects while it had no connection open, the daemon sends it `plugin_started` with its grant; before hataori stops a plugin it starts, it sends `plugin_stopping`, and SIGTERM follows when the plugin answers or after 10 seconds. Each call is logged with the plugin, the point, the thread and the outcome, never with text or a plugin's reason.

### The outbox point

Since protocol 3.19 the chat front end calls `message_sending` before it posts a message of a thread, and before an edit that changes the text a post shows, as the post or its last edit left it ([protocol.md, `hook`](protocol.md#request-hook)). The progress line's edits are hooked like any other; when they are made is not changed. Lamps, deletes and edits that keep the text are not hooked.

- **A rewrite** (`continue` with `text`) is what is posted. The same message in the thread's other links is posted with the same text, and the plugin is called once for it.
- **A cancel** (`stop`) posts nothing in its place. The message stays in the outbox with the plugin and the reason, and the messages behind it go on. `hataori thread <number>` lists it under `cancelled`, and a `post_cancelled` event records it. A reply to a cancelled message is posted without the reply; an edit or a lamp on it is dropped.
- **Stored before the post.** What the handlers made of a message is stored with it before it is posted, so a message posted again, after a restart too, is not hooked again. A daemon that stops while a handler is called calls the handlers again for that message.
- A message of a thread's link is called with the thread's number and scope. A message with no link in a scope's own channel or one of its chat threads, such as the reply to a trigger or to a command, is called with that scope and no thread. A direct message is not hooked.
- **`skip` lets text out.** While a handler whose policy is `skip` fails, or its plugin is not connected, messages go out as they are. That includes an edit that rebuilds a post's text from the event, such as a question's "Answered" line, so text the plugin rewrote in the first post can come back. A plugin that must not let text through sets `on_failure` to `block`.
- **`post_sent`** is the event of a message posted in a link of a thread, written once per message in the transaction that marks it posted. It carries the link, the event and part it shows, the chat service's message id, and the text for a plugin with the `conversation` grant. The daemon writes it only while a plugin of the config is granted the event kind `post_sent`.

## An example

```json
{
  "id": "secret-scan",
  "version": "0.3.0",
  "protocol": "3.0",
  "operations": ["Threads"],
  "events": ["thread_created", "turn_ended"],
  "hooks": [{"point": "message_sending", "priority": 100, "timeout": "5s"}],
  "conversation": true,
  "config": {
    "type": "object",
    "properties": {"patterns": {"type": "array", "items": {"type": "string"}}}
  }
}
```
