# Robin docs site

The documentation website for Robin, built to static HTML with Astro. React
islands carry the parts that need a script (search, the theme toggle, the
mobile sidebar, copy buttons), styled with Tailwind CSS and shadcn/ui on the
design tokens in `src/styles/tokens.css`. The variants those components
use are in `src/styles/shadcn.css`, so the shadcn CLI is no dependency: to
add a component, run it on demand with `npx shadcn@4.22.1 add <name>`.
Pages are MDX in
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
`.github/workflows/pages.yml`: it runs `npm ci --ignore-scripts` and
`npm run pages`, then pushes `dist/` with a `.nojekyll` to the `gh-pages`
branch as a single orphan commit, replacing what was there, so the branch
never grows. `npm run pages` builds and checks exactly what it publishes,
so run it before pushing a change here.

The build runs every npm package in the lockfile, so it gets a read-only
token, no stored credentials and no install scripts (none of the packages
needs one here). What it hands on is data. The publish job runs only from
`main`, in the `docs` environment, and only its push step sees the write
token. `scripts/publish-pages.sh` keeps git's directory outside the built
files, reads no git config but its own, runs no hooks and refuses a build
holding a `.git` anywhere, since one there could carry config that runs a
command while the token is in reach. `scripts/publish-pages.test.ts`
checks that, and `scripts/pages-workflow.test.ts` checks the workflow's
layout: no token in the build, the token in the last step alone.
`scripts/workflows.test.ts` holds every workflow, CI's too, to the rest:
actions pinned to a commit, a read-only default token, no
`pull_request_target`, no credentials kept by a checkout, no install
scripts under `npm ci` and no `${{ }}` inside a `run:` script.

The token can write the whole repository, not only `gh-pages`, so the
repository's settings carry the rest:

- Environments: `docs` with a deployment branch rule that allows `main`
  only. GitHub enforces it, so a branch's own copy of the workflow cannot
  publish by dropping the job's `if`.
- Rulesets: one on `main` and one on `refs/tags/*` that block force pushes
  and deletion and require a pull request with code owner review, with no
  bypass for GitHub Actions; optionally one on `gh-pages` that only
  GitHub Actions may update.
- Actions: default workflow permissions read-only, "Allow GitHub Actions
  to create and approve pull requests" off, and approval required for
  workflows from every outside contributor's fork.
- Dependabot: security updates on for the lockfile. `.github/dependabot.yml`
  proposes the site's npm updates and keeps the actions' commit pins
  current, each release a week old first, and `.github/CODEOWNERS` puts the
  workflows and the publish script under code owner review.
