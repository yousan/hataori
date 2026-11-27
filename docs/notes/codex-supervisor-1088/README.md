# Confined Codex supervisor (#1088)

Current proof: Linux, codex-cli 0.163.0, model `gpt-6-luna`, effort `low`.
Other CLI versions, models and operating systems are refused. This is explicit
selection, not provider failover. Ordinary workers keep their own adapter,
model and settings.

## The boundary

#268 correctly found that sandbox permissions alone do not remove native
tools. Current verification found a separate mechanism: `environments: []`
on `thread/start` and **every** `turn/start`, including after `thread/resume`,
with `stable_environment_tools=false`. The current CLI's
[environment-tool registration](https://github.com/openai/codex/blob/rust-v0.163.0/codex-rs/core/src/tools/spec_plan.rs)
gates native patch and shell tools separately from supplied MCP tools.

The current tool inventory contained only the supplied fixture MCP tool,
MCP resource accessors for the supplied servers, and the read-only clock.
Native `apply_patch` and `exec_command` calls threw `TypeError`. In code mode,
`fetch`, `process`, `require` and `WebSocket` were undefined; its `exec` is
JavaScript orchestration over that inventory, not an OS shell. No shell,
file tool, subagent, web/browser/network fetch or background/wake tool was
available. Read-only sandbox and approval policy `never` remain defense in
depth; refusing a write is insufficient to attest `NoWriteTools`.

The adapter reads complete composed instructions from a generated session-state
file. Missing or unrecognized instructions refuse codec startup. The current
CLI restores its old static developer-instruction baseline when resuming; even
an explicit thread/resume override does not re-render it. Default collaboration
mode can also ignore custom instructions in favor of the model catalog. The
adapter therefore uses `thread/inject_items` to append the current trusted
composition as a developer message after start/resume, and waits for its
acknowledgment before reporting readiness or starting a turn. Failure refuses
startup. This snapshot explicitly replaces earlier supervisor snapshots, keeping
the documented layer order. Static CLI instructions contain only the adapter's
literal-input explanation. Native tools remain independently disabled.
Automatic daemon startup is also disabled; stdio app-server needs no proxy.

Text input on pipes is not a terminal command. Every input and steer replaces
ASCII `@` with `(at)` and `$` with `(dollar)`, with empty text elements.
Codex expands `$skill` and `[$skill](path)` even from text with empty elements
([current mention parser](https://github.com/openai/codex/blob/rust-v0.163.0/codex-rs/skills/src/mentions.rs)).
The adapter explains the replacements to the model and correlates delivery
against the transformed text. The model may restore these characters in its
own output or arguments to restricted MCP tools.

Linux bubblewrap provides a separate mutable CLI home in session state.
Only runtime paths, the scope, supplied MCP executables/absolute arguments,
and session state are mounted; the existing CLI authentication file is
mounted read-only. User home/config, system Codex config, ambient API keys,
proxy variables, hooks and plugins are not inherited. Scope configuration is
explicitly untrusted; AGENTS discovery is disabled. The scope mount itself is
read-only: restricted file writes happen through the daemon's supplied MCP
API, outside the namespace. The runner's four attestation checks stay intact.

Authentication bytes are never read or copied by hataori. A symlink is unsafe:
CLI refresh opens its auth file with truncation. Read-only mounting means
local refresh persistence can fail. The CLI can still attempt provider-side OAuth
renewal before persistence, and may continue with cached authentication after a
refresh error; the read-only mount does not prevent that network behavior. The
launch never retries with API credentials or initiates login. Operators handle
authentication failures separately; these fixtures do not test expired credentials.
Missing bubblewrap, unavailable file authentication, symlinked state or an
unverified CLI/model fails closed. Generated MCP configuration contains its
launch token in mode-0600 session state, like the existing adapter's MCP config;
the token does not belong in stored specs, captures or the conversation.

## Evidence and limits

- `internal/agent/codex/testdata/0.163.0/supervisor_gate/`: sanitized real
  stdio messages and filesystem observations from disposable fixtures,
  captured on 2026-11-27 by `supervisor_gate.py`. Allowed MCP note writes,
  unavailable native tools, direct escape attempts, hostile project settings,
  literal command/file/skill text and a fresh-process resume all passed.
  A synthetic MCP credential reached only its supplied server and was absent
  from its rollout files. Virtual home and rollout filenames are normalized.
- `TestRealCodexSupervisor`: opt-in, actual adapter and daemon with a fake
  chat service and fake workers. Initial launch, allowed management/file
  operations, daemon reconnection and process sleep/resume have passed.
  The full worker/conversation/reminder proof is recorded when complete.
- Capture-based regression tests check the actual CLI's returned literal
  input and its delivery identity; adapter refusal and settings isolation,
  selected-agent own/conversation/wake launches and operator switching have
  focused tests. Full tests run in CI, including terminal tests; no local
  tmux test or other provider is needed for this work.

The fixtures do not constitute a general-purpose OS sandbox proof, a proof
for other models/versions, or a production deployment. No production state,
worker or configuration was changed, and no login command was initiated. Required reviews are a fresh
independent Codex code review and a separate confinement-focused review.
Final-head CI must pass before merge; main CI must pass after merge.

## Non-destructive operator switch and rollback

The operator owns this procedure. A config change alone does not migrate an
existing thread to another provider. No transcript is translated or replayed
as native conversation, and no automatic provider or paid-API fallback runs.

1. Record the scope's current supervisor and conversation thread numbers,
   worker numbers/owners, open questions and reminders. Read the current
   notes and outstanding stage instructions. Stop issuing new supervisor
   conversation requests while switching. Keep ordinary workers running.
2. Close the old supervisor's own thread, then its open conversation threads,
   using `hataori close <thread> --root <root>`. Close refuses a running turn,
   open question or undelivered input. Wait for a safe idle/failed state, or
   explicitly resolve/stop that coordinator turn as the operator; do not stop
   or close its workers. Closing retains the chat/history, scope files and
   session records and revokes the closed session's management token.
3. Confirm that all old coordinator threads are closed. Change only that
   scope's `supervisor.agent`, `model` and `effort` in the operator-managed
   configuration. For the verified Codex path use `codex`, `gpt-6-luna` and
   the desired supported effort. Keep the scope identity, repos, autonomy,
   budgets, directory and reminder configuration. Do not change ordinary
   worker adapter defaults or remove old adapter configuration/history.
4. The operator restarts/reloads the daemon through their normal procedure.
   Its existing idempotent Start key creates one replacement supervisor
   thread and fresh native conversation. The shared scope directory and
   notes remain; existing workers keep their sessions/ownership, and open
   reminders remain scope-scoped. A restart during Start cannot duplicate
   that replacement. Do not delete sessions, notes, stashes or workspaces.
5. Verify the selected adapter/model and one safe `list_threads` call. Have
   it read notes and reconcile the existing worker state/open questions and
   reminders **before authorizing further stages**. Confirm that the two
   inventories match; do not restate old stage instructions as new orders.
   Historical conversation context is available for an operator to consult,
   but the new provider starts with a fresh conversation and shared notes.

Rollback repeats the same close/reconcile procedure with the previous
adapter/model settings. It creates another fresh coordinator conversation;
it does not reopen an old coordinator while a replacement still runs. Notes,
worker ownership, outstanding reminders and all closed histories are retained.
No production switch or restart is performed by the worker implementing this
issue.
