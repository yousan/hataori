# Captures of `hataori mcp` with real clients

`claudecode/<version>/` holds one exchange of Claude Code with `hataori mcp`, taken by `capture.sh`:

- `client.jsonl`: what Claude Code wrote to the server, one JSON-RPC message per line.
- `server.jsonl`: what `hataori mcp` answered.
- `init.json`: the MCP part of Claude Code's init message: the server's status and the tool names it took.

Claude Code 2.1.328 first sends `server/discover`, which the server answers "method not found"; it then sends `initialize`, `notifications/initialized` and `tools/list`.

To take a new capture, run `./capture.sh claudecode` from this directory, with `claude` and `jq` installed and logged in. It makes one small model request, saves no session, and starts no daemon. Add the new directory next to the old ones and point the test at it.

# The events behind the wake digest

`wake/capture.json` holds the events of one scope, as a daemon with the fake agent and the fake surface wrote them, and each thread's title and last output as the waker reads them. In it, threads ended their turns in each way: completed, blocked on a question, failed with the reasons `limit` and `api_error`, interrupted, with a background task, with output over 1 KB, and threads with two turns. A person decided the supervisor's requests in each way: a proposed send approved and sent, one refused, one approved while its thread ran a turn, and an interrupt request approved. Thread 1 is the supervisor's own. The repository's and the root's paths are replaced by `/repo.git` and `/root`.

`wake/digest.txt` is the digest `TestDigestOfCapturedEvents` makes of it, with at most 6 threads. To take a new capture, run `HATAORI_CAPTURE_WAKE=$PWD/internal/supervisor/testdata/wake go test -run TestCaptureWakeEvents ./cmd/hataori` from the repository's root, then `go test -run TestDigestOfCapturedEvents ./internal/supervisor -update`, and read the new digest before committing it.
