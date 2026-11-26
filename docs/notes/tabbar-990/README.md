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
