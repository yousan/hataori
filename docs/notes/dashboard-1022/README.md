# Dashboard: Send and Answer buttons look like browser defaults (#1022)

Send, Answer and approval buttons only inherit their font. They use browser-default borders and fills beside explicitly styled Type/Stop/view controls.

Give Send, Answer, question choices and approval actions the same explicit theme-aware fill, border, radius and padding, with disabled and accent keyboard-focus states. Long labels wrap. All handlers and action semantics stay unchanged.

## Evidence

All images use the isolated synthetic fixture of #1007; identifier text is omitted
after measurement. No installation data or configuration is used.

| Width | Before | After, light | After, dark |
| --- | --- | --- | --- |
| 1440 px | [Before](https://raw.githubusercontent.com/yousan/hataori/757f311/docs/notes/dashboard-1007/before/1440-light-en-question.png) | [After](after/1440-light-en-question.png) | [After](after/1440-dark-en-question.png) |
| 390 px | [Before](https://raw.githubusercontent.com/yousan/hataori/757f311/docs/notes/dashboard-1007/before/390-light-en-question.png) | [After](after/390-light-en-question.png) | [After](after/390-dark-en-question.png) |

### All after captures

- [1440-dark-en-approval](after/1440-dark-en-approval.png)
- [1440-dark-en-composer](after/1440-dark-en-composer.png)
- [1440-dark-en-long-question](after/1440-dark-en-long-question.png)
- [1440-dark-en-question](after/1440-dark-en-question.png)
- [1440-light-en-approval](after/1440-light-en-approval.png)
- [1440-light-en-composer](after/1440-light-en-composer.png)
- [1440-light-en-long-question](after/1440-light-en-long-question.png)
- [1440-light-en-question](after/1440-light-en-question.png)
- [390-dark-en-approval](after/390-dark-en-approval.png)
- [390-dark-en-composer](after/390-dark-en-composer.png)
- [390-dark-en-long-question](after/390-dark-en-long-question.png)
- [390-dark-en-question](after/390-dark-en-question.png)
- [390-light-en-approval](after/390-light-en-approval.png)
- [390-light-en-composer](after/390-light-en-composer.png)
- [390-light-en-long-question](after/390-light-en-long-question.png)
- [390-light-en-question](after/390-light-en-question.png)

## Wireframes rendered before source changes

Choose A: neutral outlined actions match the existing Type/Stop controls and do not assign primary emphasis to every alternative, including Allow/Deny. B fills every choice with accent color, making competing choices equally prominent.

- [A, 1440-dark-en-question](wireframe-A/1440-dark-en-question.png)
- [A, 1440-light-en-question](wireframe-A/1440-light-en-question.png)
- [A, 390-dark-en-question](wireframe-A/390-dark-en-question.png)
- [A, 390-light-en-question](wireframe-A/390-light-en-question.png)
- [B, 1440-dark-en-question](wireframe-B/1440-dark-en-question.png)
- [B, 1440-light-en-question](wireframe-B/1440-light-en-question.png)
- [B, 390-dark-en-question](wireframe-B/390-dark-en-question.png)
- [B, 390-light-en-question](wireframe-B/390-light-en-question.png)

## Validation

Pinned Node install/check (173 tests)/build, gofmt, go vet and focused page tests pass. Baseline buttons use 0 px browser-default corners and short choices are 24.19 px high. The fixed controls use 4 px corners with at least 28 px height; normal and long-label questions, approval and composer are captured in both widths/themes. Fresh Codex review found no issues. No local tmux tests.

Fresh independent Codex review: no findings.

Concurrency: Open PRs #1042, #1039, #1035, #1032, #1029, #1028, #1027, #1026, #1013 share styles.css/generated page files; each finding uses a separate source hunk and starts from current main. Old #990 shares CSS but is merged; remote branches were checked before the source edit.
