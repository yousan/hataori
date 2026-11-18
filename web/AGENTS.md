# The dashboard's page

The read-only dashboard's page (design, section 7.2): TypeScript and React, built with Vite. The page is in English and Japanese; choose with `?lang=ja` or `?lang=en`, and the choice is kept in this browser; without a choice, the page follows the browser's language.

- **The build is committed.** `npm run build` writes `internal/dashboard/page/`, which the `hataori` binary embeds, so `go build` needs no Node. Commit the build with the change to `web/` that made it; CI builds again and fails when the two differ.
- **Build with the pinned Node** (`.nvmrc`; `engines` refuses another) after `npm ci --ignore-scripts`. Every package is pinned to an exact version in `package.json` and in `package-lock.json`. Add one with `npm install --save-exact --ignore-scripts`, a release at least two weeks old.
- **Every user-visible string is in `src/catalog.ts`**, used as `t("key")`. `npm run check` fails on text, a visible attribute (`title`, `alt`, `aria-label`, …) or `document.title` written anywhere else, including `index.html`. It sees strings written in place and in variables of the same file, not strings that come from another module or a function, so take text only from `t()`. When it misses a case, add it to `scripts/testdata/` marked `want`.
- **The page loads only its own server's files.** No CDN, no web fonts, no inline script, no `data:` URLs: the server's Content-Security-Policy (`internal/dashboard/page.go`) refuses them. Its one exception: `<style>` elements may be inline, because xterm.js writes its own at run time.
- `go.mod` here holds no code: it keeps the `go` command out of `node_modules`.

Before pushing a change here, in `web/`:

```sh
npm ci --ignore-scripts
npm run check
npm run build        # then commit internal/dashboard/page
```
