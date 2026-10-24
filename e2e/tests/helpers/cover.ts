import type { Page } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';

// Chrome's coverage of the page for make cover, saved in the shape of V8's
// own takePreciseCoverage result, which internal/tools/pagecover reads for
// Node's unit tests as well. Only Chromium has page.coverage, and only
// run.sh with ROBIN_E2E_COVER set asks for it.
const DIR = process.env.ROBIN_E2E_COVER;

const measured = new WeakSet<Page>();

// startPageCoverage measures every script the page runs from now on,
// across navigations.
export async function startPageCoverage(page: Page) {
  if (!DIR || page.context().browser()?.browserType().name() !== 'chromium') return;
  await page.coverage.startJSCoverage({ resetOnNavigation: false });
  measured.add(page);
}

// savePageCoverage writes what startPageCoverage measured as
// <ROBIN_E2E_COVER>/page/<name>-*.json. It never fails a test: a page that
// is gone has nothing to hand in.
export async function savePageCoverage(page: Page | undefined, name: string) {
  if (!page || !measured.has(page)) return;
  measured.delete(page);
  try {
    const result = await page.coverage.stopJSCoverage();
    fs.writeFileSync(path.join(DIR!, 'page', `${name}-${process.pid}-${Date.now()}.json`), JSON.stringify({ result }));
  } catch (e) {
    console.warn(`page coverage of ${name} lost: ${e}`);
  }
}
