# GitHub REST captures

Projected from real public API responses fetched on 2026-11-28. Projection removes unused prose/URLs and preserves parsed field values and structure.

- repository: repos/yousan/hataori
- run/jobs: actions/runs/46093553816/attempts/1 (and /jobs?per_page=100)
- pull: pulls/1068
- reviews: pulls/1107/reviews?per_page=100
- merge: git/commits/8e051963929541ad126f9fff3c6b9c32dc4e58b7
- merge-pulls: commits/8e051963929541ad126f9fff3c6b9c32dc4e58b7/pulls?per_page=100

The actual run is a partial draft pull_request run; the captured review is COMMENTED, not APPROVED. Tests derive explicitly mutated variants for positive/failure scenarios. Mutations and test signing keys are not real repository approval or activation evidence. No deployment data or private transcripts are included.

Additional projections:

- full-jobs: actions/runs/46089369497/attempts/1/jobs?per_page=100
  (actual corrected phase-one run: Linux success, macOS failure; required step
  names, runner assignment and labels retained)
- content: contents/.github/workflows/ci.yml?ref=33e45270d32e94b832f3ce2ea1ae55ca87ac1e9b
- ref: git/ref/heads/main (capture-time SHA only, not a live watermark)
- compare: compare/b9e6c4fcd4f43996a7340d1e36916bda3dada04f...8e051963929541ad126f9fff3c6b9c32dc4e58b7

Positive combined Linux/macOS/compatibility tests explicitly mutate these records
and add a test-only compatibility job. They do not claim that any captured draft
or failed run supplied a valid baseline or approved control.
