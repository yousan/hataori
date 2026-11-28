# Slack captures (in progress, #341)

This directory is capture tooling and evidence, not a Slack driver. The design
prerequisite was merged in #1031. Captures use only operator-designated fresh
private test channels and the operator's own bot DM. Public-channel evidence is
not provisioned. Never enumerate conversations or workspace members.

The manifests describe the provisioned internal Socket Mode app and the second
incoming-webhook app. Credentials and raw recordings stay outside the repository.
Person scenarios, acknowledgment experiments and observed findings will be added
as real recordings become available. No unobserved behavior is established here.
