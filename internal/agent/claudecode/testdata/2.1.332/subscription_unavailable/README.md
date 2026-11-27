# Subscription rejection

Source: the sanitized real CLI evidence supplied in issue #1086, Claude Code
2.1.332. `result.json` preserves the issue's print-mode fields verbatim. No new
provider call was made. The provider's suggested cause is not verified account
billing information.

The issue also reports an interactive transcript with `error: oauth_org_not_allowed`,
`isApiErrorMessage: true`, `apiErrorStatus: 403`, and
`apiErrorCode: oauth_not_allowed_for_organization`. The original sanitized
interactive record and StopFailure hook have been requested before adding a
capture-based regression; those records must not be reconstructed from memory.
