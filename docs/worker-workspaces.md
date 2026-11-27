# Worker workspaces and session files

The #961 resident implements the worker protocol's existing `workspace` and
`transfer` capabilities. Clone, fetch, inspect, remove, restore, acknowledge,
purge, upload and download resolve receiver-registered targets and opaque
handles on the selected worker. The main supplies neither a repository URL nor
an OS pathname. Main routing/placement and attachment presentation in the remote
runner ship in #962 and later first-worker steps; this foundation does not
make a configured machine usable by the chat daemon. No directory is migrated.

## Local configuration

Add optional `workspaces` to the worker's private `config/worker.json`, alongside
`agents`. These are synthetic example paths and identities:

```json
{
  "agents": [],
  "workspaces": {
    "machine": "worker-machine",
    "main_machine": "main-machine",
    "repositories": {"project": "https://forge.example.invalid/team/project.git"},
    "roots_complete": true,
    "roots": [
      {"machine": "worker-machine", "path": "/srv/worker-root"},
      {"machine": "main-machine", "path": "/srv/main-root"}
    ],
    "proof_destinations": [
      {
        "address": "https://forge.example.invalid/team/project.git",
        "machine": "independent-forge",
        "path": "/team/project.git",
        "independent": true
      }
    ],
    "retention_seconds": 604800
  }
}
```

| Field | Default | Meaning |
| --- | --- | --- |
| `machine`, `main_machine` | empty | Canonical physical machine identities for this worker and its main, required for a complete proof catalog. They may be equal when both installations share a machine. |
| `repositories` | empty | At most 128 opaque repository IDs mapped to worker-local Git addresses. Clone selects an ID; fetch uses the registered ID, not mutable origin settings. |
| `roots` | empty | At most 128 canonical machine/path pairs for **all** configured hataori roots, including this worker and the main. Paths must be absolute, clean and other than `/`. |
| `roots_complete` | false | Operator assertion that every root is listed. False, a missing own-root entry or an absent main-machine entry refuses proof; completeness of all main-root paths remains operator-asserted. |
| `proof_destinations` | empty | At most 128 exact independent addresses, each with canonical `machine`, absolute `path` and `independent: true`. No inferred aliases. |
| `retention_seconds` | 0 | Positive retention after the completed removal before acknowledged trash can be purged; at most 365 days. Zero disables purge. |

Machine identities are operator-assigned canonical physical identities,
separate from installation binding IDs. Use the same identity for every alias
and installation on one physical machine, including a main and worker sharing
that machine. Include every root on those machines. These are trusted local declarations, never worker reports or
main-supplied request fields. When worker and main share a physical identity, their separate root paths
still must be listed; a matching identity alone cannot establish completeness.
Misclassifying a machine or omitting a root is
outside the proof's trust boundary. Repository credentials and Git/SSH settings
are the worker OS user's own; no main credential, shared mount or forwarded
agent is required. Changing the worker configuration requires reopening the
resident; this implementation performs no service-manager operation.

Removal refuses an address not declared exactly, any Git URL rewrite, a proof
path inside any cataloged root on its declared machine, and SSH/file proofs on
another configured machine even outside its roots. SCP-style and inferred home
paths are not classified as proof destinations. Explicit `ssh://` paths must
match the declaration and cannot contain home expansion. `file://` is accepted
only on this worker with an explicit absolute destination outside every root;
the existing local proof still resolves links, alternates and object links.
HTTP proof fetches disable redirects. Git server independence is an operator
assertion: SSH/HTTP cannot establish where an arbitrary server stores its
objects. Do not declare a server independent if its objects depend on a root.

## Identity, proof and recovery

Clones have independent objects, the repository origin, and the existing
worker-local non-pruning mirror cache. Removing a mirror never removes the
clone's objects. Fetch explicitly refreshes the configured repository without
pruning or resetting the checkout. Registered clone/fetch share a two-minute
operation deadline across mirror work and checkout/fallback Git commands; a
stalled repository cannot retain the resident request lock indefinitely. The
resident owns its root exclusively;
this is not a sandbox against another process controlling that same OS account.

