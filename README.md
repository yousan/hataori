# hataori

Run coding agents in parallel from chat.

One chat thread is one agent session, and one agent session owns one working directory.
Start a thread, and an agent gets its own clone to work in. Start ten threads, and ten agents work side by side without stepping on each other.

Chat surfaces (Discord, Slack, Microsoft Teams) and coding agents (Claude Code, Codex) plug into that core.

**Status:** early development. Nothing here is usable yet.
