# GitHub captures

Real GitHub REST API answers for the adapter's tests. Each JSON file holds the request, HTTP status, selected response headers and parsed response body.

The two lists come from urfave/cli with `per_page=5`; the issue list's two pages hold one issue among pull requests. The detail reads come from cli/cli (issue 15104 and pull request 14910). The ref checks are for a commit in urfave/cli. The requests are the adapter's own, except for the lists' page size, which the tests lower to 5.

The comparisons come from cli/cli: pull request 14910's merge commit with the trunk commit four commits after it, each way, with itself, and with a commit of the branch merged there, which does not contain it; and with a commit that does not exist.

They were taken unauthenticated and hold no token. The rate-limit answer's IP address was replaced with a documentation address.

Taken on 2026-11-23 with API version `2022-11-28`.

## How they were taken

```sh
python3 -I capture.py answers <dir>
python3 -I capture.py ratelimit <dir>
python3 -I capture.py compare <dir>
```

The comparisons were taken later the same day, with `compare`.

## Captures by read

| Read | Captures |
|---|---|
| Issue list | `issues-page-1.json`, `issues-page-2.json` |
| Issue and comments | `issue.json`, `issue-comments.json` |
| Pull request list | `pulls.json` |
| Pull request | `pull.json`, `pull-reviews.json`, `pull-comments.json`, `pull-review-comments.json`, `pull-files.json`, `pull-check-runs.json`, `pull-status.json` |
| Ref checks | `ref-check-runs.json`, `ref-status.json` |
| Comparison of two commits | `compare-ahead.json`, `compare-behind.json`, `compare-identical.json`, `compare-diverged.json`, `compare-not-found.json` |
| Refusals | `not-found.json`, `rate-limited.json` |
