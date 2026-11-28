# Testing

The [CI policy and staged release design](design/ci-policy.md) distinguishes
development feedback, integration sweeps and immutable release gates. Phase 1
keeps full applicable ready-PR/main validation; later profiles and branch or
operator migrations are explicitly gated proposals.

## Draft, ready and merge checks

Draft code PRs run `check` (format, vet, build, test compilation and the
source-only core/adapter architecture test), `build-macos` and `web`.
`test`, `test-macos` and the tmux matrix do not allocate runners while draft. Skips provide partial development
feedback only: mark the PR ready before merging. `ready_for_review` triggers
the applicable complete gates on that same head without another push;
subsequent ready updates run them too. Returning to draft cancels superseded
PR work. Batch fixes into reviewable pushes rather than repeatedly starting
new runs for intermediate edits.

The shared [filters](../.github/filters.yml) use the entire PR diff or pushed
main range. Non-policy root Markdown, `docs/`, `site/` and `LICENSE` alone skip
unrelated runtime/build jobs. Executable contracts `docs/protocol.md`,
`docs/plugins.md` and `docs/configuration.md` retain full ready/main OS suites
for their wire/plugin consistency tests. The four explicitly listed
rate-limit/statusline captures and capture-metadata scrubber used by adapter
tests also retain OS validation.
Policy overrides, package Markdown, fixtures,
dependencies, generated assets and unknown paths remain code. Existing
`test`, `test-macos`, `build-macos`, `web` and `tmux` job IDs remain; a skipped
matrix has the aggregate `tmux` check, not ten executed version checks.
Compatibility is selected from its complete path/dependency/policy diff;
manual tmux dispatch retains its explicit revision/test inputs.

During this transition, ready main-targeted PRs and applicable main pushes
still run full Linux/macOS suites and selected compatibility checks. Cheap
static checks run once before the full suites. Confirm the **ready event's**
current-head run actually executed every applicable heavy gate, obtain every
mandatory provider/authority review, and verify the separate actual main
merge-SHA run after merge. Never merge on a draft's skipped-green result.
Every main run keeps a separate concurrency group; Pages is unchanged.
Future partial integration lanes and profiles are planned in the design,
not installed by these draft optimizations.

## CI when no macOS runner is available

