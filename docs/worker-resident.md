# Worker resident

Implementation of #959, part of #956. The resident belongs to one worker
installation and one registered main. It runs in the operator's user session,
builds agent launches and probes locally, and reconnects its owned detached
hosts before readiness. SSH only forwards its private socket; it never starts
the resident or an agent.

The resident is a foundation for the first-worker gate, not placement or a
remotely usable worker by itself. SSH management, runner decoding, workspace
operations and main-side placement are separate concerns in #956. The worker
protocol and its immutable golden files are independent of the control
protocol. No deployment is part of this implementation.

## Installation and recovery requirements

The root and socket are private to their OS user. Initial registration is
explicit, binds installation identities rather than hostnames, and never
rebinds an existing root to a different main. A resident lock prevents two
residents serving the same installation. A replacement installation cannot
serve existing sessions.

Host launches and stops have durable operation receipts. Recovery inventories
owned runs and waiting hooks before accepting new launches, without treating a
PID as cross-machine authority. Resident exit leaves detached hosts and hooks
alive. Unsupported remote holders are refused.

Manual Linux user-service and macOS LaunchAgent examples, command/configuration
fields, tested limits and isolated fake-agent recovery evidence are added with
the implementation. Real macOS login-session access remains unverified; macOS
worker support must not be advertised until the #969 platform gate is met.
