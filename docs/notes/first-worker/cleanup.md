# First-worker fixture process ownership follow-up

This supplements #1099 for #965. The main-restart gate must retain fixture
ownership of every new SSH client and established server connection after a
main SIGKILL, and confirm their termination on normal and failed fixture exits.
A bare PID, process name, old pidfile or unrelated connection grants no cleanup
authority. Existing residual processes are outside this follow-up.

The implementation will keep newly created process handles outside the main,
bind cleanup to the fixture's private connections, refuse foreign identities,
and prove actual exit across restart and failure. Guard-removal assertions and
two new independent Codex reviews, including authority/destructive boundaries,
are required on the final head. Existing first-worker acceptance evidence must
remain valid. Exact final-head and post-merge-main Linux/macOS CI must pass.

The earlier macOS clone/publication uncertainty has no established causal link
to the cleanup defect. A successful retry cannot replace the cleanup fix.
Tests remain isolated fake-worker/controlled boot-ID evidence; actual OS reboot,
provider login and full macOS worker runtime conformance remain unverified.
No production, service, deployment, existing-process cleanup or optional-feature
work is part of this change. This document initially records scope, not proof.
