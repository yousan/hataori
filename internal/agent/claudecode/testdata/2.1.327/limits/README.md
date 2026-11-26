# Usage snapshot

`statusline.json` is the existing scrubbed real Claude Code 2.1.327 status-line capture from `docs/notes/agent-usage-limits/captures/claude-statusline-with-rate-limits.json`, copied here for #923's regression tests. No new CLI run was made. Tests replay this captured shape and deliberately vary usage/reset values and JSON ordering to exercise freshness transitions; those variants are test scenarios, not additional captures.
