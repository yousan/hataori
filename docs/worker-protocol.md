# Worker protocol

Status: protocol foundation for #957, part of #956. This document specifies
worker protocol **1.0**. It is independent of the control protocol described in
`protocol.md`; a control socket and an unversioned host socket are never worker
endpoints. The resident, SSH transport and runner integration ship in later
issues. There is no remotely usable worker in this change.

## Compatibility and golden files

`internal/workerproto` defines the encoding and validates messages before a
dispatcher may look up a host, open a resource or execute anything. Its examples
live under `internal/workerproto/testdata/protocol/<major.minor>/`. Every request,
reply, stream kind and refusal has a golden example. The `SUMS` manifest freezes
each version's complete example set. Never regenerate a released version or its
checksum. Change `workerproto.Version`, add a new directory and append its sum.

A major change breaks an existing promise, field meaning, required field or
authority boundary. A minor change adds optional fields, operations or
capabilities. Peers with different majors refuse before any effects, naming
both versions. Same-major peers use the lower minor and the intersection of
their offered capabilities; an operation requires its negotiated capability.
Unknown operations and required capabilities refuse. Unknown optional fields
are ignored; they never become addresses, authority or commands. Neither peer
infers support just from a larger minor. Capability additions must preserve the
semantics of already negotiated operations.

## Trust boundary

The main initiates SSH to an already running, private resident socket. The
operator registers a main installation and a worker installation out of band.
The hello must match that existing binding on both sides. A hello cannot create
or replace a binding. There is no owner-by-default hello, person identity,
supervisor credential, arbitrary command, environment, socket address or file
path in this protocol. A worker is never an instance control socket. A
replacement installation cannot serve the old installation's sessions.

Connection messages name the binding. Every session operation, reply and
stream additionally names the exact thread, session and host run recorded by
the main and resident. The main reserves a fresh run identity before starting
a host; a PID is diagnostic data only and never authority. The receiver checks
all identities before resolving a handle or touching a resource. Inventory may
introduce only records durably owned by the registered main on this worker;
the main reconciles these with its own records before readiness. Unexpected
threads are quarantined, never adopted into another thread. A connection has
no implicit current session.

The root layout is **not a sandbox**. Compromising the main gives an attacker
the reach of its configured SSH credentials and the worker operations available
to that main. Compromising the worker OS user exposes that user's workspaces,
agent/repository credentials and processes. A malicious worker can forge output
for its own sessions. Main-side scope checks remain necessary; worker output
never authorizes another session or promotes a caller. No bot, forge, person or
supervisor token is copied to a worker. SSH agent forwarding is disabled.

## Transport and limits

Messages are UTF-8 JSON objects, one per line, on a private duplex connection.
Binary data uses JSON base64. A frame, including its newline, is at most 1 MiB;
one byte chunk is at most 64 KiB. Oversize, duplicate JSON keys, trailing JSON,
malformed versions and invalid identities refuse before dispatch. Opaque IDs
are 1–128 ASCII letters, digits, `_` or `-`; they cannot be paths or addresses.
Requests and replies carry a nonempty request ID. Stream frames carry the ID
of their opening request. No request ID conveys authority.

The hello exchanges `version`, `binding` and `capabilities`. Successful
negotiation returns the negotiated minor and capabilities with the same
binding. Refusals are bounded structured codes with a safe explanation; they
must never echo credentials, login material or untrusted diagnostic payloads.
The message kinds, bodies, operation table and crash/replay rules below are
normative; golden files demonstrate their encodings.

## Implementation status

This foundation does not launch processes, open sockets, read files or remove
directories. Future dispatchers must call the binding/target/capability guards
before doing so, and implement the durable rules below before advertising a
capability. Specification tests run on Linux and macOS without tmux, agent
credentials, a service manager or an operator's SSH configuration. Runtime
platform conformance, including macOS login-session access, remains #969.
