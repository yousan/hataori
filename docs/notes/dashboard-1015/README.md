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

Pinned Node install/check (173 tests)/build pass; build committed. Baseline browser target check fails on ordinary 24.19 px and attention 40.98 px rows. All ten fictional thread rows are now 44 px; all three terminal rows are 46.98 px in both languages/themes. Desktop captures confirm unchanged density. gofmt, go vet and focused page tests pass; no local tmux tests.

Fresh independent Codex review: no findings.

Concurrency: Open PR #1026 shares styles.css and built page files but changes the phone filter, not row sizing. No other open PR overlaps those files. Old #990 shares CSS but is merged; remote branches were checked before editing.
