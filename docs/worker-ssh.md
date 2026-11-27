# Worker SSH transport

Implementation of #960, part of #956. The main opens system OpenSSH to an
already-running resident in the worker's user session. It uses the operator's
keys, authentication agent and configuration, with private control/forward
sockets. It never signs into a dashboard, writes SSH credentials/configuration
or launches a resident/agent through SSH.

Readiness requires the registered installation binding, compatible worker
protocol and owned-host reconciliation. A disconnected or unverified transport
is unavailable; it never retries an uncertain worker operation. Connection
retries have bounded keepalive, authentication deadlines and backoff.

The transport guide will document its API/configuration mapping, tested limits,
SSH argument protections and isolated Linux/macOS fixture evidence with the
implementation. Actual macOS user-session support remains gated by #969.
