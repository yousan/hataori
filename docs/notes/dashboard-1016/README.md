# Dashboard: phone controls retain tiny desktop touch targets (#1016)

At 390 px, view buttons measure about 23 px, Back about 28 px, list summaries about 33 px, and footer actions about 19 px high.

Phone buttons and disclosure summaries now have minimum 44 × 44 px targets. Disclosure padding centers its label within the larger row; desktop toolbar dimensions stay unchanged.

## Evidence

All images use the isolated synthetic fixture of #1007; identifier text is omitted
after measurement. No installation data or configuration is used.

| Width | Before | After, light | After, dark |
| --- | --- | --- | --- |
| 1440 px | [Before](https://raw.githubusercontent.com/yousan/hataori/757f311/docs/notes/dashboard-1007/before/1440-light-en-question.png) | [After](after/1440-light-en-question.png) | [After](after/1440-dark-en-question.png) |
| 390 px | [Before](https://raw.githubusercontent.com/yousan/hataori/757f311/docs/notes/dashboard-1007/before/390-light-en-question.png) | [After](after/390-light-en-question.png) | [After](after/390-dark-en-question.png) |

### All after captures

- [1440-dark-en-list](after/1440-dark-en-list.png)
- [1440-dark-en-passkeys](after/1440-dark-en-passkeys.png)
- [1440-dark-en-question](after/1440-dark-en-question.png)
- [1440-dark-en-stop](after/1440-dark-en-stop.png)
- [1440-light-en-list](after/1440-light-en-list.png)
- [1440-light-en-passkeys](after/1440-light-en-passkeys.png)
- [1440-light-en-question](after/1440-light-en-question.png)
- [1440-light-en-stop](after/1440-light-en-stop.png)
- [390-dark-en-list](after/390-dark-en-list.png)
- [390-dark-en-passkeys](after/390-dark-en-passkeys.png)
- [390-dark-en-question](after/390-dark-en-question.png)
- [390-dark-en-stop](after/390-dark-en-stop.png)
- [390-light-en-list](after/390-light-en-list.png)
- [390-light-en-passkeys](after/390-light-en-passkeys.png)
- [390-light-en-question](after/390-light-en-question.png)
- [390-light-en-stop](after/390-light-en-stop.png)

Only sizing/spacing or overflow containment changes; no new layout structure or hierarchy.

## Validation

Pinned Node check script (173 tests, test-concurrency=1), build, gofmt, go vet (Go p=1) and focused page/origin tests (-parallel=1) pass after integrating current main and rebuilding generated assets. Synthetic list, question, passkeys and stop captures at both widths/themes pass >=44 x44 px phone button/disclosure targets; desktop remains unchanged. Baseline fails on 19–33 px controls. Fresh independent Codex review reports no findings. No local tmux test.

Fresh independent Codex review: no findings; a second fresh Codex session reviewed the integrated 11-line CSS change and confirmed narrow scope and unchanged event/permission/input semantics.

Concurrency: Resumed existing PR after rechecking open PRs and remote branches. Shared dashboard CSS/generated assets: #1054, #1050, #1047, #1042, #1039, #1035, #1032, #1029, #1013. Main 6092ab61 is integrated; previous findings stay on main and only this finding remains in this PR’s diff.
