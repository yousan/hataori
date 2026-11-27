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

Pinned Node 24.22.1: npm ci --ignore-scripts, complete npm check with 173 tests serially (node --test --test-concurrency=1), npm run build; generated page rebuilt after main integration. gofmt, go vet (Go p=1) and focused TestThePage tests (-parallel=1) pass. Sixteen refreshed synthetic captures cover long/ordinary questions, start failure and desktop split layouts at 1440/390 px in both themes; all wrapping geometry checks pass. Baseline-failing evidence is preserved. No local tmux tests.

Fresh independent Codex review: landing_review_1017: fresh independent Codex review after current-main integration found no findings; only nine CSS insertions, phone 44 px controls preserved by selector specificity, no action or permission changes.

Concurrency: Resumed existing PR after rechecking open PRs and remote branches. Shared dashboard CSS/generated assets: #1054, #1050, #1047, #1042, #1039, #1035, #1032, #1013. Main fae3fd04 is integrated; previous findings stay on main and only this finding remains in this PR’s diff.
