# Compact tab bar (#990)

Three wireframes at 1280 CSS pixels, in Japanese and both themes:

- A: persistent tab-list chevron; a filter icon and muted hidden-thread count.
- B: compact text filter; other tools use icons.
- C: underlined tabs; the tab-list chevron joins the tool group.

Choose A. It leaves the most room for titles while keeping the tab list in a
stable place directly after the tabs. The chevron is always present so opening
another tab or resizing does not move the other tools. B spends title space on
text already available in tooltips; C mixes tab navigation with view settings.
The active tab uses the pane background, matching the existing app.

All titles and counts in these wireframes and the test screenshots are made up.
Icons are hand-drawn for this change, using currentColor and a 1.5-unit stroke.

## Browser measurements

CSS pixels, with the tree and details open, eight fictional thread tabs
(including running, waiting and failed threads), and 57 hidden threads.
Both themes give the same heights. Screenshots use the built app served by
an isolated test HTTP server with synthetic API responses, in headless Chromium
on Linux. No installation's data, configuration or titles are used.

| Window | Language | Before | After |
| --- | --- | ---: | ---: |
| 1280 | Japanese | 74.78 | 34 |
| 1280 | English | 59.38 | 34 |
| 1920 | Japanese | 46.59 | 34 |
| 1920 | English | 31.19 | 34 |

The tab strip has hidden scrollbars and retains native horizontal scrolling.
The tab-list button is always visible. At 1280 px the tabs retain 446 px next
to the tools (1086 px at 1920 px), with both side panels open. Time, badges
and input markers cannot shrink or wrap; only titles truncate. The phone
list keeps its existing text controls.

## Validation

- TypeScript, catalog lint and all 173 existing web tests pass; the pinned
  Node build is committed with the sources.
- Focused browser tests cover the 1024/1280/1920 px strip, both languages and
  themes, every overflowed tab via real pointer events, menu keys and dismissal,
  existing tab keys and resize/reveal behavior, filtering/counts and phone layout.
- The new compact-strip regression fails against the original built page:
  at 1024 px it measures 89.78 px, finds wrapping time/badge text and text tools.
- Pointer checks confirm close controls appear on the active/hovered tab and
  the phone retains two text filter choices. Page-serving and same-origin
  resource checks pass. Full Linux/macOS suites run in CI.
- Fresh independent Codex review found no functional issues. Its screenshot
  review caught a synthetic thread number behind the tab menu; the menu image
  excludes the background pane before capture.

The menu also consumes a focus request immediately when re-selecting the
active tab; a real-pointer regression then selects a tree row and checks that
focus stays in the tree. Fresh Codex review confirmed this follow-up fix.
