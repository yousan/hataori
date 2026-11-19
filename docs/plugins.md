# Plugins

A plugin is a separate program, in any language, that adds to what hataori does without changing it ([design, section 7.3](design/overview.md#73-plugins)). It talks to the daemon over the control socket ([protocol.md](protocol.md)). The operator lists it in the instance's config ([configuration.md, `plugins[]`](configuration.md#plugins)); this file describes what the plugin's author writes, its manifest.

This version reads and checks each plugin's manifest and config entry when the daemon starts. It has the hook calls and the dispatcher that runs them (below, [protocol.md, Requests the daemon sends](protocol.md#requests-the-daemon-sends)); the core and the chat front end do not call a hook point yet.

## What a plugin's connection may do

Once hataori starts plugins, a plugin's connection is bound, by a token the daemon issues, to a caller made from its grant: what the manifest asks for and the config grants, and nothing more. The core refuses any other operation with `forbidden` and one log line naming the operation, the scope and the plugin. Events of kinds it was not granted are left out of `Events`, `ThreadEvents` and `Thread`; positions are the instance's own, so a plugin that reads again after the last position it kept misses no event of its kinds. `Threads` and `Thread` still summarize a thread's state, such as its turns, usage and open requests, whatever kinds were granted. Without the `conversation` grant the text is left out of what it reads, and `Terminal` is refused.

A plugin calls as the id `plugin:<id>`. Its `Start`, `Send`, `Answer`, `Stop` and `Link` are refused like a guest's unless the operator lists that id under a person of the config and that person in the scope's `people` ([configuration.md, `people[]`](configuration.md#people)), as for a supervisor. `AskInterrupt` is refused in this version whatever the grant: a plugin has no thread to ask in.

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

### How a hook is called

Since protocol 3.16. At a point, the daemon sends a `hook` request to each plugin whose grant has the point and the thread's scope, one after another: by priority, higher first, then in the config's order. Each gets the text as the one before left it, and answers `continue`, `continue` with a new `text` where the point may change it, or `stop` with a `reason` where the point may stop it. Without the `conversation` grant the request has no text.

A handler fails, and its policy (`block` or `skip`) applies, when:

- it does not answer within its timeout, 15 seconds unless the manifest or the config says otherwise;
- its plugin is not connected, while it starts, starts again or stays stopped: so by default a `before_agent_run` handler blocks input in its scopes until the plugin is back. An operator who prefers that work goes on sets that plugin's `on_failure` to `skip`;
- its connection closes before it answers;
- it answers with an error, `not_supported` too;
- its answer is one the point does not allow, such as `stop` at `before_prompt_build`.

The daemon sends hook requests on the plugin's oldest connection that is still open and is not a stream: open a connection for requests first, and follow events on others. The project's Go client answers them with the handler given to `wire.DialTokenHandler`. When a plugin connects while it had no connection open, the daemon sends it `plugin_started` with its grant; before hataori stops a plugin it starts, it is to send `plugin_stopping` (with the starting of plugins, #349). Each call is logged with the plugin, the point, the thread and the outcome, never with text or a plugin's reason.

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
