# Codex captures

Real output of `codex app-server`, for the Codex adapter. These files are not a contract: when the CLI changes, add a capture of the new version next to this one.

- `0.163.0/`: codex-cli 0.163.0 on Linux (also in `0.163.0/VERSION`). Two sets:
  - the confinement scenarios (`locked_*`, `read_only_*`, `workspace_write_*`), on 2026-11-18 with the model `gpt-5.6-luna`, taken by `confine.py` for #268;
  - the adapter scenarios (every other directory) and `methods.json`, on 2026-11-19 with the model `gpt-6-luna` and effort `low`, taken by `capture.py` for #317.
- `confine.py` took the first set and made it committable (`confine.py --scrub RAW_DIR OUT_DIR`, with the rules of `../../claudecode/testdata/scrub.py`: paths, UUIDs and thread ids, e-mail addresses, tokens; the model's response item ids, `msg_` and 40 hexadecimal digits, become `msg_NNNNNN`, numbered in order of appearance).
- `capture.py` took the second set (below).
- `../captures_test.go` fails when a capture holds a home directory path, an e-mail address, an unscrubbed UUID, a long hexadecimal string or something shaped like a token.

## The adapter's scenarios (0.163.0, taken for #317)

```sh
python3 capture.py RAW_DIR            # every scenario; or name some
python3 capture.py --scrub RAW_DIR OUT_DIR
```

Each scenario starts `codex app-server` on stdio in a process group of its own, with a minimal environment, the Codex the operator installed and its own login. Its `cwd` is a new temporary directory that is a Git repository with one commit, as a workspace is a clone. `thread/start` sends `cwd`, the model, `config.model_reasoning_effort`, `developerInstructions`, `sandbox: "workspace-write"` and `approvalPolicy: "never"` (`"untrusted"` in the approval scenarios); every `turn/start` sends `effort`.

**The user's configuration is never written.** In 0.163.0, `thread/start` with a `cwd` and a sandbox that can write there records the `cwd`'s Git root in the user's `config.toml` as a trusted project when no trust level is set for it (this is also what `codex exec` does in that version). The script therefore gives every app-server `-c projects.<cwd>.trust_level="trusted"`, which sets a trust level without writing the file. It reads the checksum of the user's `~/.codex/config.toml` before the run and after every scenario, and stops with exit status 2 when it changed. Three full runs left it unchanged. Server requests are answered by the scenario (approvals `accept`, or `decline` in `approval_decline`). Processes are ended by our own process group, and the processes that the app-server started outside it (found by parent or by working directory, checked by start time) by their pid; none was left after any run. Session files under the user's `~/.codex/sessions` are written by the app-server, as for every thread that is not ephemeral; `resume` needs them.

Each scenario directory holds:

| File | What it is |
|---|---|
| `wire.jsonl` | Every line in both directions, in order: `{"t": <seconds since the app-server started>, "dir": "send" \| "recv", "msg": {...}}`, with `"server": N` when the scenario starts more than one app-server. Nothing is left out: deltas, usage, rate limits, reasoning. |
| `notes.json` | What is not on the wire: process lists (argv shortened to the program and the command it runs; no real pids), exit codes, timings, errors, or `not_run` with the reason. |

`methods.json` is the method list of 0.163.0, from `codex app-server generate-json-schema --experimental`: client requests and notifications, server requests and notifications.

| Scenario | What it shows |
|---|---|
| `start` | `initialize`, `thread/start`, one turn. In this capture the model answered with one `agentMessage`, `phase: "final_answer"` ("I'll run the requested shell command, then answer the arithmetic question."), and ran nothing; `item/agentMessage/delta`. `usage` has the same prompt and shows the full shape: `phase: "commentary"`, a command, then `phase: "final_answer"`. |
| `command`, `file_change` | A `commandExecution` item with `item/commandExecution/outputDelta`; a `fileChange` item with `turn/diff/updated`. |
| `approval_accept`, `approval_decline` | Under `untrusted`: `item/commandExecution/requestApproval` and `item/fileChange/requestApproval`, each followed by `serverRequest/resolved`. Accepted, both files were written; declined, the items completed with `status: "declined"` and neither file exists. |
| `question_async` | `request_user_input_async`: an `agentMessage` (`phase: "final_answer"`) with `questions`, while the turn goes on; the `turn/steer` answer was taken into the same turn, which then answered "Blue". |
| `question_after_turn` | The same question, not answered. The model used its `sleep` tool, then ended the turn on its own after 34 s ("I haven't received your answer yet"); the answer sent with `turn/start` was used. In an earlier run, not kept, it was still sleeping after 120 s and the turn was interrupted: whether the turn ends is up to the model. |
| `question_plan` | `collaborationMode/list`, a turn with `collaborationMode: {"mode": "plan", ...}`: the server request `item/tool/requestUserInput` with the thread's status flag `waitingOnUserInput`, answered with `{"answers": {<id>: {"answers": [<label>]}}}`. |
| `steer` | `turn/steer` while `sleep 20` ran: answered with the turn's id, a `userMessage` item at once, and the final answer followed the steered text. |
| `interrupt` | `turn/interrupt` while `sleep 600` ran: `turn/completed` with `status: "interrupted"` at once. 15 s later `sleep 600` and its sandbox wrapper were still running, outside our process group. After `thread/backgroundTerminals/clean` they were gone, and the command's item completed with `status: "failed"`, `exitCode: -1`. The next `turn/start` worked. |
| `resume`, `resume_unknown` | A new app-server, `thread/resume` of `start`'s thread: the model answered from the old conversation. For a thread id that does not exist: the error `-32600`, `no rollout found for thread id <id>`. |
| `subagent` | `spawn_agent`: `collabAgentToolCall` items; the sub-agent's own `turn/completed` (another `rootTurnId`) arrives on the same connection before the parent's. |
| `usage` | `thread/tokenUsage/updated` and `account/rateLimits/updated` during the turn; `account/rateLimits/read` answered during it. |
| `background` | `(sleep 40; echo bg-done) &`: the command item completed at once and the turn ended. Nothing it started was running 0 s, 45 s or 60 s after the turn, and no message and no turn followed in 60 s. |
| `exit` | Input closed between turns: the app-server wrote nothing more and exited with status 0 within a second. A second app-server killed (SIGKILL to its group) during `sleep 600`; a third one resumed that thread: `thread/resume` succeeded, the thread `idle`, the killed turn `interrupted` with only its `userMessage` item. |
| `login_device` | The device login link and code, stopped before anyone signed in; the end is from Codex source. The code was replaced by its shape by hand after this run; see `notes.json`. |
| `login_status` | `codex login status`: exit status 0, `Logged in using ChatGPT` on standard error, nothing on standard output. `account/read` gives the account type. |
| `trust` | Not run (below). |

**What the captures answer** (the design's Appendix B, Codex):

- `thread/backgroundTerminals/clean` stops the command that `turn/interrupt` left running (`interrupt`).
- `thread/resume` of an unknown thread is a JSON-RPC error, code `-32600`, message `no rollout found for thread id <id>` (`resume_unknown`).
- No turn started that nobody asked for (`background`, 60 s after the turn; the command's background child did not outlive the command).
- Whether the app-server records a fresh workspace as trusted was not taken from a run: `trust` needs a `CODEX_HOME` of its own. The source of 0.163.0 (`thread_processor.rs`, the `set_project_trust_level` call in `thread/start`) does so for a writable sandbox when the workspace has no trust level; see above for how the script prevents it.

**Not run:**

- `trust`: it needs `CODEX_HOME` set to a directory of its own with a login made for it. No such login was made, and the user's credentials are never copied.
- A usage limit and an expired login: not seen.

**Notes for the codec:**

- `request_user_input_async` is offered only for models whose catalog entry lists it in `experimental_supported_tools`. With `gpt-5.6-luna` the model answered that the tool was unavailable; `gpt-6-luna` has it. A question item has `questions` set; every `agentMessage` carries the key, `null` otherwise.
- A long command is moved to a background terminal: with "Run `sleep 600`" alone, the model ended the turn while the command went on running. The prompt has to ask it to wait.
- Commands run in a process group of their own, not the app-server's. They ended with the app-server, also when it was killed in `exit` (the sandbox wrapper is started with `bwrap --die-with-parent`, seen in the full argv before it was shortened).


## A confined session (0.163.0, taken for #268)

```sh
python3 confine.py RAW_DIR            # every scenario; or name some
python3 confine.py --scrub RAW_DIR 0.163.0
```

Each scenario starts `codex app-server` (stdio) with a minimal environment and one MCP server, given only with `-c`:

```
codex app-server -c mcp_servers.sv.command="<python>" -c mcp_servers.sv.args=["<server>"] \
  -c mcp_servers.sv.default_tools_approval_mode="approve" [variant flags]
```

`sv` is a stdio server written by the script, with one tool, `ping`, that only answers. Then it sends `thread/start` with a new empty temporary directory as `cwd`, `approvalPolicy: "never"`, `ephemeral: true` and the variant's `sandbox`. After that it sends one `turn/start` that asks for one thing. "Outside" is a second new temporary directory. The prompt asks for exactly one real attempt even when the model expects it to fail: without that, the model often declined from what its instructions say about the sandbox, which shows nothing about the sandbox. Server requests (approvals) are declined; none arrived under `never`.

The user's `~/.codex` is read, as every Codex start reads it. The script never writes to it. Codex itself does in one scenario (see `project_config` below). Both temporary directories become `/tmp/capture-work` when scrubbed.

| Variant | `sandbox` | Extra flags |
|---|---|---|
| `read_only_*` | `read-only` | none |
| `workspace_write_*` | `workspace-write` | none |
| `workspace_write_no_tmp_*` | `workspace-write` | `-c sandbox_workspace_write.exclude_slash_tmp=true -c sandbox_workspace_write.exclude_tmpdir_env_var=true` |
| `workspace_write_no_tools_*` | `workspace-write` | the above, plus `--disable shell_tool --disable unified_exec --disable multi_agent --disable goals --disable sleep_tool --disable apps --disable plugins --disable hooks` (only `subagent` and `webfetch`) |
| `locked_*` | `workspace-write` | the above, plus `-c web_search="disabled" -c agents.max_depth=0` |

Each scenario directory holds:

| File | What it is |
|---|---|
| `messages.jsonl` | The server's messages for the thread: `thread/started`, `turn/started`, `turn/completed`, and every `item/started` and `item/completed` except reasoning. Deltas, usage and rate limits are left out. |
| `status.json` | `mcpServerStatus/list` before and after `thread/start` (servers this script did not make are counted, not named) and the features whose names concern agents, shells, the web, plugins and hooks, from `experimentalFeature/list`. |
| `outcome.json` | The completed items in short, the thread's reported sandbox, and whether the file the attempt was asked to write exists. |

What they show (each row of the design's Appendix B comes from these):

- **The shell runs in both sandboxes.** `read-only` and `workspace-write` confine what a command can write and its network, not whether it runs. Reads are not confined: `cat` of a file in the outside directory succeeded in every variant that has a shell.
- **`workspace-write` lets a command write to `/tmp` and `$TMPDIR`** unless `exclude_slash_tmp` and `exclude_tmpdir_env_var` are set: `workspace_write_write_outside` wrote into the other temporary directory. With both set, the write was refused (`patch rejected: writing outside of the project; rejected by user approval settings`; from the shell, `read-only file system`).
- **The network is off in both sandboxes:** `curl` failed with `Could not resolve host`. The web search tool is not affected by this. With the shell and the other features of `workspace_write_no_tools` off, it answered (`workspace_write_no_tools_webfetch`, a `webSearch` item), until `web_search="disabled"`.
- **`--disable multi_agent` leaves `spawn_agent`.** The tool was there in every variant but `locked`: `workspace_write_subagent` and `workspace_write_no_tmp_subagent` show a completed `spawnAgent`. In `read_only_subagent` and `workspace_write_no_tools_subagent`, the model's call failed on its own arguments (`Provide one of: message or items`, `missing field target`); in an earlier run with other wording, not kept, `spawnAgent` completed in both. `agents.max_depth=0` removed it. (`agents.max_threads=0` made the app-server exit at start.)
- **Background processes:** with a shell, `sleep 600 &` started and the turn ended. No such process was left after the app-server exited. No tool to wake the thread later was used.
- **MCP tools need an approval.** Under `approvalPolicy: "never"`, a call is refused (`MCP tool call requires approval, but approval policy is never`, seen for the project's server in `workspace_write_no_tmp_project_config`) unless the server's `default_tools_approval_mode` is `"approve"`, as it is for `sv`.
- **`unified_exec` stays on** in `experimentalFeature/list` after `--disable unified_exec`. With `shell_tool` off, no command ran all the same.
- **The user's configuration:** before `--disable apps`, one server that this script did not make, the account's connectors, was loaded from it. With `apps` and `plugins` off, only `sv` was.
- **`project_config`:** the working directory held `.codex/config.toml` with `sandbox_mode = "danger-full-access"`, `approval_policy = "never"` and an MCP server `project`. The thread kept the `sandbox` it was started with: the write outside was refused. The project's MCP server was loaded: the model called it, and the call was refused for want of an approval. In this run a parent of the directory was trusted in the user's configuration. **`codex app-server` wrote the directory into the user's `~/.codex/config.toml` as a trusted project** (`[projects."<dir>"]`, `trust_level = "trusted"`), one entry per run of this scenario: four in all. The Claude Code session that ran the check removed them by hand, before the maintainer asked for the file to be left alone, and checked the file against its checksum from before the runs. `confine.py` now runs these scenarios only with `CODEX_HOME` set to a directory other than the user's own, and otherwise records them as not run. It never writes the user's configuration.
- **Session state:** even with `ephemeral: true`, the app-server wrote rollout files under the user's `~/.codex/sessions` for some of these threads (22 of a full run name the capture directories), and refreshed its caches there. They are not configuration, and the script does not remove them.

## Device login capture

`login_capture.py` captures the device login prompt into a new directory, scrubbing the link, one-time code and identifying values before writing. It stops as soon as the link and code appear; nobody signs in.

```sh
CODEX_HOME=$(mktemp -d) python3 login_capture.py 0.163.0/login_device
```

It refuses an unset or empty `CODEX_HOME`, the user's own directory (including a symlink or a directory holding the user's config), a directory that does not exist, a `CODEX_HOME` holding `auth.json`, or an existing output directory. It checks that the user's configuration and authentication files remain unchanged. The capture's description of the login's end comes from the Codex source, because the script stops before that point.
