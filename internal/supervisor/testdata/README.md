# Captures of `hataori mcp` with real clients

`claudecode/<version>/` holds one exchange of Claude Code with `hataori mcp`, taken by `capture.sh`:

- `client.jsonl`: what Claude Code wrote to the server, one JSON-RPC message per line.
- `server.jsonl`: what `hataori mcp` answered.
- `init.json`: the MCP part of Claude Code's init message: the server's status and the tool names it took.

Claude Code 2.1.328 first sends `server/discover`, which the server answers "method not found"; it then sends `initialize`, `notifications/initialized` and `tools/list`.

To take a new capture, run `./capture.sh claudecode` from this directory, with `claude` and `jq` installed and logged in. It makes one small model request, saves no session, and starts no daemon. Add the new directory next to the old ones and point the test at it.
