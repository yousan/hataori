# Dashboard design pass (#1007)

Review the built dashboard on an isolated HTTP test server with fictional data,
at 1440 × 900 and 390 × 844 CSS pixels, in both themes. No installation data or
configuration is used. The compact tab strip from #990 / PR #992 is the baseline.

Evidence and the prioritized findings will be added here after the first capture.

Concurrency at start: no open PR changes the dashboard sources, browser checks,
or generated page. Old #724 and #990 branches share conversation/catalog and
tab-bar/CSS files respectively; their PRs have already merged. Check again before
each finding is started and before any source edit.
