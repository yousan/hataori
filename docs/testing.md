# Testing

## Phase 1 acceptance criteria and their tests

Every acceptance criterion of phase 1 ([design, section 8](design/overview.md#phase-1-replace-c-lord-for-dispatched-orders-discord-and-claude-code)), and the tests that cover it with the fakes. `e2e` tests are in `cmd/hataori` and run the daemon (in the test process, or as a process of its own where a test kills it), a session host, the fake agent and the fake surface; the others are named with their package. A criterion that needs the real services says so, and why.

| Criterion | Covered by | Needs the real services |
|---|---|---|
| The full test suite exists before the first real order: the codec against captures for every scenario in Appendix B, and end-to-end tests for each criterion that does not need the real services | this table; `internal/agent/claudecode`: `TestEachCapture` and the codec tests beside it; `TestCapturesAreScrubbed` | |
| A webhook post with the trigger, a repository and a prompt in an enabled channel creates a thread, a clone under `workspaces/<client>/<channel>/<thread>/` and a first reply; the full prompt is in the thread; editing the channel message changes nothing | e2e `TestOneThreadOneTurn`, `TestTwoInstancesIgnoreEachOthersScopes` (the clone's place); `internal/chat`: `TestTriggerStartsAThread`, `TestRedeliveryAfterACrashShowsTheFirstText` | Discord's webhook and thread calls: the driver's own tests use its fakes |
| The same post in a channel that is not enabled, or from an account that is not allowed, starts nothing and writes one log line | e2e `TestRefusedTriggersStartNothingAndLogOneLine` | |
| A webhook post into the thread continues the same agent session id | e2e `TestFollowUpContinuesTheSameAgentSession` | |
| A thread in a repository never opened on the host starts with nobody at the terminal; a session that does not report its start within the timeout is `failed` with its terminal output | `internal/runner`: `TestBlockedAtStart` | the folder-trust dialog is the real CLI's (#56) |
| `model:` and `effort:` on the trigger line are the ones the agent reports, and the next thread without them gets the channel's defaults | e2e `TestModelAndEffortReachTheAgentAndTheNextThreadGetsTheDefaults`; `internal/chat`: `TestModelAndEffortFromTheTriggerLineOrTheDefaults` | what the real CLI reports (#62) |
| A turn of 30 minutes ends with its final answer posted once | e2e `TestLongTurnEndsWithItsAnswerPostedOnce` (half an hour on the front end's clock) | a real 30-minute turn: only the real agent and service show their own timeouts |
| Every turn, including one the agent starts by itself after a background task, produces exactly one end event, with who started it and how many background tasks still run | e2e `TestTurnTheAgentStartsHasOneEndEvent`; `internal/agent/claudecode`: `TestWhoStartedATurn` | |
| `hataori events --after <n>` returns exactly the events after `n`, also after the daemon was restarted | `cmd/hataori`: `TestCommands`; e2e `TestEventsAfterNOnceTheDaemonWasRestarted` | |
| A question from the agent appears in the thread, produces a `blocked` event, and is answered by the next message | e2e `TestQuestionIsShownBlocksAndTakesTheNextPost`; `internal/agent/claudecode`: `TestQuestionBecomesARequest` | |
| A usage limit, an expired login and a crashed agent each produce one line in the thread and one `failure` event with that reason | e2e `TestLimitLoginAndCrashEachGiveOneLineAndOneFailure`; `internal/agent/claudecode`: `TestFailureReasons` | recorded from the real CLI when they occur; simulated until then |
| While a tool runs for more than 90 seconds, the thread shows one progress line, updated in place and removed at the end | e2e `TestProgressLineAppearsIsEditedAndRemoved`; `internal/chat`: `TestProgressLineAppearsIsEditedAndRemoved`, `TestNoSecondLineWhileTheFirstIsNotDeleted` | |
| `hataori thread <key>` tells a hataori thread from an ordinary one, and gives its state | e2e `TestThreadCommandTellsAHataoriThread`; `cmd/hataori`: `TestCommands` | |
| `hataori stop <thread>` ends a running command within 10 seconds and leaves no process of the agent's behind | e2e `TestStopEndsARunningToolWithinTenSeconds`; `internal/runner`: `TestStopEndsARunningTool` | |
| The daemon is killed with SIGKILL during a turn and started again: the turn finishes, every reply appears once, and its end event once | e2e `TestDaemonKilledDuringATurnFinishesIt` (the daemon in a process of its own, killed with SIGKILL); `internal/runner`: `TestDaemonKilledDuringATurn` | |
| Three posts into a thread while the daemon is down are run once each, in order, after it starts | e2e `TestThreePostsWhileTheDaemonIsDownRunOnceEachInOrder`; `internal/chat`: `TestThreeMessagesWhileDownRunOnceEachInOrder`, `TestCursorStaysWhenAFailureComesBetween` | |
| After the idle period the agent process is gone; the next post resumes the same conversation. The same after all processes were killed | e2e `TestIdlePeriodEndsTheAgentAndTheNextPostResumes`, `TestEverySessionProcessKilledThenTheNextPostResumes`; `internal/runner`: `TestIdleSessionSleepsAndResumes`, `TestRebootThenResume` | |
| When turns fail because the agent is logged out, the owner gets one direct message for the outage, also across a daemon restart, and threads that stop later are added to it | e2e `TestLoginOutageWithTheFakes`; `internal/chat`: `TestOneDirectMessagePerOutage`; `internal/session`: `TestLoginOutageSurvivesRestart` | |
| The owner's reply with the code, and nothing else, reaches the login command; on success the message says so and each stopped thread gets one line; no message is resent | e2e `TestLoginOutageWithTheFakes`; `internal/chat`: `TestDirectMessagesThatAreNoCode`; `internal/relay`: `TestLoginThroughTheRelay`, `TestCodeOnlyWhenAskedAndOneWord` | a real login: `TestRealLogin` below runs the real command with a wrong code only |
| After a login through the relay, the sign-in link and the code are found nowhere in the database, the event log or the log files | e2e `TestLoginOutageWithTheFakes` (every table and every file under the root) | |
| No code path in the binary removes or resets a working directory | `cmd/hataori`: `TestRemovalsAreAllowListed` | |
| Two instances with different roots on one host each ignore the other's channels | e2e `TestTwoInstancesIgnoreEachOthersScopes` | |
| No core package imports a driver, an agent adapter or a client, or contains the name of a surface or an agent (checked in CI) | `cmd/hataori`: `TestAdaptersOnlyUnderCmd`; `internal/chat`: `TestNoSurfaceOrAgentNames`; and the fakes stay out of the binary: e2e `TestTheBinaryHasNoFakes` | |
| For one week, every order the deployment's manager sends runs on hataori and none on c-lord | | yes: it is the deployment itself |

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

### The phase 1 check

```sh
HATAORI_REAL_CLAUDE=1 go test -count=1 -run TestRealClaudeCode -v -timeout 50m ./cmd/hataori
```

It runs `hataori run` as a process of its own, with the Claude Code adapter registered in the binary, the fake chat surface, and the installed CLI. It takes about 13 minutes, most of it one turn that waits on a command. Each criterion of #62 is one named step:

1. **First start in a new clone.** A trigger clones a new local repository into a new temporary directory. The adapter answers the folder-trust dialog, and the first turn's answer is posted. The wrapper adds the full-screen interface (`tui: fullscreen`) to a copy of the adapter's `--settings` file; the step fails if the terminal shows no full-screen interface, or shows the first paste echoed by the terminal (written before the CLI read keys, #178).
2. **A turn.** That turn ends once, as a completion by the caller.
3. **A follow-up with the same session id.** A message in the thread runs in the same agent session.
4. **A question and its answer.** `AskUserQuestion` is posted in the thread and the turn ends `blocked`. The next message is recorded as the answer, and the agent's reply uses it.
5. **An interrupt.** `hataori stop` during a command ends the turn `failure`, `interrupted`.
6. **A turn of 10 minutes.** Two `python3` sleeps of 5 minutes in the foreground. The turn ends once, after more than 10 minutes.
7. **A turn the agent starts by itself.** A background `sleep 45`: the caller's turn ends with one task running, then one turn started by the agent completes with none.
8. **Killing the daemon during a turn.** SIGKILL to the daemon during a command, then a new daemon. The answer is posted once, and the turn ends once.
9. **Sleep and resume.** With a 20-second idle period, the agent is stopped (`asleep`, reason `idle`). The next message resumes the same conversation: the agent still knows a word from before.
10. **Nothing left running, nothing left in the workspace.** After everything is stopped, no session process runs, and `git status --ignored` in the workspace is empty.
11. **The user's settings file unchanged.** The settings file (`settings.json` in `$CLAUDE_CONFIG_DIR`, or `~/.claude`) has the same modification time and checksum as before.

Steps 10 and 11 run even when an earlier step failed. A failed step logs the thread's events, its posts and the end of the daemon's log, because the temporary directory is removed afterwards.

The CLI runs through a wrapper (`realWrapper` in the test): `--setting-sources project,local`, and `CLAUDE_CODE_DISABLE_CLAUDE_MDS=1`, `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1` and `ENABLE_CLAUDEAI_MCP_SERVERS=false`. The user's settings, hooks, plugins, CLAUDE.md, memory and connectors stay out. The adapter adds its own `--settings` (its hooks) and the model `haiku`.

What that leaves out: a person's own settings change how the CLI starts. #178 (the first input of every new thread never submitted) passed this check on 2.1.326 because the settings that bring it on were never loaded. With `tui: fullscreen` in a person's settings, the CLI reports `SessionStart`, takes its screen down, and only then starts reading keys in the full-screen interface; input written in between reaches the terminal's line discipline, and its Enter becomes a new line. The check now brings the full-screen interface in, through the wrapper's copy of the adapter's settings file. It does not put it in the clone's own `.claude/settings.json`: the first try did, and the folder-trust dialog was never answered (the run kept too little of the screen to show why). The rest of a person's settings (a status line, their hooks, plugins, Remote Control) still stay out, because the check must not read or change `~/.claude`: a start with those is covered only by the captured start in `testdata/2.1.327/first_enter_lost` and by a smoke test of a real deployment.

The login is left alone. The check runs the CLI with whatever login it has, the same way for every kind of login: it passes the CLI nothing about the login and switches the login relay off. `TestRealCheckLeavesTheLoginAlone`, which runs in CI, fails if the check's source names a credential, a key, an account file, a login command or a kind of login, or names the config directory for anything but finding the settings file it compares. A key given only in the environment is not passed on (the adapter passes `pass_env` variables only, and the check sets none), so a login like that needs `pass_env` in a real config, not in this check.

Last passed: 2026-11-17, Claude Code 2.1.326, logged in with a subscription. The maintainer's machine has that login; the check does not look. All 11 steps passed in 12.6 minutes, and the 10-minute turn took 10 m 12 s.

Last run: 2026-11-18, Claude Code 2.1.327, logged in with a subscription, with the full-screen interface (#178). Steps 1 to 5 passed, the first start's answer after 11 s; the terminal showed the full-screen interface and no echoed paste. Step 6 failed on the answer only: the turn ran both sleeps one after the other and ended `completion` after 10 m 9 s, but the model answered "long done" followed by a note, and the step wants exactly "long done". Steps 7 to 9 were skipped; 10 and 11 passed.

### New captures

`internal/agent/claudecode/testdata/capture.py` records hook calls, transcripts and terminal output from the real CLI, and `scrub.py` makes them committable. Their README says how each capture was taken. Run `go test ./internal/agent/claudecode` before committing: it fails on a capture that still holds a home path, an e-mail address, an id or a token.

```sh
cd internal/agent/claudecode/testdata
python3 capture.py /tmp/raw login login_terminal    # or no names, for every scenario
python3 scrub.py /tmp/raw <version>-new
```
