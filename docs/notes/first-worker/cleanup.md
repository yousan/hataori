# First-worker fixture process ownership follow-up

This supplements #1099 for #965. It changes test infrastructure, with no
product configuration field, public interface or protocol version change.
The fixture's main can be killed and restarted; the test parent retains the
creator handles and private records of every real SSH client and each
inetd-mode `sshd` connection monitor. A Go-owned loopback listener replaces the
forking SSH listener, so established connection monitors remain direct children.
`MaxSessions 0` permits forwarding and prevents remote command sessions.

## Ownership and exit barriers

The wrapper is a private Unix broker client. A synthetic fixture token and
kernel peer credentials bind requests to the registered main generation or
the explicitly registered in-process owner CLI. The broker accepts pinned
master/mux argument shapes, the fixture's generated-key configuration, its
own control directories/sockets and its worker endpoint. It refuses foreign
masters, replaced identities, arbitrary commands and forwarding destinations.
Wrapper parent loss closes its lease; generation shutdown also closes owned
transports independently of the wrapper's scheduling.

Each child has one supervisor that serializes nonblocking `Wait4` and signals
on its original creator handle. An unreaped child reserves its PID. After
reaping, that record never signals again. A caller-supplied PID, process name,
pidfile or foreign owner record grants no cleanup authority. TERM-resistant
owned children receive KILL, and cleanup waits for actual reaping. No process
group or discovered descendant is signalled by this fixture manager.

Established server children and wrapper helpers have separate read-only exit
evidence: kernel parent relationships and birth identities, retained after the
creator is reaped. Linux traverses tasks of the owned process; Darwin traverses
the creator's fixed private process group and filters parent relationships.
Generation shutdown refuses new handlers, closes owned streams, waits for
in-flight authorized handlers, then takes the final process snapshot. This
also covers a request paused immediately before child registration.

## Executable evidence

The focused serial gate retains all original first-worker scenarios and adds:

- `TestFirstWorkerSSHCleanupAcrossMainRestart`: established SSH master and
  server child, actual main SIGKILL, old-generation exit, restarted input and
  explicit final teardown.
- `TestFirstWorkerFixtureFailureCleanup`: a separate test subprocess deliberately
  fails with an established connection. Its success marker requires completed
  cleanup with no recorded cleanup error and confirmed child exits.
- `TestFirstWorkerProcessOwnershipAndExitBarrier`: foreign owner and PID-only
  record refusal, a separate fixture's continued life, TERM/KILL escalation,
  reaping before return and no signal after reaping.
- `TestFirstWorkerGenerationWaitsForAuthorizedHandler`: deterministic registration
  race; shutdown cannot return while an authorized handler is held.
- `TestFirstWorkerDescendantExitEvidence`: real child/grandchild traversal,
  graceful exit and preserved birth evidence after root reaping.
- `TestFirstWorkerSSHAuthorityRejectsForeignConnection`: correct token from an
  unregistered parent cannot create an SSH child; a foreign control socket stays
  usable and is not adopted.

`gate.py --mutations` records actual assertion failures for process ownership,
child exit waiting, generation quiescence and parent binding alongside the
original deduplication, catch-up and sole-copy deletion guard failures.
The independent cleanup backstop remains active during exit-barrier mutation.
All mutations restore their source in `finally`. Compilation failures, skipped
tests and whole-test timeouts are not accepted as guard evidence.

Exact final reviewed head, merge commit and Linux/macOS CI are recorded in
PR #1100 and #956 after those gates pass; this document does not predeclare
those results. Two fresh independent Codex reviews cover implementation and
separate authority/destructive boundaries on the final head.

The earlier macOS clone/publication uncertainty has no established causal link
to the cleanup defect. The original merge's retry also failed initial fixture
reconciliation and another test reported a fork resource error. Neither run
proves the cause or fulfills acceptance. A retry cannot replace this cleanup fix.

Tests remain isolated fake-worker/controlled boot-ID evidence. Actual OS reboot,
provider login and full macOS worker runtime conformance remain unverified.
Existing residual processes are outside this change. No production, service,
deployment, existing-process cleanup or optional-feature work is included.