Before clone publication the journal records its random handle, staging path,
device and inode. Clone contents and directory entries are synced before
committing success. Removal records an exact workspace/trash plan, reruns the
entire existing proof on the worker with its Git, lsof and locks, then renames
without replacement. A live/uncertain host must end first. Inspection is
bounded diagnostic information and never grants removal. Existing checks for
modified/staged/untracked/ignored files, refs, reflogs, stash, nested repositories,
submodules, process use, Git/lsof failures and timeout still apply.

Lost replies return retained receipts for the same operation key and content.
After a crash, only a journaled clone/trash inode can be reconciled; another
directory at its name is preserved and the outcome stays unknown. An unperformed
remove intent is refused without rerunning removal on reconnect. Restore
records its identity before an exclusive rename; it never overwrites an empty
replacement either. Uncertain fetches/uploads are never blindly replayed.
Unknown filesystem outcomes make mutations unavailable pending reconciliation;
receipt/status reads remain available. No guessed cleanup or adoption occurs.

Trash retention starts after the successful rename; recovery starts it
conservatively when it confirms the trash identity. Purge requires a durable
main acknowledgment **and** elapsed retention. An interrupted authorized purge
can finish only against its recorded inode. It deletes children through an
open root for that inode and leaves the empty directory as a tombstone: POSIX
has no inode-conditional directory unlink. Tombstones are not a promise of
space reclamation for directory metadata. Legacy time-name purge ignores the
worker's opaque trash names. External, taken-in and confined directories cannot
be registered as worker-created workspaces or selected for worker removal.

## Session files and limits

Each run has at most 16 handles, including incomplete uploads, outgoing
snapshots and unjournaled directory entries left by a crash. A file is at most
8 MiB, with at most 64 KiB per protocol chunk. Upload requires an opaque handle,
whole-file SHA-256, size and exact next offset. It creates mode-0600 files
exclusively, records the inode before writing, syncs accepted bytes and verifies
the complete digest before exposing completion. Retries with the same key
return the receipt; another key cannot overwrite accepted bytes or reuse a
completed handle. An interrupted chunk is reconciled only if the exact recorded
file, length and bytes agree. It is never rewritten during recovery.

Accepted chunks remain in durable receipts until future run-retirement support.
The shared journal is bounded to 16 MiB. Upload admission reserves 4 MiB for
reconciliation and other operations, accounting for encoded chunk/receipt and
metadata overhead before any effect. Thus 8 MiB is a per-file maximum, **not**
a promise of sixteen maximum-size files: aggregate retained receipts can limit
a later upload partway through. `limit` leaves its offset, bytes and receipt
unchanged, does not mark the partial handle complete, and keeps unrelated
operations ready. There is no automatic receipt eviction or partial-file GC.

`Resident.CaptureOutgoing` is a trusted worker-local adapter API. It snapshots a
relative regular file from the exact registered workspace, refuses traversal,
symlink components and oversized/changing input, and returns only a session
handle. Download reads that immutable registered snapshot in bounded chunks
with size/digest checks. No worker pathname is returned for the main to open.
Policy handles in `prepare` remain unsupported; remote-runner integration is
separate from this transfer foundation.

Tests use independent temporary main/worker/repository roots and a fake agent.
They cover independent clone/cache removal, declared independence, proof refusal
classes, before/after rename crashes, replacement directories, acknowledgment/
retention/purge recovery, handle/offset/digest/size boundaries, orphan capacity,
aggregate upload admission and outgoing snapshots. Guard-removal experiments
must make their regressions fail. Full Linux/macOS CI runs the existing local
proof suite as well. Worker wire versions/goldens and the public control
protocol are unchanged. Full macOS worker support remains gated by #969.
