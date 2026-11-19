# Plugins

A plugin is a separate program, in any language, that adds to what hataori does without changing it ([design, section 7.3](design/overview.md#73-plugins)). It talks to the daemon over the control socket ([protocol.md](protocol.md)). The operator lists it in the instance's config ([configuration.md, `plugins[]`](configuration.md#plugins)); this file describes what the plugin's author writes, its manifest.

The daemon checks each plugin's manifest and config entry when it starts, then runs the plugins (below). It does not call hooks yet.

## What a plugin's connection may do

A plugin's connection is bound, by its token, to a caller made from its grant: what the manifest asks for and the config grants, and nothing more. The core refuses any other operation with `forbidden` and one log line naming the operation, the scope and the plugin. Events of kinds it was not granted are left out of `Events`, `ThreadEvents` and `Thread`; positions are the instance's own, so a plugin that reads again after the last position it kept misses no event of its kinds. `Threads` and `Thread` still summarize a thread's state, such as its turns, usage and open requests, whatever kinds were granted. Without the `conversation` grant the text is left out of what it reads, and `Terminal` is refused.

A plugin calls as the id `plugin:<id>`. Its `Start`, `Send`, `Answer`, `Stop` and `Link` are refused like a guest's unless the operator lists that id under a person of the config and that person in the scope's `people` ([configuration.md, `people[]`](configuration.md#people)), as for a supervisor. `AskInterrupt` is refused in this version whatever the grant: a plugin has no thread to ask in.

## How a plugin runs

hataori starts each plugin that is not `external` as a child process when the daemon starts, once the control socket listens.

- **Before each start**, the manifest's `protocol` must have the daemon's major version and a minor version no higher than the daemon's, and the executable's SHA-256 must be the config's `sha256`. If either does not hold, nothing starts and one log line names both values (both versions, or the expected and the actual hash).
- **The file that runs is the one that was hashed.** hataori reads the executable once into memory, hashes those bytes, and writes them to a new file in a directory of its own, `state/plugin-run/<id>/` (mode 0700). That file is the one it starts. A file put in the executable's place after it was read does not run before the next start, which hashes it again. The plugin runs in its executable's directory, with its configured path as its first argument. A script finds its interpreter through its `#!` line as usual. A program that locates files next to itself through its own path (`/proc/self/exe`, `__file__`) finds the copy instead; it should use its working directory.
- **The environment** has, of the daemon's own environment, only `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TZ`, `TMPDIR`, `XDG_RUNTIME_DIR` and `XDG_CONFIG_HOME`, as an agent does. hataori adds:

  | Variable | Value |
  |---|---|
  | `HATAORI_SOCKET` | The control socket's path. |
  | `HATAORI_TOKEN` | The token of this start. Present it in the hello ([protocol.md](protocol.md)); it binds the connection to the plugin's grant. It is revoked when the plugin exits, and a new one is issued at the next start. |
  | `HATAORI_PLUGIN_ID` | The plugin's id. |
  | `HATAORI_PLUGIN_STATE` | Its state directory, `state/plugins/<id>/` (mode 0700), kept across starts and restarts of the daemon. Keep the event position here. |
  | `HATAORI_PLUGIN_SETTINGS` | A file holding the config entry's `settings`, a JSON object, written again at each start. |
  | `HATAORI_PROTOCOL` | The control protocol the daemon speaks, such as `3.12`. |

- **Output.** What the plugin writes to its standard output and standard error goes to the daemon's log, a line at a time. Each line is marked with the plugin's id and cut at 4096 bytes.
- **Restarts.** When a plugin exits, its token is revoked and its connections close. It starts again after 1 second, then 2, 4 and so on, up to 1 minute. A plugin that exits 5 times within 10 minutes stays stopped until the daemon restarts, and so does one whose hash or protocol does not hold. The owner then gets one direct message, in the conversation of the login alert, and the log has one line. While it stays stopped, also across restarts of the daemon, the owner is not told again; a run that lasts 10 minutes ends that.
- **Stopping.** When the daemon stops, each plugin gets SIGTERM, and after 10 seconds SIGKILL. Both go to its process group: whatever it started in that group goes with it. On Linux a plugin is also killed when the daemon dies without stopping it. Telling a plugin that it is stopping comes with hooks.
- **An `external` plugin** is not started. The token in its `token_file` is bound to its grant when the daemon starts, and the program connects with it by itself. A connection with any other token is refused, and the log does not show the token.

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
| `operations` | array of strings | empty | The control operations it calls, by their names in [protocol.md](protocol.md#operations), such as `Threads` and `Events`. `SignInLink`, `HostTerminals`, `HostScreen`, `OperateHost` (with `HostKeys` and `End`), `AttachTerminal`, `Close` and `Remove` are refused: no plugin gets them. |
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
