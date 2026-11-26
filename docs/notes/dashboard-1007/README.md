# Dashboard design pass (#1007)

The built app was reviewed on an isolated HTTP test server with fictional data at
1440 × 900 and 390 × 844 CSS pixels in light and dark themes. Japanese stress
cases and an 800 px tablet width are included. No installation data or configuration
was used. Identifier text is omitted only in documentation captures, after measurement.

[Every view and panel, both widths and themes](gallery.md). The gallery includes
all eight thread states, waiting questions/approvals, startup failure, terminal and
conversation, conversation tool details, composer, lower details/events, empty and
signed-out screens, host screen/details, unreadable console, passkeys/trusted-files
dialogs, filtering, split panes, the tab menu and hidden panels.

## Ten findings, in priority order

| Priority | Issue | Source | Finding |
| --- | --- | --- | --- |
| 1 | [#1014](https://github.com/yousan/hataori/issues/1014) | `web/src/styles.css:765` | At 390 px in Japanese, Active/All shrink to one character per line when the hidden-shell summary is present. The filter bar grows to about 97 px. |
| 2 | [#1015](https://github.com/yousan/hataori/issues/1015) | `web/src/styles.css:1984` | Ordinary phone thread rows measure about 24 px, while attention rows measure about 41 px. The later tree box rule overrides the phone row padding. |
| 3 | [#1016](https://github.com/yousan/hataori/issues/1016) | `web/src/styles.css:1068` | At 390 px, view buttons measure about 23 px, Back about 28 px, list summaries about 33 px, and footer actions about 19 px high. |
| 4 | [#1017](https://github.com/yousan/hataori/issues/1017) | `web/src/styles.css:1369` | An unbroken fictional question extends past the desktop details panel. The answer prompt also extends past its pane at both widths. |
| 5 | [#1018](https://github.com/yousan/hataori/issues/1018) | `web/src/styles.css:612` | A long fictional trusted-file name extends outside the modal content area and is clipped at both desktop and phone widths. |
| 6 | [#1019](https://github.com/yousan/hataori/issues/1019) | `web/src/styles.css:1251` | In the 800 px table, titles shrink to only a few characters while agent, branch and last-output columns retain 14ch minimum widths. |
| 7 | [#1020](https://github.com/yousan/hataori/issues/1020) | `web/src/styles.css:158` | At 800 px, fixed side panels leave a 180 px center. Tabs vanish, the theme tool is clipped, the running title uses five lines and footer actions run off-screen. |
| 8 | [#1021](https://github.com/yousan/hataori/issues/1021) | `web/src/styles.css:1689` | Phone message and answer fields inherit the 13 px desktop body font, making the editable text smaller than typical phone forms. |
| 9 | [#1022](https://github.com/yousan/hataori/issues/1022) | `web/src/styles.css:1701` | Send, Answer and approval buttons only inherit their font. They use browser-default borders and fills beside explicitly styled Type/Stop/view controls. |
| 10 | [#1023](https://github.com/yousan/hataori/issues/1023) | `web/src/styles.css:190` | Density, Active/All, view choices and outlined actions have no intentional hover treatment, while toolbar icons do. Keyboard feedback also varies between controls. |

The baseline is PR #992: keep its 34 px desktop tab strip and existing actions.
For each finding, use one issue/branch/PR and before/after captures at both widths.
Render two variants before layout or visual hierarchy changes; spacing-only changes
need measurements and captures. API and action behavior are outside this pass.

## Reproduction

The capture runner serves only the committed dashboard build and synthetic API
responses; all browser requests to other origins are refused. It starts neither a
daemon nor an agent, and uses a temporary browser profile. Install playwright-core
1.60.0 in a separate temporary tools directory and pass its entry module through
`HATAORI_PLAYWRIGHT`; point `HATAORI_TEST_CHROME` at a Chrome executable. Run
`capture.mjs` with Node 24.22.1. `CAPTURE_OUTPUT`, `CAPTURE_VIEWS`, `CAPTURE_WIDTHS`,
`CAPTURE_LANGS` and `CAPTURE_THEMES` select the artifact directory and matrix.
`CAPTURE_CSS` renders CSS layout concepts without changing the app sources.

## Review and concurrency

A fresh independent Codex source and image review confirmed the ten findings.
Fifteen representative images were also checked for private information. The hover
finding is confirmed from CSS; static captures alone do not prove pointer feedback.
The textarea recommendation concerns readability; no iOS zoom measurement is claimed.

No open PR changes the dashboard sources, browser checks or generated page at start.
Old #724/#990 branches share conversation/catalog and tab-bar/CSS files; both PRs
have merged. Each issue rechecks open PRs and remote branches before source edits.

Reference: [Web Interface Guidelines](https://github.com/vercel-labs/web-interface-guidelines/blob/main/command.md).
The findings above are based on this app’s source and measured synthetic captures.
