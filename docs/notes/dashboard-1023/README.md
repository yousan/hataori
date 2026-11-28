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

Pinned Node24.22.1: npm ci --ignore-scripts, complete npm check (173 tests) with Node test-concurrency=1, npm run build; generated page rebuilt after main integration. gofmt, go vet (Go p=1) and focused TestThePage tests (-parallel=1) pass. Thirty-eight refreshed synthetic captures cover24HTTP hover,4keyboard focus and10HTTPS Type/Operate/host cases at1440/390px in both themes. Actual pointer/keyboard checks verify hover, focus, disabled exclusion and preserved state colors; disposable synthetic certificates only. Baseline-failing evidence and prior concepts retained. No local tmux tests.

Fresh independent Codex review: landing_review_1023: fresh independent Codex source review after current-main integration found no actionable findings; ten CSS lines only, enabled hover and focus specificity preserve selected/danger/Type/Operate colors and disabled behavior, prior fixes and all input/permission semantics unchanged.

Concurrency: Resumed existing PR after rechecking open PRs and remote branches. Shared dashboard CSS/generated assets: #1054, #1013. Main 8d3aa556 is integrated; previous findings stay on main and only this finding remains in this PR’s diff.
