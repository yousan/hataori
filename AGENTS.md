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
- Follow the ready-before-merge CI contract and staged policy in [docs/design/ci-policy.md](docs/design/ci-policy.md); draft skips are not full validation.
- Before every merge, the pull request's diff against the current `main` lists only the pull request's own changes.

## Claiming an issue

Several people and agents pick issues, and some agents share one GitHub account, so claim an issue before you work on it.

Before starting, check all three. If any fails, pick another issue or ask on it.

```sh
gh issue view N --json state --jq .state                      # must be OPEN
gh issue view N --json labels --jq '.labels[].name'           # must not list "in progress"
gh pr list --state open --search "N in:body" --json number,title   # must not show a pull request for #N
```

When starting:

1. Add the label: `gh issue edit N --add-label "in progress"`.
2. Comment who works on it: `gh issue comment N --body "Working on this: <who>"`. A person names themselves; an agent names its session or chat thread, not only the account.
3. Push the branch early and open a draft pull request that says `Closes #N`: `gh pr create --draft`.

When stopping without finishing, remove the label (`gh issue edit N --remove-label "in progress"`) and comment why, so the next person knows where it stands.

## Build and test

Run all of these before you push. CI runs the same.

```sh
gofmt -l .          # must print nothing
go vet ./...
go test ./...
```

## Tests

The core and the adapter interfaces are still moving, so do not write broad unit tests yet.
Until hataori runs real work for the first time, only these tests are required:

- Code that reads what an agent or a chat service produced (terminal screens, event streams, API payloads) is tested against real captured output stored under `testdata/`, not strings written from memory. When the real output changes, add the new capture next to the old one.
- Code that removes, resets or overwrites a working directory is tested (see "Never lose work").
- One end-to-end test: with a fake chat surface and a fake agent, open one thread, run one turn, and check that the turn finishes.

Write the full test suite before hataori runs real work for the first time.

A test that passes with the fix reverted proves nothing. Check that it fails without the fix.

## Never lose work

A working directory may hold the only copy of someone's work: unpushed commits, untracked files, stashes, nested repositories, submodules.
Code that removes or resets a working directory must prove there is nothing left to lose first, and must refuse when it cannot tell.

## Issues

This repository tracks problems that any hataori user could hit.
Do not put deployment details in issues, pull requests, commits or code: no host names, absolute paths from your machine, disk sizes, user or channel IDs, tokens.
A problem that only happens on one deployment belongs in that deployment's own tracker.

## Protocol

The control socket's messages and the events are a public format. Change them only as [docs/protocol.md](docs/protocol.md) says, with its version and golden files.

## Layout

- `cmd/hataori/`: the command
- `internal/`: packages that are not a public API
- `web/`: the dashboard's page; its build is committed. Rules: [web/AGENTS.md](web/AGENTS.md)

## About this file

- Keep it short. It holds what every change needs and what the code does not tell you quickly.
- Rules for one package go in an `AGENTS.md` inside that package. Add a one-line pointer to it here.
- Procedures with several steps go in `docs/`. Link them from here instead of copying them.
- Do not repeat what gofmt, go vet or CI already enforce. State rules, not the history behind them.
- `CLAUDE.md` only imports this file. Do not add anything else to it: Claude Code reads only `CLAUDE.md` files when any exists in or above the working directory, so the import is what keeps Claude Code and Codex on the same rules.
