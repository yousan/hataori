# Testing against the real agent CLIs

`go test ./...` never runs a real agent CLI. It uses captures of real output under each adapter's `testdata/`, and fake binaries. The checks below run the installed CLI on purpose, and only when asked for.

## Claude Code

### Rules for every run

- Never touch the real login. Anything about login runs with `CLAUDE_CONFIG_DIR` set to a new, empty directory, and answers only with a wrong code. Never run `claude auth logout` or `claude setup-token`.
- Do not edit the user's `~/.claude/settings.json`, `~/.claude.json` or any project settings. Hooks are given with `--settings`.
- Run sessions in new, empty temporary directories, with the cheapest model and tiny turns. Stop every process you start.

### The login check

```sh
HATAORI_REAL_CLAUDE=1 go test -count=1 -run TestRealLogin -v ./internal/agent/claudecode
```

The test does the following:

1. It runs `claude auth login` through the adapter's `LoginLaunch` on pipes. `CLAUDE_CONFIG_DIR` points to a temporary directory, and the test refuses to start if it points anywhere else.
2. It reads the link and the request for a code with `LoginDecode`.
3. It sends `not-a-valid-code` with `LoginCode` and expects the refusal.
4. It kills the command and checks that no login was stored.

The real login is not involved. You can confirm it afterwards with `claude auth status`, which only reads.

### New captures

`internal/agent/claudecode/testdata/capture.py` records hook calls, transcripts and terminal output from the real CLI, and `scrub.py` makes them committable. Their README says how each capture was taken. Run `go test ./internal/agent/claudecode` before committing: it fails on a capture that still holds a home path, an e-mail address, an id or a token.

```sh
cd internal/agent/claudecode/testdata
python3 capture.py /tmp/raw login login_terminal    # or no names, for every scenario
python3 scrub.py /tmp/raw <version>-new
```
