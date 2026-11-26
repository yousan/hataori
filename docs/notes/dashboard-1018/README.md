# Dashboard: long trusted-file names are clipped in the dialog (#1018)

A long fictional trusted-file name extends outside the modal content area and is clipped at both desktop and phone widths.

Reset the trusted-files dialog’s inherited desktop footer nowrap, then allow file/repository and attribution spans to shrink and wrap. All text is retained and modal actions/data stay unchanged.

## Evidence

All images use the isolated synthetic fixture of #1007; identifier text is omitted
after measurement. No installation data or configuration is used.

| Width | Before | After, light | After, dark |
| --- | --- | --- | --- |
| 1440 px | [Before](https://raw.githubusercontent.com/yousan/hataori/757f311/docs/notes/dashboard-1007/before/1440-light-en-long-trusts.png) | [After](after/1440-light-en-long-trusts.png) | [After](after/1440-dark-en-long-trusts.png) |
| 390 px | [Before](https://raw.githubusercontent.com/yousan/hataori/757f311/docs/notes/dashboard-1007/before/390-light-en-long-trusts.png) | [After](after/390-light-en-long-trusts.png) | [After](after/390-dark-en-long-trusts.png) |

### All after captures

- [1440-dark-en-long-trusts](after/1440-dark-en-long-trusts.png)
- [1440-dark-en-trusts](after/1440-dark-en-trusts.png)
- [1440-light-en-long-trusts](after/1440-light-en-long-trusts.png)
- [1440-light-en-trusts](after/1440-light-en-trusts.png)
- [390-dark-en-long-trusts](after/390-dark-en-long-trusts.png)
- [390-dark-en-trusts](after/390-dark-en-trusts.png)
- [390-light-en-long-trusts](after/390-light-en-long-trusts.png)
- [390-light-en-trusts](after/390-light-en-trusts.png)

Only sizing/spacing or overflow containment changes; no new layout structure or hierarchy.

## Validation

Pinned Node install/check (173 tests)/build, gofmt, go vet and focused page tests pass. Baseline modal content measured 1,966 px inside a 510 px desktop dialog and 1,863 px inside a 356 px phone dialog. The fixed dialog contains its text at both widths/themes. Normal and long-name captures are included. No local tmux tests.

Fresh independent Codex review: no findings after an independent follow-up reviewed the desktop nowrap inheritance correction; all eight after images and measurements were regenerated.

Concurrency: Open PRs #1028, #1027, #1026, #1013 share styles.css/generated page files; each finding uses a separate source hunk and starts from current main. Old #990 shares CSS but is merged; remote branches were checked before the source edit.