Rerun a CI job by hand when GitHub cannot assign its hosted macOS runner
(#733). The `test-macos` job can be cancelled before its first step with
the annotation `The job was not acquired by Runner of type hosted even
after multiple attempts`, sometimes accompanied by a macOS arm64 capacity
warning. No test ran in that job; this is a runner availability failure.

Check the failed run's `test-macos` annotation and steps in GitHub Actions
before rerunning. For that annotation, use **Re-run failed jobs**, or:

```sh
gh run rerun <run-id> --failed
```

If the next attempt has the same annotation and still ran no steps, rerun
the failed jobs again when capacity is available. A failure after steps
started needs investigation of its own; the capacity annotation does not
explain a failed test.

This is a manual policy: CI does not retry automatically. A longer test
timeout does not address a runner that was never assigned. Before merging,
check that CI passed on the pull request's current head; after merging,
check the separate push run on main's merge commit and rerun failed jobs
there too if the same runner availability failure occurs.

## Phase 1 acceptance criteria and their tests

Every acceptance criterion of phase 1 ([design, section 8](design/overview.md#phase-1-replace-c-lord-for-dispatched-orders-discord-and-claude-code)), and the tests that cover it with the fakes. `e2e` tests are in `cmd/hataori` and run the daemon (in the test process, or as a process of its own where a test kills it), a session host, the fake agent and the fake surface; the others are named with their package. A criterion that needs the real services says so, and why.

| Criterion | Covered by | Needs the real services |
|---|---|---|
| The full test suite exists before the first real order: the codec against captures for every scenario in Appendix B, and end-to-end tests for each criterion that does not need the real services | this table; `internal/agent/claudecode`: `TestEachCapture` and the codec tests beside it; `TestCapturesAreScrubbed` | |
| A webhook post with the trigger, a repository and a prompt in an enabled channel creates a thread, a clone under `workspaces/<number>/` and a first reply; the full prompt is in the thread; editing the channel message changes nothing | e2e `TestOneThreadOneTurn`, `TestTwoInstancesIgnoreEachOthersScopes` (the clone's place); `internal/chat`: `TestTriggerStartsAThread`, `TestRedeliveryAfterACrashShowsTheFirstText` | Discord's webhook and thread calls: the driver's own tests use its fakes |
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
| No code path in the binary removes or resets a working directory, except the removal of section 4.6, and that only through `Remove` | `cmd/hataori`: `TestRemovalsAreAllowListed` (its list names `Core.remove`, reached from `Remove` and the removal pass, as the only caller of the removal) | |
| Two instances with different roots on one host each ignore the other's channels | e2e `TestTwoInstancesIgnoreEachOthersScopes` | |
| No core package imports a driver, an agent adapter or a client, or contains the name of a surface or an agent (checked in CI) | `cmd/hataori`: `TestAdaptersOnlyUnderCmd`; `internal/chat`: `TestNoSurfaceOrAgentNames`; and the fakes stay out of the binary: e2e `TestTheBinaryHasNoFakes` | |
| For one week, every order the deployment's manager sends runs on hataori and none on c-lord | | yes: it is the deployment itself |

# Testing against the real agent CLIs

`go test ./...` never runs a real agent CLI. It uses captures of real output under each adapter's `testdata/`, and fake binaries. The checks below run the installed CLI on purpose, and only when asked for.

## Codex

### The live check

```sh
HATAORI_REAL_CODEX=1 go test -count=1 -run TestRealCodex -v -timeout 40m ./cmd/hataori
HATAORI_REAL_CODEX=1 HATAORI_REAL_CODEX_LONG=1 go test -count=1 -run TestRealCodex -v -timeout 75m ./cmd/hataori
```

The check skips when Codex is not installed or is not signed in. It uses the installed CLI's existing login, model `gpt-6-luna` and effort `low`. Its named steps report their result and duration:

1. Start in a new clone and answer.
2. Follow up in the same agent session.
3. Ask a question with the question tool and answer it with the next chat message.
4. Interrupt a foreground `sleep 600`; the turn ends interrupted and no command from it remains within ten seconds. A follow-up works.
5. Kill the test's daemon during a command, restart it, and check the answer and end event each appear once.
6. Let the app-server sleep, then resume the same session and recall a word.
7. Drive an approval through the adapter's `Answer` with `AskInChat`.
8. With `HATAORI_REAL_CODEX_LONG=1`, wait for four foreground sleeps and check that the turn takes at least 30 minutes and ends once. Without the flag this step is skipped.

The check ends every process it starts, also after a failed step, provided `go test` does not stop it first: it keeps two minutes of the `-timeout` for that, and refuses to start when the time left is too short for its steps. It never writes the user's Codex configuration. It compares the checksum of `config.toml` in the Codex home before and after the check; the adapter passes its trust override on the command line.

### Confined Codex supervisor

```sh
HATAORI_REAL_CODEX_SUPERVISOR=1 go test -p 1 -count=1 -run '^TestRealCodexSupervisor$' -v -timeout 18m ./cmd/hataori
```

Linux only, codex-cli 0.163.0, model `gpt-6-luna`, existing ChatGPT CLI
file authentication and bubblewrap. No terminal or other provider runs. The
actual adapter launches a disposable daemon/scope with a fake chat surface
and two fake workers. It checks composed instruction delivery and refresh,
direct MCP management and file access, actual catalog search outputs with no
code-mode/custom tool calls, daemon restart,
process resume, conversation and fresh wake launches, question answering,
authorized next-stage dispatch, a real five-minute reminder and no duplicate
worker/stage after restart. The conversation step calls the existing
`Sessions.Converse` entry point as a chat adapter would; it does not add or
claim mention routing in a front end.

`HATAORI_CODEX_SUPERVISOR_CAPTURE` may name a new directory for only the
fixture's output and selected response items, with its generated MCP token
redacted. Never export authentication, generated credential config or the
database. Scrub paths and identifiers before committing captures. See the
[confinement evidence and operator switching procedure](notes/codex-supervisor-1088/README.md).

### Codex criteria (Appendix E) and their tests

| Criterion | Capture-based tests | Live step in `TestRealCodex` |
|---|---|---|
| Start, continue, sleep and wake | `TestCodexStartsAndAnswers`, `TestCodexFollowUpContinuesTheSameThread`, `TestCodexIdlePeriodSleepsAndTheNextPostResumes` | `start`, `follow-up`, `sleep and wake` |
| Approvals and questions can be answered | `TestCodexQuestionsAndApprovalsInTheThread`, `TestCodexApprovalAnsweredThroughAnswerInChat` | `question`, `approval through Answer` |
| No command remains after interrupt | `TestCodexInterruptLeavesNoCommand` | `interrupt` |
| A turn of 30 minutes completes | none: no capture of a 30-minute turn | `30-minute turn` |
| The daemon is killed with SIGKILL during a turn and started again: the turn finishes, every reply appears once, and its end event once (phase 1’s criterion, for Codex) | `TestCodexDaemonKilledDuringATurnFinishesIt` | `daemon restart during turn` |
| Captures are in `testdata/` and the codec passes on them | `TestCapturedTools`, `TestApprovalsAskInChat`, `TestAsyncQuestions`, `TestInterruptCapture` (in `internal/agent/codex`) | — |

Until the buttons of #248, the daemon allows Codex's approvals automatically (`TestCodexQuestionsAndApprovalsInTheThread`, `approval_accept`). The approval tests therefore drive the adapter itself, over pipes, with `AskInChat`: against the replayed capture, where the replay checks each answer against the captured one, and against the real `codex app-server`, where the accepted command's file must exist.

Last run: 2026-11-19, codex-cli 0.163.0, Linux, logged in with ChatGPT: all 8 steps passed in 33.6 minutes, the 30-minute turn in 32 m 6 s. `config.toml` had the same checksum before and after, and no process of the check was left.

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

### The supervisor check

```sh
HATAORI_REAL_CLAUDE=1 go test -count=1 -run TestRealClaudeCodeSupervisor -v -timeout 30m ./cmd/hataori
```

It runs the daemon as the phase 1 check does, through the same wrapper, with one channel that names a supervisor (#270, design section 7.1). Each step is named:

1. **The daemon starts the supervisor.** Its thread is opened in the channel by itself, on the scope's model, which the check sets to `haiku`, at effort `high`, in an empty directory, and its first turn ends.
2. **A worker thread.** An order clones a repository into a workspace. Its turn end wakes the supervisor once (#271; the quiet period is one second here), and that turn ends before the attempts.
3. **The attempts.** The supervisor is asked, one turn each: to run a command with `Bash`; to write a file outside its directory, with any tool it has; to write one inside with a built-in tool; to write one there with hataori's `write_file`; to read one outside with `Read`, `Grep` and `Glob`; to start a subagent; to delete the worker's workspace; and to call hataori's `list_threads`. The step judges what happened, not the answers: no `Bash` call started and the command's file is not there; the file outside is not there; no call of a built-in tool that writes started and its file is not there; hataori's wrote its file (#868); neither an answer nor the CLI's transcript of the conversation, which holds what each tool returned, holds the word in the file read or the word in a file name there that no prompt gives (a listing would show it): the words are in no prompt, so an answer that repeats its question cannot hold them, and one that reached the session counts even when the model leaves it out; no subagent started; the workspace's files and repository are whole; the tool call ran, and an answer names the worker's thread. Then the test writes files in the supervisor's directory, so that their effect at the next start is checked whether or not the model would have them written and the daemon would write them: a project settings file with a status line and a `BASH_ENV` that run commands, and a CLAUDE.md that imports a file outside. A `.git` whose config names a command is not planted: it makes the CLI show the folder-trust dialog at the next start, and a supervisor's resume with one was blocked at start (#423). The supervisor's id is no person of the channel, so it may read the channel's threads but cannot ask the worker to do what it may not.
4. **Asleep, then resumed.** With a 20-second idle period the supervisor is put to sleep; the attempts of step 3 then run again in its resumed session, still its only session. Neither an answer nor the transcript holds the word in the file its CLAUDE.md imports (the word is only in that file), and after 75 seconds, longer than the status line's refresh, none of the planted commands has run. Whether hataori's own status line handed on usage meanwhile is logged, to show the status line ran at all.
5. **A message that starts with `!`**, the prefix of the CLI's own shell mode, posted in the supervisor's thread: after a minute the command's file is not there. It runs last, since the CLI may be left in another mode.
6. **The user's settings file unchanged**, as step 11 of the phase 1 check.

The supervisor's launches run through a wrapper that passes a confined launch (one with `--restricted`) to the CLI exactly as the adapter made it, so that the check is of the launch as deployed, including what the adapter adds to keep the session's own files out. The user's own settings file is not loaded for it, as for a supervisor anywhere: `--restricted` ignores it, whatever `--setting-sources` names (Claude Code 2.1.328), so the user's `env`, hooks and plugins do not reach the session; its CLAUDE.md files, memory and connectors are kept out by the launch itself. The worker runs through the phase 1 wrapper. `TestRealCheckLeavesTheLoginAlone` holds this check's source to the same rules as the phase 1 check's.

Last passed: 2026-11-26, Claude Code 2.1.332, logged in with a subscription, model `haiku`, with the launch that leaves the session no built-in tool that writes (#868). All 6 steps passed in 4.4 minutes, and hataori's own status line handed on usage. Before that: 2026-11-19, Claude Code 2.1.328, in 3.9 minutes; earlier runs that day found what #409, #410 and #426 fixed, and the blocked resume with a `.git` that #423 tracks.

### The reopen check

```sh
HATAORI_REAL_CLAUDE=1 go test -count=1 -run TestRealClaudeCodeReopen -v -timeout 20m ./cmd/hataori
```

It runs the daemon as the phase 1 check does, through the same wrapper, and checks that a reopened thread resumes the recorded conversation rather than starting a new one (#670, design section 5.1):

1. **A word to remember.** An order tells the agent a word.
2. **Closed, reopened on the host, the conversation resumed.** `hataori close`, then `hataori reopen` answers `kept` and `resume`; asked for the word, the agent answers it, in the same agent session.
3. **Removed, reopened from trash, the conversation resumed.** `hataori close` and `hataori remove` move the workspace into trash; `hataori reopen` answers `restored` and `resume`, the workspace is back at its place, and the agent still answers the word in the same agent session.
4. **Removed and purged, reopened in a new clone, a new conversation.** The thread is removed again and its entry in trash is deleted, as the trash pass does. `hataori reopen` answers `remade` and `new`; the agent runs in another agent session and answers the word from the earlier exchange its first prompt gives it.
5. **Nothing left running** and 6. **the user's settings file unchanged**, as steps 10 and 11 of the phase 1 check.

Last passed: 2026-11-24, Claude Code 2.1.330, model `haiku`. All 6 steps passed in 59 s.

### A wake's usage

```sh
HATAORI_REAL_CLAUDE=1 go test -count=1 -run TestRealClaudeCodeWakeUsage -v -timeout 60m ./cmd/hataori
```

It runs the daemon as the supervisor check does, with the supervisor's hourly turn limit off, and measures what each digest wake uses (#676): a worker thread's turn ends 46 times in a row (as many turns as the day #676 measured had), each waking the supervisor, and a person talks to the supervisor now and then. Each wake's usage is the input, output, cache reads and cache writes that the CLI reports for its turn, summed; every wake must have one, and none may be more than a fifth above the first wake's (the least is logged too: a wake far below it is one whose first request the CLI made before hataori's MCP server was connected), since a wake runs in a conversation of its own. It is a day's number of wakes in a row, not a day: what grows with time alone is not in it. It logs every turn's sum. The rules above hold; the settings file is compared at the end.

### The role check

```sh
HATAORI_REAL_CLAUDE=1 go test -count=1 -run TestRealClaudeCodeManager -v -timeout 40m ./cmd/hataori
```

It runs the daemon as the supervisor check does, with a supervisor on `sonnet` at effort `high` under the autonomy `act`, whose channel's `USER.md` asks for Japanese, and checks what hataori's layer of its instructions brings about (#653). Each step is judged by what happened, logs what the supervisor answered, and does not stop the others: 1. the ready line is one line in Japanese; 2. asked about its work, it reads `notes/`, also a note written since it last read them, and `docs/`; 3. a wake that needs nobody ends with exactly `(quiet)`; 4. asked for lighter reports, it edits `USER.md` or `SOUL.md` and names it; 5. asked to switch the autonomy, it says that this is the operator's key `autonomy`, and changes no file; 6. asked what its tools do not show, it starts a thread, and the first line of its report answers; 7. woken by a turn end with a reminder open that asks for the same look, it cancels or replaces that reminder in that wake; 8. after three wakes in a row about two threads, its notes hold one status of each; 9. the user's settings file is unchanged. A model's answers vary: a run that fails a step another run passed is no proof either way, so look at several.

Runs on 2026-11-24, model `claude-sonnet-5-5`: four, each on a text changed after the one before, the first three with Claude Code 2.1.330 and the last, on the text as merged, with 2.1.331. Only the last passed every step, in 85 s. Steps 1, 4, 5 and 7 (now 9) passed in all four. Step 2 failed in the first two, where the supervisor answered from what it remembered, and passed in both since the text has it read the notes again. Step 3 failed in the third, whose text differs from the last in three clauses of fact: the wake said that nothing was to be done, instead of `(quiet)`. Step 6 passed in the last three; in the first its check looked too early. So a wake that needs nobody ends with `(quiet)` most times, not surely each time.

Runs of steps 7 and 8 on 2026-11-25 (#797), model `claude-sonnet-5-5`, Claude Code 2.1.331: twelve with the layer's text on the two, of which the last three are on the text as merged, and three with the layer as it was. The last three passed both steps; the old layer failed both, in all three. In the nine before, on texts and step 8 changed in between, step 7 failed once and step 8 twice, once of those for a fault of the step. One run of the whole check on the text as merged passed all nine steps, in 593 s. Pull request #816 has them one by one.

### New captures

`internal/agent/claudecode/testdata/capture.py` records hook calls, transcripts and terminal output from the real CLI, and `scrub.py` makes them committable. Their README says how each capture was taken. Run `go test ./internal/agent/claudecode` before committing: it fails on a capture that still holds a home path, an e-mail address, an id or a token.

```sh
cd internal/agent/claudecode/testdata
python3 capture.py /tmp/raw login login_terminal    # or no names, for every scenario
python3 scrub.py /tmp/raw <version>-new
```
