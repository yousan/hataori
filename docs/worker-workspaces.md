# Worker workspaces and session files

Implementation of #961, part of #956. The selected resident keeps independent
clones, mirrors, session files and retained trash on its own machine. Workspace
writes have durable receipts and recorded identities before rename; uncertain
outcomes reconcile without adopting or overwriting another directory.

Removal must rerun the existing full worker-local proof immediately before
rename and require explicitly independent proof destinations. Trash cannot be
purged before main acknowledgment and retention. Session-file transfers use
bounded session handles, never paths opened on the main.

The implementation will document shipped configuration, tested crash/identity
and proof/transfer limits. Main routing and placement remain later work.
