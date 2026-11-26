# Phone list row spacing (#1015)

Give phone thread and terminal rows a minimum 44 px touch target. Desktop density
stays unchanged. Only row sizing/spacing changes; no new layout hierarchy.

Concurrency: PR #1026 shares CSS and generated assets but changes the phone filter,
not row sizing. No other active dashboard source change was found.
