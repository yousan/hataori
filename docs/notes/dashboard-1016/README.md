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

Pinned Node install/check (173 tests)/build, gofmt, go vet and focused page-serving tests pass. Synthetic browser checks cover list, question, passkey dialog and stop confirmation at both widths/themes. The baseline fails on 19–33 px controls; every tested phone button/disclosure is now at least 44 × 44 px. Desktop captures remain unchanged. Fresh Codex review found no issues. No local tmux tests.

Fresh independent Codex review: no findings.

Concurrency: Open PRs #1027, #1026, #1013 share styles.css/generated page files; each finding uses a separate source hunk and starts from current main. Old #990 shares CSS but is merged; remote branches were checked before the source edit.
