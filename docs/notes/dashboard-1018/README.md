# Dashboard: long trusted-file names are clipped in the dialog (#1018)

A long fictional trusted-file name extends outside the modal content area and is clipped at both desktop and phone widths.

Reset the trusted-files dialog’s inherited desktop footer nowrap, then allow file/repository and attribution spans to shrink and wrap. All text is retained and modal actions/data stay unchanged.

## Evidence

All images use the isolated synthetic fixture of #1007; identifier text is omitted
after measurement. No installation data or configuration is used.

| Width | Before | After, light | After, dark |
| --- | --- | --- | --- |
| 1440 px | [Before](https://raw.githubusercontent.com/yousan/hataori/757f311/docs/notes/dashboard-1007/before/1440-light-en-long-trusts.png) | [After](after/1440-light-en-long-trusts.png) | [After](after/1440-dark-en-long-trusts.png) |
| 390 px | [Before](https://raw.githubusercontent.com/yousan/hataori/757f311/docs/notes/dashboard-1007/before/390-light-en-long-trusts.png) | [After](after/390-light-en-long-trusts.png) | [After](after/390-dark-en-long-trusts.png) |

### All after captures

- [1440-dark-en-long-trusts](after/1440-dark-en-long-trusts.png)
- [1440-dark-en-trusts](after/1440-dark-en-trusts.png)
- [1440-light-en-long-trusts](after/1440-light-en-long-trusts.png)
- [1440-light-en-trusts](after/1440-light-en-trusts.png)
- [390-dark-en-long-trusts](after/390-dark-en-long-trusts.png)
- [390-dark-en-trusts](after/390-dark-en-trusts.png)
- [390-light-en-long-trusts](after/390-light-en-long-trusts.png)
- [390-light-en-trusts](after/390-light-en-trusts.png)

Only sizing/spacing or overflow containment changes; no new layout structure or hierarchy.

## Validation

Pinned Node 24.22.1: npm ci --ignore-scripts, complete npm check (173 tests) with Node test-concurrency=1, npm run build; generated page rebuilt after main integration. gofmt, go vet (Go p=1) and focused TestThePage tests (-parallel=1) pass. Eight refreshed synthetic captures show ordinary and long trusted-file names at 1440/390 px in both themes; all containment checks pass. Baseline-failing evidence is preserved. No local tmux tests.

Fresh independent Codex review: landing_review_1018: fresh independent Codex review after current-main integration found no findings; ten CSS insertions only, prior question and phone touch fixes preserved, no permission/input/action changes.

Concurrency: Resumed existing PR after rechecking open PRs and remote branches. Shared dashboard CSS/generated assets: #1054, #1050, #1047, #1042, #1039, #1035, #1013. Main e0ca177f is integrated; previous findings stay on main and only this finding remains in this PR’s diff.
