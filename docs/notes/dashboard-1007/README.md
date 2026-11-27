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

The design reference is PR #992: keep its 34 px desktop tab strip and existing actions.
The dashboard baseline is commit `56d7d853`; subsequent main changes through
`562e241` did not change the dashboard sources or build.
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

## Fix pull requests and completion gate

Each finding was implemented in its own branch and PR, with before/after captures
at both widths, a regression check that fails on the baseline, and a fresh Codex
review. These links track the live merge/CI state; a prepared implementation is not
a completed issue. Require green CI on the exact head, including macOS, then merge,
confirm issue closure, and require green CI on the exact main merge commit.

| Finding | Pull request |
| --- | --- |
| [#1014](https://github.com/yousan/hataori/issues/1014) | [#1026](https://github.com/yousan/hataori/pull/1026) |
| [#1015](https://github.com/yousan/hataori/issues/1015) | [#1027](https://github.com/yousan/hataori/pull/1027) |
| [#1016](https://github.com/yousan/hataori/issues/1016) | [#1028](https://github.com/yousan/hataori/pull/1028) |
| [#1017](https://github.com/yousan/hataori/issues/1017) | [#1029](https://github.com/yousan/hataori/pull/1029) |
| [#1018](https://github.com/yousan/hataori/issues/1018) | [#1032](https://github.com/yousan/hataori/pull/1032) |
| [#1019](https://github.com/yousan/hataori/issues/1019) | [#1035](https://github.com/yousan/hataori/pull/1035) |
| [#1020](https://github.com/yousan/hataori/issues/1020) | [#1039](https://github.com/yousan/hataori/pull/1039) |
| [#1021](https://github.com/yousan/hataori/issues/1021) | [#1042](https://github.com/yousan/hataori/pull/1042) |
| [#1022](https://github.com/yousan/hataori/issues/1022) | [#1047](https://github.com/yousan/hataori/pull/1047) |
| [#1023](https://github.com/yousan/hataori/issues/1023) | [#1050](https://github.com/yousan/hataori/pull/1050) |

## Supplemental listener and integration checks

The [HTTPS gallery](https-gallery.md) adds the web listener's Type/Operate controls,
the floating Type prompt, and the richer passkey dialog. It uses a fresh test-only
certificate, not any installation certificate. The phone has no terminal Type
prompt in the existing app; its unchanged layout is captured separately.

The [combined preview](integration-gallery.md) applies all ten source patches to an
isolated checkout of main and rebuilds it. This is local compatibility evidence,
not a merged branch or a replacement for each PR's head CI. Sixty English captures
cover 1440/800/390 px in both themes; six Japanese phone captures cover filter,
list and question. The browser checks confirm 44 px phone targets, 16 px phone and
13 px desktop editable text, title/center width, wrapping, button styling,
reachable desktop tools, and actual pointer/keyboard feedback. All checks pass.
A separate fresh Codex review of the combined source found no actionable issue.

Combining #1017 and #1022 needs one source conflict resolved: keep the shared
button styling from #1022 and the `.answers-row button, .answers-check` containment
rule from #1017. Generated assets must be rebuilt after each main integration.
The other source patches combined cleanly. Do not carry the combined preview into
one finding's PR.

The runner now supports `CAPTURE_BUILD_ROOT`, `VERIFY_CASE`, `CAPTURE_INTERACTION`
and `CAPTURE_SUFFIX` for focused regression evidence. `CAPTURE_HTTPS_DIR` may point
to a newly generated temporary directory containing only a disposable `key.pem`
and `cert.pem`. It never needs an installation config, socket or data directory.
Use `VERIFY_CASE=integration` only against the rebuilt combined source, or the
individual finding keys against each PR's rebuilt source. The ephemeral test
server and browser are closed by the runner itself.

Final independent documentation/image review found and corrected an HTTP-only
runner timeout: the HTTPS Type prompt is now skipped on HTTP. The focused HTTP
reproduction check passes; follow-up review confirmed the correction. No remaining
findings or private content were reported.
