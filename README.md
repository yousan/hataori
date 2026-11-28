# hataori

Run coding agents in parallel from chat.

One chat thread is one agent session, and one agent session owns one working directory.
Start a thread, and an agent gets its own clone to work in. Start ten threads, and ten agents work side by side without stepping on each other.

Chat surfaces (Discord, Slack, Microsoft Teams) and coding agents (Claude Code, Codex) plug into that core.

## Status

hataori runs coding agents from Discord, with Claude Code and Codex adapters. Linux and macOS are covered by CI.

What works today:

- An order in an enabled Discord channel opens a thread, clones the repository into its own working directory, and runs Claude Code or Codex there. Messages in the thread continue the same session.
- Model and effort per order, with defaults per channel and agent.
- Replies in the thread: progress text, the final answer, a progress line while a tool runs long, subagent start and end lines, long replies split, and files the agent hands over.
- Questions in the thread, answered by buttons or a message; message status reactions, completion and attention pings, and a Stop button on the progress line. Ordinary agent sessions allow permissions automatically by default.
- Access for the owner and configured people or automated posters, checked per channel; guests are refused and logged.
- Agent sessions outlive the daemon: a restart does not stop running turns, and messages posted while the daemon was down are run once each, in order, after it starts.
- Idle sessions sleep and resume on the next message.
- When Claude Code is logged out, the owner gets a direct message and can relay the CLI's login code. Codex uses its existing CLI login; hataori does not run a Codex login.
- Discord commands to start, stop and inspect threads, attach chat to an existing thread, and open the dashboard; incoming attachments are handed to the agent.
- Host commands to inspect threads and events, stop turns, attach to or watch a live terminal, and close or reopen threads, over the local control socket.
- A web dashboard with threads, events and live terminals, including access over SSH with `hataori connect`.
- An optional supervisor for a channel, using Claude Code or the verified Linux Codex launch ([requirements](docs/notes/codex-supervisor-1088/README.md)): it watches turn results, keeps notes and reminders, reads repository issues and pull requests, and proposes or takes actions within configured access and budgets.
- External-process plugins with manifests, scoped grants and hooks.
- Workspace removal from the host, with checks that refuse when work could be lost or safety cannot be established; removed workspaces are kept temporarily for restoration. Optional cleanup applies the same checks to closed threads. Nothing in chat removes a workspace.

Not built yet:

- Slack (next) and Microsoft Teams (planned).
- An installer or release binaries; build from source, or use the macOS artifacts from CI.

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
     "terminal_size": {"cols": 160, "rows": 60},
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

### The dashboard from your own machine

`hataori connect` runs on your laptop and opens the dashboard of an instance on a server you reach by SSH: `hataori connect user@host --root <the instance root>`. It uses your own `ssh`, keeps the connection up and reconnects by itself, and remembers the host for next time. See [the configuration reference](docs/configuration.md#dashboard).

To get `hataori` on a Mac, either build it with Go (`go install github.com/yousan/hataori/cmd/hataori@latest`, which puts it in `$(go env GOPATH)/bin`), or take the build CI makes for every run: the `hataori-darwin` artifact of a run on `main` in the repository's Actions tab, `hataori-darwin-arm64` for Apple silicon and `hataori-darwin-amd64` for Intel. A downloaded binary needs `chmod +x`, and macOS may refuse to run it until you remove its quarantine mark (`xattr -d com.apple.quarantine hataori-darwin-arm64`).

## Documentation

- [Configuration reference](docs/configuration.md)
- [Design overview](docs/design/overview.md)
- [Control protocol](docs/protocol.md)
- [Testing](docs/testing.md)
- [Working on hataori](AGENTS.md)
- [Story-point estimation](docs/development/story-points.md)
