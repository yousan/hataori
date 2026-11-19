#!/bin/sh
# capture.sh OUT_DIR: captures the MCP exchange of Claude Code with
# `hataori mcp`, up to tools/list, into OUT_DIR/<claude version>/.
#
# It builds hataori into a temporary directory, runs `claude -p` once with
# only `hataori mcp` as its MCP server and no built-in tools, and records
# what each side wrote: client.jsonl (Claude Code to hataori mcp),
# server.jsonl (hataori mcp to Claude Code), and init.json, the MCP part of
# Claude Code's init message (the server's status and its tool names).
# No daemon runs: initialize and tools/list do not reach one. It makes one
# small model request ("Reply with ok.") and saves no session.
set -eu
out=$1
repo=$(cd "$(dirname "$0")/../../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
(cd "$repo" && go build -o "$work/hataori" ./cmd/hataori)
cat > "$work/server.sh" <<SH
#!/bin/sh
tee -a "$work/client.jsonl" | "$work/hataori" mcp --scope C0123 --root "$work/root" | tee -a "$work/server.jsonl"
SH
chmod +x "$work/server.sh"
cat > "$work/mcp.json" <<JSON
{"mcpServers": {"hataori": {"command": "$work/server.sh", "env": {"HATAORI_TOKEN": "capture-token-not-issued"}}}}
JSON
version=$(claude --version | cut -d' ' -f1)
mkdir -p "$out/$version"
(cd "$work" && claude -p "Reply with ok." --mcp-config "$work/mcp.json" --strict-mcp-config --tools "" \
	--no-session-persistence --output-format stream-json --verbose < /dev/null > "$work/stream.jsonl")
head -n1 "$work/stream.jsonl" | jq -c '{mcp_servers, tools: [.tools[] | select(startswith("mcp__"))]}' > "$out/$version/init.json"
# hataori's version in serverInfo is the build's; the test does not compare it.
cp "$work/client.jsonl" "$work/server.jsonl" "$out/$version/"
echo "captured into $out/$version"
