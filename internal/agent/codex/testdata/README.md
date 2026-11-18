# Codex captures

Real output of `codex app-server`, for the Codex adapter. These files are not a contract: when the CLI changes, add a capture of the new version next to this one.

- `0.163.0/`: codex-cli 0.163.0, on 2026-11-18 (also in `0.163.0/VERSION`), on Linux, with the model `gpt-5.6-luna`, taken by `confine.py` for #268.
- `confine.py` took them and made them committable (`confine.py --scrub RAW_DIR OUT_DIR`, with the rules of `../../claudecode/testdata/scrub.py`: paths, UUIDs and thread ids, e-mail addresses, tokens; the model's response item ids, `msg_` and 40 hexadecimal digits, become `msg_NNNNNN`, numbered in order of appearance).

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
- **`project_config`:** the working directory held `.codex/config.toml` with `sandbox_mode = "danger-full-access"`, `approval_policy = "never"` and an MCP server `project`. The thread kept the `sandbox` it was started with: the write outside was refused. The project's MCP server was loaded: the model called it, and the call was refused for want of an approval. In this run a parent of the directory was trusted in the user's configuration. **`codex app-server` wrote the directory into the user's `~/.codex/config.toml` as a trusted project** (`[projects."<dir>"]`, `trust_level = "trusted"`), one entry per run of this scenario: four in all. They were removed by hand, and the file was checked against its checksum from before the runs. `confine.py` now runs these scenarios only with `CODEX_HOME` set to a directory other than the user's own, and otherwise records them as not run. It never writes the user's configuration.
- **Session state:** even with `ephemeral: true`, the app-server wrote rollout files under the user's `~/.codex/sessions` for some of these threads (22 of a full run name the capture directories), and refreshed its caches there. They are not configuration, and the script does not remove them.
