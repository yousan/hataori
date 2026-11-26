# Dashboard: phone message and answer textareas use 13 px text (#1021)

Phone message and answer fields inherit the 13 px desktop body font, making the editable text smaller than typical phone forms.

Use 16 px editable text in narrow message and answer textareas. Desktop fields retain 13 px text; entry and send behavior are unchanged.

## Evidence

All images use the isolated synthetic fixture of #1007; identifier text is omitted
after measurement. No installation data or configuration is used.

| Width | Before | After, light | After, dark |
| --- | --- | --- | --- |
| 1440 px | [Before](https://raw.githubusercontent.com/yousan/hataori/757f311/docs/notes/dashboard-1007/panels/1440-light-en-composer.png) | [After](after/1440-light-en-composer.png) | [After](after/1440-dark-en-composer.png) |
| 390 px | [Before](https://raw.githubusercontent.com/yousan/hataori/757f311/docs/notes/dashboard-1007/panels/390-light-en-composer.png) | [After](after/390-light-en-composer.png) | [After](after/390-dark-en-composer.png) |

### All after captures

- [1440-dark-en-composer](after/1440-dark-en-composer.png)
- [1440-dark-en-question](after/1440-dark-en-question.png)
- [1440-light-en-composer](after/1440-light-en-composer.png)
- [1440-light-en-question](after/1440-light-en-question.png)
- [390-dark-en-composer](after/390-dark-en-composer.png)
- [390-dark-en-question](after/390-dark-en-question.png)
- [390-light-en-composer](after/390-light-en-composer.png)
- [390-light-en-question](after/390-light-en-question.png)

Only sizing/spacing or overflow containment changes; no new layout structure or hierarchy.

## Validation

Pinned Node check (173 tests)/build, gofmt, go vet and focused page tests pass. Baseline phone composer and answer textareas fail at 13 px. Both now measure 16 px at 390 px and remain 13 px at 1440 px in both themes. Eight synthetic captures are included. Fresh Codex review found no issues. No local tmux tests.

Fresh independent Codex review: no findings.

Concurrency: Open PRs #1039, #1035, #1032, #1029, #1028, #1027, #1026, #1013 share styles.css/generated page files; each finding uses a separate source hunk and starts from current main. Old #990 shares CSS but is merged; remote branches were checked before the source edit.
