# Local Git protocol captures

Captured from actual `git version 2.43.0` receive-pack using a new disposable bare repository, generated empty-tree base/head/two-parent merge objects, and integration/candidate refs. No remote repository, credential, live ref or production data was used. `advertisement.pkt` is actual stateless advertise-refs output with the smart-HTTP service header; `success.pkt` is actual report-status output for the atomic base update plus explicit candidate no-op assertion and an empty pack. `transaction.json` records those generated object identities.

These captures prove parsing of real Git output only. Tests also execute the real local receiver against both ref conflicts and a rejecting hook. Neither is GitHub provider/rules/PR-recognition proof or live authority approval.
