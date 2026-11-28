# macOS desktop app

Implementation work for #1072. This directory is not yet a supported desktop
distribution. #1071 supplies the owner-only dashboard listener; this step must
keep its private discovery, peer-UID, ACL and fixed-origin checks.

The app uses the daemon's existing dashboard and CSP. Its main process owns
transport and lifecycle; a sandboxed, isolated renderer receives no Node or
general-purpose IPC capability. Browser sign-in credentials are never native
credentials. No owner TCP listener is permitted.

Acceptance is tracked separately for implementation, isolated hosted-runner
execution, packaging, and an explicitly authorized maintainer Mac. Draft or
skipped jobs, static checks and cross-builds are not runtime acceptance. Real
Mac installation, GUI login/Keychain availability, menu bar/notifications,
login startup, teardown and sleep/wake remain unverified. Unsigned outputs
must not be public Actions artifacts or published releases; #1074 owns the
public distribution/signing decision.

Only #1072 is in scope. Windows/WSL work waits for #1073. There is no deployment
or new app sign-in. Existing instance data, configuration, logins, detached
hosts and referenced binaries must survive stopping, updating and uninstalling.

Required gates: independent Codex implementation review, separate authority
review, full final-head Linux/macOS CI and full CI on the exact main merge.
The primary issue requires Codex review and prohibits running Claude.
