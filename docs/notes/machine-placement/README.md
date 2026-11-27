# Machine placement implementation (#963)

The accepted policy is in design section 6.4 E. Admission requires a fresh,
ready, reconciled machine, scope and agent permission, logged-in agent health,
and room under every configured resource limit. Rank eligible machines by
remaining resident-slot fraction, normalized load, free memory and key.

The main must commit the fixed machine and its reservation before workspace
creation. Automatic starts without room retain their thread, link and input
durably; explicit ineligible requests refuse without a fallback. Pending work
is reconsidered oldest first with current authorization and configuration.

Validation uses isolated fixtures and serial focused local checks. Full Linux
and macOS suites run in CI. This work does not establish actual macOS worker
runtime support. Final-head CI, independent fresh Codex reviews, merge,
post-merge-main CI and issue closure are required before completion.
