# First Linux worker

One main keeps the chat clients, people, control API and SQLite database. A
worker keeps its resident, detached session hosts, agents, workspaces, mirrors
and trash. Each thread stays on the machine first chosen for it. Existing
single-machine roots need no worker configuration and keep their directories.

Another machine adds CPU and memory, **not provider quota**. Logins and resource
observations are machine-local; reports from two machines are not added into an
account allowance.

## Prerequisites

- Install the same compatible hataori build manually on the main and Linux
  worker. Worker protocol versions are independent of the public control
  protocol. An incompatible major refuses before work; SSH reachability alone
  is not readiness. Automated installation/upgrades are outside this guide.
- Install the chosen agent CLI and Git on the worker. Sign into the agent
  directly there as the worker's OS user, using its existing login. A worker
  with a missing CLI, unavailable login or unknown resource measurements is
  ineligible. Remote login relay is not required or provided by this path.
- The main must reach that user through an existing system OpenSSH alias with
  verified host keys and noninteractive authentication. Configure ports and
  identity selection in the operator's SSH configuration. hataori neither
  creates keys nor accepts unknown host keys, forwards an authentication agent,
  or sends main bot/forge/supervisor credentials to the worker.
- Start the resident in the worker user's own session. SSH connects to that
  already-running resident; it does not install a service or start an agent in
  an SSH command. See [resident lifecycle](worker-resident.md) for explicit
  user-service templates that preserve detached hosts.

The following names and paths are synthetic. Use a new private worker root;
never initialize it by copying a main root, database, configuration or login.

## Register the two installations

On the main, obtain the identity of an already initialized instance:

```sh
hataori worker main-id --root "$MAIN_ROOT"
```

On the worker, with an existing parent directory, run:

```sh
hataori worker init --root "$WORKER_ROOT" --main MAIN_INSTALLATION_ID
```

Keep the returned `main` and `worker` IDs. A repeated initialization must name
the same main and original root; a replacement root or another main refuses.
There is no automatic adoption or command to discard an existing binding.

Write the worker's own `config/worker.json` with mode `0600` inside its private
root. This example uses the terminal-capable Claude Code adapter and its
existing local login; it invokes no login or upgrade automation:

```json
{
  "agents": [
    {"agent": "claude-code", "configuration": "default", "settings": {}}
  ],
  "workspaces": {
    "repositories": {
      "project": "https://forge.example.invalid/team/project.git"
    }
  }
}
```

For Codex, use `"agent": "codex"` and its existing adapter settings instead.
The optional `config_dir` selects an existing worker-local login directory; it
is not sent by the main. Register repository addresses on the worker, with
worker-local noninteractive Git credentials. Repository IDs such as `project`
are opaque handles. The main can select a registered ID or its exact configured
address; an unregistered repository refuses. Nothing clones on the main as a
fallback.

Start the resident manually in the worker user's session:

```sh
hataori worker run --root "$WORKER_ROOT" --main MAIN_INSTALLATION_ID --installation WORKER_INSTALLATION_ID
```

The ready line reports the worker protocol. Keep this process running, or
install a user service explicitly using the lifecycle documentation. Stopping
only the resident leaves detached hosts alive; reboot/logout of the entire
user session is a different event. No daemon connection manages services.

## Configure placement on the main

Add a worker entry to the main's existing configuration, preserving its people,
chat drivers and other settings:

```json
"machines": [
  {"key": "local"},
  {
    "key": "build",
    "ssh": "worker-alias",
    "root": "/srv/example-worker",
    "installation": "WORKER_INSTALLATION_ID",
    "agents": ["claude-code"],
    "max_resident": 4,
    "max_starting": 1
  }
]
```

`root` is the worker's canonical absolute root; the main does not open it as a
local path. Configure the same agent under the main's `adapters`. To restrict
an ordinary-work scope to this worker, add `"machines": ["build"]` to that
scope. Omitted permits all configured machines; `[]` permits none. Supervisors
and their confined conversations remain local. At the next main startup,
binding/version, health and inventory reconciliation must succeed before
placement. See [configuration](configuration.md#machines) for shipped limits.

An explicit `/new` machine choice of `build` refuses with its reason if the
worker is unavailable, outside the scope or full. Leave the machine choice out
for automatic selection among eligible machines. When no eligible machine has
room, the accepted automatic start waits durably; it does not create another
directory or silently select an excluded machine. Resident slots include idle
and waiting agents. The public API's `Start.machine` has the same explicit
choice and refusal semantics; see [Start](protocol.md#operation-start).

Use the configured repository address, a short prompt and the scope's normal
chat trigger or `/new`. Send a follow-up, ask the agent to pose a question and
answer that question through the same chat thread. Status reports the chosen
machine. No thread migration is performed by changing configuration.

## Status and owner attachment

On the main:

```sh
hataori threads --root "$MAIN_ROOT"
hataori thread --root "$MAIN_ROOT" THREAD_NUMBER
hataori events --root "$MAIN_ROOT" --follow
hataori attach --root "$MAIN_ROOT" THREAD_NUMBER
```

Attachment is owner-only and requires a terminal-capable agent; a pipes-only
agent such as Codex refuses terminal attachment. The CLI asks the main for the
registered worker/run and opens its own checked SSH connection. Ctrl-\ detaches
and leaves the agent running. A connection loss ends attachment visibly. Open
a new attachment afterward; raw keys are never replayed. No arbitrary worker
terminal discovery or take-in is provided by this first-worker path.

When disconnected, chat/API status shows availability, first failure time and
last known session state/time, rather than claiming that an away turn is
currently running. `machine_away_grace` defaults to `30s`; transport loss blocks
new placement and dispatch immediately, including during the grace.

## Recovery and workspace protection

A main restart reconnects to the same worker hosts. Recovery drains retained
output, reconciles original operation receipts and current hooks, then checks
current person/scope authority before delivering held work. Input, a named stop
and a request-specific answer wait on the main. A pending stop has stopped
nothing. A stop for an old turn and an answer to an expired/withdrawn question
refuse visibly. A proven transient no-write failure permits a checked fresh
attempt; an uncertain or missing receipt never authorizes blind replay.

Unknown inventory keeps reservations and stays unavailable. Only verified host
loss after a reboot uses interrupted recovery, on the recorded machine and
directory. Restore the original destination/root/installation binding when
recovering an unavailable worker. Do not delete journals, reinitialize another
root under the same key or replace a running binary to force compatibility.
Retained replay history is bounded; exhaustion pauses rather than discarding
the bytes needed for reconstruction.

Close, reopen, removal and attachment refuse while the owning machine is
unchecked. Removal/restore/purge operate on the worker's registered handles and
current local proof. The minimal example above intentionally supplies no
independent-proof catalog and therefore refuses removal. Before enabling that
path, declare complete physical-machine/root and independent-destination
information as documented in [worker workspaces](worker-workspaces.md).
Purge is disabled unless positive retention is configured; acknowledged trash
and crash reconciliation are required. Another machine's directory is not an
independent backup for deletion proof.

## Evidence and current limits

The first-worker gate uses isolated main/worker roots, a fake chat surface,
fake agent, generated fixture SSH keys and the test's own SSH server. Linux and
macOS CI are recorded separately in #965. Those checks do not establish an
actual Linux provider login or real-agent run, and do not establish macOS
worker runtime support. Actual macOS login-session/Keychain/LaunchAgent,
case-insensitive filesystem, native resource, sleep/wake/reboot and teardown
evidence remains required by #969. Cross-builds and simulated connection cuts
are not that evidence. Remote login, automated upgrades and machine dashboard
features remain follow-up work.
