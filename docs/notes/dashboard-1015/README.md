# Dashboard: phone thread rows are only 24 px high (#1015)

Ordinary phone thread rows measure about 24 px, while attention rows measure about 41 px. The later tree box rule overrides the phone row padding.

Phone thread rows now have a 44 px minimum with vertically centered content. Terminal rows also keep a 44 px minimum. Desktop row density and all actions stay the same.

## Evidence

All images use the isolated synthetic fixture of #1007; identifier text is omitted
after measurement. No installation data or configuration is used.

| Width | Before | After, light | After, dark |
| --- | --- | --- | --- |
| 1440 px | [Before](https://raw.githubusercontent.com/yousan/hataori/757f311/docs/notes/dashboard-1007/before/1440-light-en-list.png) | [After](after/1440-light-en-list.png) | [After](after/1440-dark-en-list.png) |
| 390 px | [Before](https://raw.githubusercontent.com/yousan/hataori/757f311/docs/notes/dashboard-1007/before/390-light-en-list.png) | [After](after/390-light-en-list.png) | [After](after/390-dark-en-list.png) |

### All after captures

- [1440-dark-en-list](after/1440-dark-en-list.png)
- [1440-dark-ja-list](after/1440-dark-ja-list.png)
- [1440-light-en-list](after/1440-light-en-list.png)
- [1440-light-ja-list](after/1440-light-ja-list.png)
- [390-dark-en-list](after/390-dark-en-list.png)
- [390-dark-ja-list](after/390-dark-ja-list.png)
- [390-light-en-list](after/390-light-en-list.png)
- [390-light-ja-list](after/390-light-ja-list.png)

Only sizing/spacing or overflow containment changes; no new layout structure or hierarchy.

## Validation

Pinned Node check script (all 173 tests, Node test-concurrency=1) and build pass; generated assets rebuilt after integrating current main. gofmt, go vet (Go p=1) and focused page-serving/origin tests (-parallel=1) pass. Baseline row target check fails at 24.19/40.98 px; the integrated fix passes at >=44 px in English/Japanese and both themes. Fresh synthetic captures retain the already-merged filter fix. No local tmux test.

Fresh independent Codex review: no findings; a second fresh Codex session reviewed the current-main integration and confirmed phone-only geometry and unchanged roles/actions/permissions.

Concurrency: Resumed after checking all open PRs and recent remote branches. Remaining design PRs share styles.css/generated assets; #1013 and #1054 share generated assets. Current main 5285fcde is integrated, preserving #1014; the final diff contains only #1015.
