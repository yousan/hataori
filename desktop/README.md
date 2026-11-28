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

## Isolated checks and candidate delivery

The separate `Desktop` workflow classifies the complete frozen PR diff or
main push range. Documentation alone does not allocate a desktop build.
Drafts retain the existing lightweight/full-check split: Linux pure tests and
fixture compilation run; both standard hosted macOS architecture jobs wait
for normal ready/main validation. Neither skipped jobs nor compilation are
runtime acceptance. Existing CI and required check names are unchanged.

On standard disposable `macos-15` and `macos-15-intel` runners, the hosted proof
uses only scratch repositories, a fake chat service/agent, lifetime pipes and
the private native listener. It checks the existing embedded page/CSP in an
actual sandboxed Electron window, owner fetch/streams, duplex first-message
rules, navigation/capability refusals and a held fake turn across a daemon
lifetime replacement with the same live host run and resumed terminal offset.
The fixture stages two separately compiled daemon/helper versions and selects
the second through the production launcher while the fake turn is held. The
launcher closes and joins only its owned daemon pipe before the second starts;
the old referenced binary remains available. This is a hosted scratch proof,
not an interactive login/service-manager or signed-release acceptance check.
It never runs LaunchAgent installation or a real agent/login. Versioned
selection/checksum/schema and persistent-stop cases have separate inert file
fixtures. Hosted execution and interactive real-Mac acceptance remain separate
gates. `desktop/test/runtime-main.mjs` and
`TestDesktopHostedFixture` are hosted-only entry points; do not run them on an
operator host. The private helper fixture's normal-suite skip is not evidence.
Compatibility probes use the computed host endpoint and kernel peer UID, never
a saved socket pointer. A legacy host created through a different root alias
can have a different long-path hash; replacement then fails closed and keeps
the host, selection and pointer intact rather than adopting that endpoint.

A maintainer may create a stable candidate tag at an independently reviewed,
full-CI-green exact head to supply private device acceptance before merge.
This workflow never creates a tag. Only a same-repository tag push by the
repository owner can obtain release-write authority. Its checked DMGs,
checksums and manifests go only to an exact-source unpublished draft release;
PR/main jobs never upload unsigned build artifacts. Existing assets are never
overwritten and published/mismatched releases are refused. The maintainer
must keep the draft unpublished throughout delivery and until #1074 permits
public distribution. Creating a candidate tag, accessing a device, signing or
publishing is not authorized by these instructions.

Closing the window hides it; Quit leaves a registered launcher/daemon alone.
Stop is persisted before the launcher ends its owned daemon lifetime pipe.
Start is explicit. Manual selection verifies the staged bundle and preserves
Stop, configuration, data, hosts and old referenced binaries. An interrupted
first run can explicitly register its login launcher again from the menu;
readiness failure keeps recovery actions reachable. Disable login launcher
persists Stop and disables/boots out only the exact stored private plist and
its root-derived GUI-user service label. It retains all instance files and
binaries; removal of the app itself is manual. These lifecycle behaviors still
require hosted/runtime and the issue's interactive real-Mac acceptance; this
README does not mark them executed.

Packaging sets `osxSign: false` and `osxNotarize: false`. The pinned packager
can reset Electron's internal framework ad-hoc signature during its normal
rename step; it uses no Developer ID, certificate, Keychain or notarization
account. Developer ID signing, Gatekeeper acceptance and public distribution
remain under the existing later gates.
The DMG source contains `hataori.app` at its root and preserves Electron's
relative framework links; packaging checks the copied app's runtime version,
resource manifest and DMG integrity before computing its checksum.
