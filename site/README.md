# Robin docs site

The documentation website for Robin, built to static HTML with Astro. React
islands carry the parts that need a script (search, the theme toggle, the
mobile sidebar, copy buttons), styled with Tailwind CSS and shadcn/ui on the
design tokens in `src/styles/tokens.css`. Pages are MDX in
`src/content/docs`, one file per page; the `section` and `order` in each
file's frontmatter place it in the sidebar.

It needs Node 22.23.4 or newer (`.nvmrc` says 24) and npm. The Go build does
not touch it.

## Develop

```sh
npm ci --ignore-scripts
npm run dev        # http://localhost:4321, reloads on save
```

Search has no index under `npm run dev`, so the dialog says it could not
load one. Build and preview to try it.

## Build

```sh
npm run build      # astro build, then Pagefind indexes dist/
npm run preview    # serves dist/
```

`dist/` is the whole site: upload it to any static host that serves
`/install` from `install.html`. Set `SITE_URL` when building to say where it
will be served: it adds canonical and `og:url` tags, and its path becomes the
base every page, asset and search result sits under.
`SITE_URL=https://robin.example` serves from the root;
`SITE_URL=https://yasinmiran.github.io/robin` serves from `/robin/`, and
`npm run preview` then serves it there too. Links written in Markdown,
`[Sound](/sound)`, are moved under the base when the page is built; code
that writes a link puts it under the base with `withBase` or `docHref` from
`src/lib`. Under a base the dev server has the homepage at `/robin`, without
the slash.

## Test

```sh
npm test                # Vitest: components and helpers, in happy-dom
npm run test:coverage   # the same, failing under 85% of statements
npm run check           # astro check, the build, and the link checker
npm run test:e2e        # Playwright against dist/, in your installed Chrome
npm run check:all       # coverage, check and test:e2e, in that order
npm run pages           # check:all on the build for GitHub Pages
```

Coverage counts the code written here: `src/lib`, the components in
`src/components`, the docs table in `src/components/ui/table.tsx` and
`scripts/`. The other shadcn components are kept as the CLI generated them
and are left out. The link checker (`scripts/check-links.ts`) reads every
built page and fails on a link to a page, file or `#anchor` that does not
exist, or that leaves the base. Both it and the smoke test read `SITE_URL`
the way the build does, so give them the same one.

`npm run test:e2e` needs `npm run build` first and Google Chrome installed,
since it uses Playwright's `chrome` channel instead of downloading a browser.
It serves on port 4817; set `SITE_E2E_PORT` if that one is taken.

## Publish

The docs live at <https://yasinmiran.github.io/robin/>. A push to `main`
that touches `site/` (or a manual run) starts
`.github/workflows/pages.yml`: it runs `npm ci` and `npm run pages`, then
pushes `dist/` with a `.nojekyll` to the `gh-pages` branch as a single
orphan commit, replacing what was there, so the branch never grows. The
build job has a read-only token and no stored credentials; only the publish
job, which runs git and nothing from npm, can write. `npm run pages` builds
and checks exactly what it publishes, so run it before pushing a change
here.
