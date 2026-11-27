# Worker resident

The resident, SSH transport, workspace registration, remote runner and durable
placement through #963 are foundations of #956's first-worker gate. Away
actions (#964) and the combined product gate (#965) follow. It never
loads the main's configuration, starts a bot or accepts a control-socket hello.
No deployment or service-manager operation is part of the implementation.

## Explicit installation

Install the same built hataori executable on the worker. Choose a new private
root whose parent already exists. Run these commands manually as the worker's
OS user, replacing the example identities with the operator's out-of-band
registration. Never copy a main root or credentials to initialize a worker.

```sh
hataori worker init --root "$WORKER_ROOT" --main MAIN_INSTALLATION_ID
```

The JSON result contains `main` and a randomly created `worker` installation
ID. Record that binding on the main. An existing registration repeats only for
the same main and original root inode; a second main, copied registration or
replacement root refuses. An unregistered nonempty directory is never adopted,
removed or repaired. Root relocation while a resident is open is unsupported:
pathname identity must still match its pinned root before operations.
The same OS user can change its files/processes; this layout is not a sandbox
against that user or an attacker controlling the account.

Write `config/worker.json` inside that root, mode `0600`. The root and its
configuration/state parents must be owned by the worker user, mode `0700`.
For example, one locally installed adapter can be configured as:

```json
{
  "agents": [
    {
      "agent": "codex",
      "configuration": "default",
      "settings": {},
      "no_project_settings": false
    }
  ]
}
```

`agent` is an adapter compiled into this executable; `configuration` is an
opaque local name. `settings` uses that adapter's existing configuration schema.
Optional `config_dir` is an absolute, worker-local login configuration directory;
it is never accepted from a protocol request. It selects `CODEX_HOME` for Codex
and the adapter's login directory for Claude Code. `no_project_settings` demands
the adapter's policy isolation capability; unsupported adapters refuse launch.
At most 32 configurations are loaded. The optional `workspaces` object configures
[worker-local workspaces and files](worker-workspaces.md). Other unknown
top-level configuration fields refuse; no main instance configuration is consulted. An empty agent list is
valid for a health-only resident. Changes require a resident restart.

Worker adapters must implement `agent.WorkerAdapters`: they discard ambient
`pass_env` additions for both probe and launch and retain their fixed user/login
environment. SSH and hataori control environment variables are additionally
removed from launches, except the worker-generated host hook socket. Agent login
credentials remain on the worker. The resident never receives a main bot,
forge, person or supervisor credential. Do not put those credentials in its
service environment or explicit adapter configuration.

```sh
hataori worker run --root "$WORKER_ROOT" --main MAIN_INSTALLATION_ID --installation WORKER_INSTALLATION_ID
```

`--root`, `--main` and, for `run`, `--installation` are required. `HATAORI_ROOT`
is never a fallback. Run takes an exclusive private resident lock, loads the
registration and bounded journal, verifies owned detached hosts and their
waiting hooks, then listens. The socket is normally `state/worker/resident.sock`;
long roots use a deterministic private directory in the system temporary
directory (`worker.SocketPath` defines it). The socket has mode `0600`. A live
endpoint, regular file, unowned or ambiguous socket is never replaced. Shutdown
removes only the socket inode this resident created and releases its own lock.
It never signals a host, finishes a turn or answers a hook.

## User-service examples

These are manual templates; replace identities and executable/root locations
before installing. Service-manager lifecycle settings must preserve detached
hosts when only the resident exits. Logout, reboot and whole-account shutdown
are separate events and are not promised to preserve agents.

A Linux user unit (`hataori-worker.service`) can contain:

```ini
[Unit]
Description=Hataori worker resident

[Service]
Type=simple
ExecStart=%h/.local/bin/hataori worker run --root %h/.local/share/hataori-worker --main MAIN_INSTALLATION_ID --installation WORKER_INSTALLATION_ID
Restart=on-failure
KillMode=process
UMask=0077

[Install]
WantedBy=default.target
```

Place it in the user's systemd unit directory, then manually reload that user's
manager and enable/start the unit. `KillMode=process` intentionally leaves the
host processes outside the resident's lifecycle; the default control-group
mode would kill them. systemd discourages this setting for ordinary services
because surviving processes escape service lifecycle accounting. Here their
lifecycle is explicitly managed through owned host receipts, and stopping the
resident does not mean its agents stopped. See
[systemd.kill](https://www.freedesktop.org/software/systemd/man/latest/systemd.kill.html).

The macOS foundation is a LaunchAgent in the **logged-in GUI user's session**,
never an SSH-started resident or a system LaunchDaemon. A template for
`local.hataori.worker.plist` under that user's `Library/LaunchAgents` is:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>local.hataori.worker</string>
  <key>ProgramArguments</key><array>
    <string>ABSOLUTE_EXECUTABLE</string><string>worker</string><string>run</string>
    <string>--root</string><string>ABSOLUTE_WORKER_ROOT</string>
    <string>--main</string><string>MAIN_INSTALLATION_ID</string>
    <string>--installation</string><string>WORKER_INSTALLATION_ID</string>
  </array>
  <key>LimitLoadToSessionType</key><string>Aqua</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>AbandonProcessGroup</key><true/>
  <key>Umask</key><integer>63</integer>
</dict></plist>
```

Replace `ABSOLUTE_EXECUTABLE`/`ABSOLUTE_WORKER_ROOT` with absolute paths; plist
arguments do not expand shell variables. Bootstrap it manually into the logged-in
user's GUI domain. `AbandonProcessGroup` prevents launchd from killing remaining
processes in the resident's process group when it dies; detached hosts also
create their own sessions. `Aqua` restricts loading to the GUI session. These
keys are described in Apple's
[launchd.plist manual source](https://github.com/apple-oss-distributions/launchd/blob/main/man/launchd.plist.5).
No service manager has been operated as part of this change.

## Recovery and available operations

The worker protocol is independent of `docs/protocol.md`. This resident requires
worker protocol 1.1's `lifecycle` and `receipts` capabilities. It implements
`health`, `inventory`, `probe`, `receipt`, `prepare`, `start`, `end_host`, `input`
and `answer`. Whole-host termination is `end_host`; 1.0 `stop` means a named turn
interrupt and remains unsupported here. Replay, terminal attachment, transfers,
login automation and workspace operations are not advertised or implemented.
Uploaded policy handles refuse until the transfer gate exists. Only the own
holder is accepted. #961 must register a local workspace before a real launch;
a main-supplied path or guessed handle never supplies one.

A run has a worker-generated opaque host identity, private ownership marker and
exact installation/thread/session/run binding. A PID file or stored socket
pointer grants no authority. A live host must prove its identity on the same
connection used for an effect. Recovery reads same-identity final host status
when no live socket remains. A changed OS boot proves an old host absent, but
never proves an unobserved start succeeded. Inconclusive same-boot state blocks
readiness; nothing is silently adopted or relaunched.

Intent is persisted before an effect; the receipt records its request hash.
A repeated key returns the retained outcome and changed content refuses. Lost
start/end replies reconcile against owned live/final state. Uncertain input or
hook writes become `unknown` and are never replayed. Failed durable result
writes cannot return a cached `complete` outcome. Receipts are retained without
automatic deletion; the journal is bounded to 16 MiB and 4096 run records, so
capacity exhaustion refuses new work. There is no operator reset/purge command.

Per-configuration probes distinguish installed, missing, logged in, logged out
and unknown. They have a two-second timeout, with a four-second total health
budget. Resource samples expire after ten seconds (wire maximum sixty); unknown,
future or stale data never admits a start. Start also requires nonzero free
memory and disk. Main placement and the resident recheck configured admission
limits before a new host starts (worker protocol 1.3). Linux
uses native memory/load and filesystem free-space observations. Its `/proc`
read is only the OS boot UUID, never process identity or ownership. Darwin uses
native boot time, but its resource sampler currently reports unknown, so real
launch admission on macOS remains unavailable.

Connections are limited to 64; frame size is 1 MiB, chunks 64 KiB. Handshake and
writes have five-second deadlines; idle requests expire after sixty seconds.
Host identity checks have bounded deadlines; host end has a bounded forty-second
final-status wait. Operations serialize under the resident lock. Diagnostics
return fixed bounded refusals and do not echo provider output or login material.

## Evidence and platform gate

Isolated tests launch only the existing fake adapter and owned test hosts. They
cover keyed start across lost reply/restart, final stop reconciliation, continued
fake turns and waiting hooks after resident exit, unreplayed uncertain input,
failed persistence, root/installation replacement, permissions/lock, wrong
identities and stale telemetry. Probe tests replay committed real CLI captures;
no real provider, account login, SSH, tmux or service manager is used locally.
The same portable tests run in Linux and macOS CI, including the own PTY case.

No real macOS GUI-login, Keychain/login access, LaunchAgent restart or reboot
experiment has been performed. The template and cross-build are foundation
only. **macOS worker support remains unavailable until #969 records those
platform checks and supplies known resource telemetry.** CI fake-process
ownership does not substitute for that evidence.
