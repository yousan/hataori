# Capture metadata normalization (#1091)

This follow-up normalizes deployment-specific metadata in the disposable
Codex supervisor captures. It does not change tool calls, turn ordering,
confinement settings, timing evidence or the instruction-readiness gate.

Acceptance requires recursive normalization in both capture writers,
regression coverage using captured record shapes, and an audit of sibling
captures without printing private values. Independent code and confinement
reviews and green final-head and post-merge main CI remain required.

No production inputs, configuration, logs or operations are needed.
