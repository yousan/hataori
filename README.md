# hataori

Run coding agents in parallel from chat.

One chat thread is one agent session, and one agent session owns one working directory.
Start a thread, and an agent gets its own clone to work in. Start ten threads, and ten agents work side by side without stepping on each other.

Chat surfaces (Discord, Slack, Microsoft Teams) and coding agents (Claude Code, Codex) plug into that core.

## Status

hataori is in phase 1 of [the design](docs/design/overview.md#8-phases): Discord and Claude Code, for orders that are posted into a channel, by a person or through a webhook. It runs real work every day in that form.

What works today:

- An order in an enabled Discord channel (`<trigger> repo:<url> <prompt>`) opens a thread, clones the repository into a working directory of its own, and runs Claude Code there. Messages in the thread continue the same session.
- Model and effort per order, with defaults per channel.
- Replies in the thread: progress text, the final answer, a progress line while a tool runs long, subagent start and end lines, long replies split, files the agent hands over.
- A question from the agent is shown in the thread, and the next message answers it. Permission requests are allowed automatically.
- Only the owner and the configured posters (people or webhooks) are accepted; everything else is ignored and logged.
- Agent sessions outlive the daemon: a restart does not stop running turns, and messages posted while the daemon was down are run once each, in order, after it starts.
- Idle sessions are put to sleep and resumed by the next message.
- When Claude Code is logged out, the owner gets one direct message, and can log in again by replying with the code.
- `hataori threads`, `thread`, `events` and `stop` on the host, over a local control socket ([protocol](docs/protocol.md)).
- hataori never removes a working directory.

Not built yet:

- Slack and Microsoft Teams.
- Codex.
- The supervisor (coordinator) agent.
- The dashboard.
- Plugins.
- For people in chat: slash commands, buttons for questions, lamps, pings, a Stop button, attaching a terminal to a live session, sending attachments to the agent, and removing workspaces.

## Getting started

### Requirements

- Linux or another Unix. hataori runs agents on pseudo-terminals and talks to them over Unix sockets.
- Go 1.27.4 or later (from [go.mod](go.mod)), to build.
- `git` on `PATH`. Repositories are cloned with prompts switched off, so private ones need credentials that work without a prompt (an SSH key, a credential helper).
- Claude Code installed as `claude` on `PATH` and logged in, as the user that will run hataori. Check with `claude auth status`. hataori uses whatever login the CLI has; it never handles the login itself.
- A Discord application with a bot user:
  - the **Message Content** privileged intent switched on in the Developer Portal (hataori also uses the Guilds, Guild Messages and Direct Messages intents, which need no switch);
  - invited to your server with these permissions in the channel it serves: View Channel, Read Message History, Create Public Threads, Send Messages in Threads, Attach Files.

### From a clean clone to one channel taking orders

1. Build:

   ```sh
   git clone https://github.com/yousan/hataori.git
   cd hataori
   go build -o hataori ./cmd/hataori
   ```

2. Make an instance root. This example uses `$HOME/hataori-instance`; any directory works.

   ```sh
   export HATAORI_ROOT="$HOME/hataori-instance"
   mkdir -p "$HATAORI_ROOT/config"
   chmod 700 "$HATAORI_ROOT/config"
   ```

3. Put the bot token in a file in `config/`:

   ```sh
   printf '%s\n' '<bot token>' > "$HATAORI_ROOT/config/discord-token"
   chmod 600 "$HATAORI_ROOT/config/discord-token"
   ```

4. Write `$HATAORI_ROOT/config/hataori.json` and `chmod 600` it. With Discord's Developer Mode on, copy the ids from the context menus.

   ```json
   {
     "owner": "<your Discord user id>",
     "trigger": "!hataori",
     "idle": "30m",
     "scopes": [
       {"key": "<channel id>", "client": "discord", "enabled": true}
     ],
     "drivers": {
       "discord": {"bot_id": "<the bot's user id>", "token_file": "discord-token"}
     },
     "adapters": {
       "claude-code": {}
     }
   }
   ```

   To let a webhook post orders, add its id to the scope's `posters`. Every option is in [the configuration reference](docs/configuration.md).

5. Run the daemon. It stays in the foreground and logs to standard error; run it under your service manager to keep it up.

   ```sh
   ./hataori run
   ```

6. In the channel, post an order:

   ```
   !hataori repo:https://github.com/<owner>/<repo>.git
   Read the README and summarise what this project does.
   ```

   hataori opens a thread on the message and the agent's replies appear there. Post into the thread to continue. From another terminal, `./hataori threads` lists the threads and `./hataori events --follow` shows the turns as they start and end.

## Documentation

- [Configuration reference](docs/configuration.md)
- [Design overview](docs/design/overview.md)
- [Control protocol](docs/protocol.md)
- [Testing](docs/testing.md)
- [Working on hataori](AGENTS.md)
