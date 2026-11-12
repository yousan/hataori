# Working on hataori

These rules are for anyone changing this repository: people, Claude Code, Codex.
They are about developing hataori. How a particular deployment runs hataori is not part of this repository.

## What hataori is

One chat thread is one agent session, and one agent session owns one working directory.
That core model does not change. Chat surfaces (Discord, Slack, Microsoft Teams) and coding agents (Claude Code, Codex) are adapters that plug into it.
Core code never names a specific surface or agent: it talks to them through interfaces. Anything that says Discord, Slack, Teams, Claude Code or Codex lives in that adapter's package.

## Language

English for code, comments, commit messages, issues, pull requests and docs.

## Workflow

- One issue per concern. Write acceptance criteria that are either met or not.
- One branch per issue, one pull request per branch. Reference the issue (`Closes #N`).
- Do not push to `main` directly.

## Build and test

Run all of these before you push. CI runs the same.

```sh
gofmt -l .          # must print nothing
go vet ./...
go test ./...
```

## Tests

- Code that reads what an agent or a chat service produced (terminal screens, event streams, API payloads) is tested against real captured output stored under `testdata/`, not strings written from memory. When the real output changes, add the new capture next to the old one.
- A test that passes with the fix reverted proves nothing. Check that it fails without the fix.

## Never lose work

A working directory may hold the only copy of someone's work: unpushed commits, untracked files, stashes, nested repositories, submodules.
Code that removes or resets a working directory must prove there is nothing left to lose first, and must refuse when it cannot tell.

## Issues

This repository tracks problems that any hataori user could hit.
Do not put deployment details in issues, pull requests, commits or code: no host names, absolute paths from your machine, disk sizes, user or channel IDs, tokens.
A problem that only happens on one deployment belongs in that deployment's own tracker.

## Layout

- `cmd/hataori/`: the command
- `internal/`: packages that are not a public API

## About this file

- Keep it short. It holds what every change needs and what the code does not tell you quickly.
- Rules for one package go in an `AGENTS.md` inside that package. Add a one-line pointer to it here.
- Procedures with several steps go in `docs/`. Link them from here instead of copying them.
- Do not repeat what gofmt, go vet or CI already enforce. State rules, not the history behind them.
- `CLAUDE.md` only imports this file. Do not add anything else to it: Claude Code reads only `CLAUDE.md` files when any exists in or above the working directory, so the import is what keeps Claude Code and Codex on the same rules.
