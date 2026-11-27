# Worker SSH transport

The `internal/workerssh` library (#960, part of #956) opens system OpenSSH to an
already-running resident in the worker's user session. It uses the operator's
keys, authentication agent and configuration for authentication. It never signs
into a dashboard, writes SSH keys/configuration, or starts a resident/agent in an
SSH command. The main daemon configures routing and placement using the shipped
machine fields; see [First Linux worker](first-worker.md). There is no separate
transport CLI.

## Configuration and ownership

Create one `Pool` with an explicit private absolute `Options.RunDir`. Map a
configured `machines[].ssh` to `Config.Destination`, `machines[].root` to
`Config.Root`, and the previously pinned main/installation IDs to `Config.Binding`.
The root is a worker path; the main never opens it. The resident socket is derived
with `worker.SocketPath`, never accepted from a worker report. Configure SSH port,
identity, proxy and known-host files in the operator's existing OpenSSH config.
Destinations are aliases or `[user@]host`, without whitespace, options, slashes,
colon/port syntax or control characters. Root/socket names with literal spaces,
colons and backslashes work; socket names containing `$` or `%` refuse before
OpenSSH expansion. Long roots use the resident's existing short hashed socket.

One exact destination has one `Worker` and shared connection per pool. A different
root/binding for that destination refuses; another pool using the same run
directory cannot acquire its private, owned, no-follow destination lock. Keep
that directory stable and do not share it with another user. The first supplied
callbacks remain installed: they must read current receiver-owned storage.
Aliases resolving to the same host are distinct destinations; configure one
canonical alias per installation. This library does not create registration or
adopt an unknown installation from a hello.

`Status` and `Changed` allow nonblocking availability checks. `WaitReady(ctx)`
waits only when explicitly requested. `Open(ctx)` immediately refuses unavailable
workers and gives each ready caller an independent duplex `Stream`, with a fresh
validated hello and negotiated `Protocol`. Closing one stream leaves siblings
running. The application must decode replies against its own registered target,
not a target copied from the reply. The transport never replays application
writes: a lost reply requires the operation's durable receipt reconciliation.

## Readiness and failures

Every connection must pass pinned binding/version negotiation, ready health,
complete inventory with the same boot, and receiver-owned expected-target checks.
`Expected(ctx)` returns targets from main records; nil accepts only an empty
inventory. Unexpected resident targets refuse readiness. A missing expected
target is for main recovery to resolve, not proof of successful termination.
`Reconcile(ctx, Observation)` must durably finish/check main recovery before
readiness; it may refuse. Both callbacks must honor their bounded context.
There is no report-driven registration or agent start in this transport.

Master exit immediately invalidates availability; heartbeat/stream errors also
trigger an independent probe. Normal EOF on one stream does not alone declare a
shared outage. Reconnection closes old generation streams and repeats all
readiness checks. Failures expose fixed categories: `authentication`, `host_key`,
`forward`, `binding`, `protocol`, `reconciliation`, `transport`; lifecycle states
are `connecting` and `closed`. Specific refusal causes persist through teardown
and backoff. An unrecognised SSH diagnostic falls back to `transport` or
`forward`, never guessed authentication success. Raw stderr is limited to an
8 KiB in-memory tail for classification and never returned or logged.

| Setting | Default | Supported bounds |
|---|---|---|
| `ConnectTimeout` | 10 seconds | 100 milliseconds–30 seconds |
| `ProbeTimeout` | 2 seconds | 50 milliseconds–5 seconds, including callbacks |
| `ProbeInterval` | 3 seconds | 50 milliseconds–10 seconds |
| `BackoffMin` | 250 milliseconds | at least 50 milliseconds |
| `BackoffMax` | 30 seconds | at least minimum, at most 30 seconds |

Backoff doubles to its cap and resets after readiness. OpenSSH independently
uses a five-second connection timeout, one attempt, five-second keepalive and
two missed replies. Authentication is noninteractive: batch mode, no password or
keyboard-interactive authentication, no askpass. A local operator agent may
supply keys for authentication; it is never forwarded. An owned SSH PID gets
SIGTERM at teardown, then SIGKILL after two seconds if needed. A 250 millisecond
`WaitDelay` bounds inherited stderr pipe waits without signalling helper
processes. Trusted operator SSH configuration may select local proxy helpers;
this library never manages those helpers or remote processes.

## SSH and filesystem protections

The foreground master uses `-N -n -T`, a fresh private control socket, no
persisted background master and no remote session command. Agent/X11 forwarding,
GSSAPI credential delegation, local commands, key-agent additions, host-key
updates and IP-key additions are explicitly disabled; strict host-key checking
is required. Unknown hosts fail; there is no automatic acceptance. Authentication
keys/configuration/known-host bytes stay operator-owned and unchanged.

`ClearAllForwardings=yes` clears operator-configured forwards. Because it also
clears command-line forwards, the resident forward is then installed with a
separate `-O forward` command against the existing private master, reading
`-F /dev/null`. That control operation cannot silently fall back to new
authentication or re-add configured forwards. The sole forward is local Unix to
worker Unix; there is no TCP listener or reverse main control socket. See the
OpenSSH manuals for [`-N` and `-O`](https://man.openbsd.org/ssh.1) and
[`ClearAllForwardings`](https://man.openbsd.org/ssh_config.5).

Each attempt owns a short mode-0700 temporary directory and mode-0600 sockets;
no SSH credential or config is written there. Cleanup removes only recorded,
unchanged socket inodes and its own unchanged empty attempt directory.
Replacements and unknown files remain. Run-directory lock files remain reusable.
Neither worker workspace files nor operator SSH files are removed.

## Tested limits and platform evidence

Linux and macOS CI run `isolated worker SSH scenarios` verbosely, naming the
scenarios that actually ran. Missing `ssh`, `ssh-keygen` or an executable `sshd`
is a hard failure, not a skipped platform acceptance. Each fixture starts its own
foreground server, generated keys and explicit config on loopback, plus a real
resident with no configured agents. Tests never use an operator destination,
credential, provider, service manager or tmux.

The scenarios exercise real private Unix forwarding; concurrent session streams
on one master; owned-connection cut and bounded reconnect; renewed main
reconciliation; unknown host/authentication/refused-forward failures; incompatible
major and binding refusals; incomplete or foreign inventories; boot/correlation
mismatch; forced safe SSH arguments; unchanged keys/config/hostname-only known
hosts; inherited-stderr bounded shutdown; destination ownership; and cleanup
preserving replacements. SSH diagnostic parser tests use actual captured stderr
under `internal/workerssh/testdata`, retaining old captures alongside new ones.
Worker-protocol fixtures are independent of `docs/protocol.md`; this transport
changes neither public control nor worker protocol encoding/version/goldens.

CI evidence covers transport behavior only. macOS resident support remains
unavailable until #969 supplies real GUI login/Keychain/LaunchAgent, sleep and
native resource evidence; cross-builds and simulated network cuts do not prove it.
