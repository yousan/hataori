# Dashboard: text controls lack consistent hover and focus feedback (#1023)

Density, Active/All, view choices and outlined actions have no intentional hover treatment, while toolbar icons do. Keyboard feedback also varies between controls.

Add a shared accent hover edge to enabled buttons and accent keyboard focus to buttons, disclosure summaries and named editable fields. Disabled controls receive no enabled hover edge. Selected and typing state colors and every action remain unchanged.

## Evidence

All images use the isolated synthetic fixture of #1007; identifier text is omitted
after measurement. No installation data or configuration is used.

| Width | Before | After, light | After, dark |
| --- | --- | --- | --- |
| 1440 px | [Before](https://raw.githubusercontent.com/yousan/hataori/757f311/docs/notes/dashboard-1007/before/1440-light-en-question.png) | [After](after/1440-light-en-question.png) | [After](after/1440-dark-en-question.png) |
| 390 px | [Before](https://raw.githubusercontent.com/yousan/hataori/757f311/docs/notes/dashboard-1007/before/390-light-en-question.png) | [After](after/390-light-en-question.png) | [After](after/390-dark-en-question.png) |

### All after captures

- [1440-dark-en-composer](after/1440-dark-en-composer.png)
- [1440-dark-en-host-https](after/1440-dark-en-host-https.png)
- [1440-dark-en-host](after/1440-dark-en-host.png)
- [1440-dark-en-list](after/1440-dark-en-list.png)
- [1440-dark-en-passkeys](after/1440-dark-en-passkeys.png)
- [1440-dark-en-question-focus](after/1440-dark-en-question-focus.png)
- [1440-dark-en-question](after/1440-dark-en-question.png)
- [1440-dark-en-running-https](after/1440-dark-en-running-https.png)
- [1440-dark-en-stop](after/1440-dark-en-stop.png)
- [1440-dark-en-typeprompt-https](after/1440-dark-en-typeprompt-https.png)
- [1440-light-en-composer](after/1440-light-en-composer.png)
- [1440-light-en-host-https](after/1440-light-en-host-https.png)
- [1440-light-en-host](after/1440-light-en-host.png)
- [1440-light-en-list](after/1440-light-en-list.png)
- [1440-light-en-passkeys](after/1440-light-en-passkeys.png)
- [1440-light-en-question-focus](after/1440-light-en-question-focus.png)
- [1440-light-en-question](after/1440-light-en-question.png)
- [1440-light-en-running-https](after/1440-light-en-running-https.png)
- [1440-light-en-stop](after/1440-light-en-stop.png)
- [1440-light-en-typeprompt-https](after/1440-light-en-typeprompt-https.png)
- [390-dark-en-composer](after/390-dark-en-composer.png)
- [390-dark-en-host-https](after/390-dark-en-host-https.png)
- [390-dark-en-host](after/390-dark-en-host.png)
- [390-dark-en-list](after/390-dark-en-list.png)
- [390-dark-en-passkeys](after/390-dark-en-passkeys.png)
- [390-dark-en-question-focus](after/390-dark-en-question-focus.png)
- [390-dark-en-question](after/390-dark-en-question.png)
- [390-dark-en-running-https](after/390-dark-en-running-https.png)
- [390-dark-en-stop](after/390-dark-en-stop.png)
- [390-light-en-composer](after/390-light-en-composer.png)
- [390-light-en-host-https](after/390-light-en-host-https.png)
- [390-light-en-host](after/390-light-en-host.png)
- [390-light-en-list](after/390-light-en-list.png)
- [390-light-en-passkeys](after/390-light-en-passkeys.png)
- [390-light-en-question-focus](after/390-light-en-question-focus.png)
- [390-light-en-question](after/390-light-en-question.png)
- [390-light-en-running-https](after/390-light-en-running-https.png)
- [390-light-en-stop](after/390-light-en-stop.png)

## Wireframes rendered before source changes

Choose A: an accent edge acknowledges hover without repainting selected or typing backgrounds. B tints the whole background, competing with the existing selected-state fill. Neither changes geometry.

- [A, 1440-dark-en-question](wireframe-A/1440-dark-en-question.png)
- [A, 1440-light-en-question](wireframe-A/1440-light-en-question.png)
- [A, 390-dark-en-question](wireframe-A/390-dark-en-question.png)
- [A, 390-light-en-question](wireframe-A/390-light-en-question.png)
- [B, 1440-dark-en-question](wireframe-B/1440-dark-en-question.png)
- [B, 1440-light-en-question](wireframe-B/1440-light-en-question.png)
- [B, 390-dark-en-question](wireframe-B/390-dark-en-question.png)
- [B, 390-light-en-question](wireframe-B/390-light-en-question.png)

## Validation

Pinned Node install/check (173 tests)/build, gofmt, go vet and focused page tests pass. Real synthetic mouse hover and keyboard focus checks fail on the baseline for density/view/filter/footer actions. They pass after the fix in both widths/themes, including disabled Send and modal controls; selected colors remain unchanged. Hover and keyboard-focus captures are included. Fresh Codex review is recorded separately. No local tmux tests.

Fresh independent Codex review: no findings; HTTPS Type/Operate and the floating Type prompt receive additional real pointer coverage.

Concurrency: Open PRs #1047, #1042, #1039, #1035, #1032, #1029, #1028, #1027, #1026, #1013 share styles.css/generated page files; each finding uses a separate source hunk and starts from current main. Old #990 shares CSS but is merged; remote branches were checked before the source edit.
