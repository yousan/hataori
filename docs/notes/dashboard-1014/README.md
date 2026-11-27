# Dashboard: phone filter labels collapse into vertical text (#1014)

At 390 px in Japanese, Active/All shrink to one character per line when the hidden-shell summary is present. The filter bar grows to about 97 px.

Keep the two segmented buttons at their intrinsic widths and group the secondary counts in a summary that can wrap. Desktop filtering and every action remain unchanged.

## Evidence

All images use the isolated synthetic fixture of #1007; identifier text is omitted
after measurement. No installation data or configuration is used.

| Width | Before | After, light | After, dark |
| --- | --- | --- | --- |
| 1440 px | [Before](https://raw.githubusercontent.com/yousan/hataori/757f311/docs/notes/dashboard-1007/japanese/1440-light-ja-filter.png) | [After](after/1440-light-ja-filter.png) | [After](after/1440-dark-ja-filter.png) |
| 390 px | [Before](https://raw.githubusercontent.com/yousan/hataori/757f311/docs/notes/dashboard-1007/japanese/390-light-ja-filter.png) | [After](after/390-light-ja-filter.png) | [After](after/390-dark-ja-filter.png) |

### All after captures

- [1440-dark-en-filter](after/1440-dark-en-filter.png)
- [1440-dark-ja-filter](after/1440-dark-ja-filter.png)
- [1440-light-en-filter](after/1440-light-en-filter.png)
- [1440-light-ja-filter](after/1440-light-ja-filter.png)
- [390-dark-en-filter](after/390-dark-en-filter.png)
- [390-dark-ja-filter](after/390-dark-ja-filter.png)
- [390-light-en-filter](after/390-light-en-filter.png)
- [390-light-ja-filter](after/390-light-ja-filter.png)

## Wireframes rendered before source changes

Choose A: natural wrapping uses only the space the summary needs and keeps the existing compact segmented control. B reserves a separate grid row for the summary even when the whole summary could fit beside the buttons.

- [A, 390-dark-ja-filter](wireframe-A/390-dark-ja-filter.png)
- [A, 390-light-ja-filter](wireframe-A/390-light-ja-filter.png)
- [B, 390-dark-ja-filter](wireframe-B/390-dark-ja-filter.png)
- [B, 390-light-ja-filter](wireframe-B/390-light-ja-filter.png)

## Validation

Pinned Node 24.22.1: npm ci --ignore-scripts, npm run check (173 tests), npm run build; generated page committed. The synthetic label-line regression fails on the baseline (4.82 and 2.82 line heights) and passes with this fix (0.82 each) in both languages/themes. Both widths captured. gofmt, go vet and focused page-serving/same-origin tests pass. No local tmux test.

Fresh independent Codex review: no findings.

Concurrency: No other open PR changes these sources at start. Old #990 shares dashboard CSS but its PR has merged. The audit PR changes documentation only.
