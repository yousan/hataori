# Subscription rejection

Claude Code 2.1.332. `result.json` preserves the sanitized real print-mode
fields supplied in issue #1086 verbatim.

`assistant.json` and `StopFailure.json` are the actual interactive assistant
record and hook payload supplied by opu for #1086. They were extracted from
already-saved failure records, with IDs, paths, conversation text and unrelated
fields removed. The hook has no host envelope. No new provider call was made,
and no record was reconstructed. Tests provide their own turn correlation.

The provider's suggested cause is not verified account billing information.
Its billing and API-key sentence is provider text, not authorization to change
authentication, billing, or credentials.
