#!/bin/sh
# poll-agents.sh <out.jsonl> <seconds>: every 0.5 s, append {ts, agents} from `claude agents --json --all`.
out=$1; n=$2; CL=${CLAUDE_BIN:-$HOME/.local/bin/claude}
end=$(( $(date +%s) + n ))
while [ "$(date +%s)" -lt "$end" ]; do
  t=$(date +%s.%N)
  j=$(env -i HOME="$HOME" PATH="$PATH" "$CL" agents --json --all 2>/dev/null | /usr/bin/python3 -c "import json,sys; print(json.dumps(json.load(sys.stdin)))" 2>/dev/null || echo null)
  printf '{"ts": %s, "agents": %s}\n' "$t" "$j" >> "$out"
  sleep 0.5
done
