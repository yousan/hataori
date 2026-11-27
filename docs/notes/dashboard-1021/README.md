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

Pinned Node 24.22.1: npm ci --ignore-scripts, complete npm check (173 tests) with Node test-concurrency=1, npm run build; generated page rebuilt after main integration. gofmt, go vet (Go p=1) and focused TestThePage tests (-parallel=1) pass. Eight refreshed synthetic composer/question captures at1440/390px in both themes verify phone textarea16px and desktop13px. Baseline-failing evidence is retained; no mobile zoom measurement is claimed. No local tmux tests.

Fresh independent Codex review: landing_review_1021: fresh independent Codex source review after current-main integration found no actionable findings; only five CSS lines, narrow textareas16px/desktop13px, no input/permission changes, prior fixes preserved.

Concurrency: Resumed existing PR after rechecking open PRs and remote branches. Shared dashboard CSS/generated assets: #1054, #1050, #1047, #1013. Main 7bf1dbf6 is integrated; previous findings stay on main and only this finding remains in this PR’s diff.
