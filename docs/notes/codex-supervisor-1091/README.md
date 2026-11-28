# Capture metadata normalization (#1091)

This follow-up normalizes deployment-specific metadata in the disposable
Codex supervisor captures. It does not change tool calls, turn ordering,
confinement settings, timing evidence or the instruction-readiness gate.

Acceptance requires recursive normalization in both capture writers,
regression coverage using captured record shapes, and an audit of sibling
captures without printing private values. Independent code and confinement
reviews and green final-head and post-merge main CI remain required.

No production inputs, configuration, logs or operations are needed.

The affected `supervisor_gate` and `supervisor_direct` wire captures each
contain two remote-control status records. Only their `serverName` strings
change to `fixture-server`; parsing before/after proves every other value,
record and ordering is identical. The recursive normalizer targets that
notification's metadata, including envelopes and arrays, rather than
rewriting tool arguments or text that happens to use a similar key.

Both writer factories are exercised with these real captured envelopes and
a synthetic identifying name. The regression checks normalization without
mutating the input, preservation of absent metadata and all sibling capture
records. Test failure messages do not echo identifying values. The actual
adapter export still filters out remote-control notifications, but uses the
same normalizer for any nested metadata that reaches its selected outputs.

The related artifacts were audited for repeated identifying names and the
existing home-path, email, UUID, opaque-token, API-key and JWT patterns.
No additional identifying values were found. This is an audit of the stored
disposable captures, not permission to read production data or credentials.

Local validation: formatting and vet pass; the Codex package and focused
supervisor adapter-selection/switch importer cases pass. Disabling the
normalizer makes the writer regression fail, and restoring it passes.
Full Linux/macOS suites run in CI; no live agent or local tmux test is needed.
