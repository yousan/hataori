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

Pinned Node 24.22.1: npm ci --ignore-scripts, complete npm check (173 tests) with Node test-concurrency=1, npm run build; generated page rebuilt after main integration and CSS conflict resolution. gofmt, go vet (Go p=1) and focused TestThePage tests (-parallel=1) pass. Sixteen refreshed synthetic composer/question/approval/long-question captures at1440/390px in both themes verify button radius/minimum height and retained phone sizing. Main question containment remains. Baseline-failing evidence is retained. No local tmux tests.

Fresh independent Codex review: landing_review_1022: fresh independent Codex review after current-main CSS conflict resolution found no findings; shared theme/disabled/focus styles only, main answer containment retained, phone44px controls and16px fields preserved, no permission/input changes.

Concurrency: Resumed existing PR after rechecking open PRs and remote branches. Shared dashboard CSS/generated assets: #1054, #1050, #1013. Main 27ff3b0d is integrated; previous findings stay on main and only this finding remains in this PR’s diff.
