# Settings-free research capability (#1117)

An ordinary research worker must omit the repository's project settings and
hooks. A confined supervisor is a different launch contract: support for it
does not establish ordinary settings-free research support.

This change will expose the selected worker adapter's capability and reject
unsupported research before creating a thread, workspace, session or launch.
The runner's independent launch-attestation refusal remains required.
Supported adapters must retain their settings-free launch behavior. No
provider switch or settings-inheriting fallback is permitted.

Acceptance requires capability refresh in supervisor instructions/tool
information, a precise pre-artifact refusal, regressions for unsupported and
supported adapters, and attributed existing-worker/operator evidence fallbacks
when neither safe research nor forge access is available. Independent
implementation and authority reviews and the adopted full final-head and
actual merge-main CI gates remain required. This initial draft records scope;
it is not implementation or completion evidence.
