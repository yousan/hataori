# Dashboard: long questions and answer prompts overflow their panels (#1017)

An unbroken fictional question extends past the desktop details panel. The answer prompt also extends past its pane at both widths.

Wrap long question/failure card text and answer prompts inside their panels. Choice buttons and checkbox labels can shrink and wrap without truncating their labels. Existing actions remain unchanged.

## Evidence

All images use the isolated synthetic fixture of #1007; identifier text is omitted
after measurement. No installation data or configuration is used.

| Width | Before | After, light | After, dark |
| --- | --- | --- | --- |
| 1440 px | [Before](https://raw.githubusercontent.com/yousan/hataori/757f311/docs/notes/dashboard-1007/before/1440-light-en-long-question.png) | [After](after/1440-light-en-long-question.png) | [After](after/1440-dark-en-long-question.png) |
| 390 px | [Before](https://raw.githubusercontent.com/yousan/hataori/757f311/docs/notes/dashboard-1007/before/390-light-en-long-question.png) | [After](after/390-light-en-long-question.png) | [After](after/390-dark-en-long-question.png) |

### All after captures

- [1440-dark-en-failed](after/1440-dark-en-failed.png)
- [1440-dark-en-long-question](after/1440-dark-en-long-question.png)
- [1440-dark-en-long-split](after/1440-dark-en-long-split.png)
- [1440-dark-en-question](after/1440-dark-en-question.png)
- [1440-dark-en-split3](after/1440-dark-en-split3.png)
- [1440-light-en-failed](after/1440-light-en-failed.png)
- [1440-light-en-long-question](after/1440-light-en-long-question.png)
- [1440-light-en-long-split](after/1440-light-en-long-split.png)
- [1440-light-en-question](after/1440-light-en-question.png)
- [1440-light-en-split3](after/1440-light-en-split3.png)
- [390-dark-en-failed](after/390-dark-en-failed.png)
- [390-dark-en-long-question](after/390-dark-en-long-question.png)
- [390-dark-en-question](after/390-dark-en-question.png)
- [390-light-en-failed](after/390-light-en-failed.png)
- [390-light-en-long-question](after/390-light-en-long-question.png)
- [390-light-en-question](after/390-light-en-question.png)

Only sizing/spacing or overflow containment changes; no new layout structure or hierarchy.

## Validation

Pinned Node install/check (173 tests)/build, gofmt, go vet and focused page tests pass. On the baseline, the unbroken question has 1,183 px of content in a 788/370 px answer pane and 1,191 px in a 273 px details card. After the fix, all measured content stays within its container, at both widths/themes and in three split panes. Fresh Codex review found no issues. No local tmux tests.

Fresh independent Codex review: no findings.

Concurrency: Open PRs #1028, #1027, #1026, #1013 share styles.css/generated page files; each finding uses a separate source hunk and starts from current main. Old #990 shares CSS but is merged; remote branches were checked before the source edit.
