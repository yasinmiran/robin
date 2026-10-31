import { defineConfig, devices } from "@playwright/test";

import { siteAddress } from "./scripts/site-url.ts";
import { joinBase } from "./src/lib/base.ts";

const port = Number(process.env.SITE_E2E_PORT ?? 4817);
// A build for SITE_URL is served, and so tested, under that address's base.
const home = `http://127.0.0.1:${port}${joinBase(siteAddress(process.env.SITE_URL).base, "/")}`;

// The smoke test drives the installed Google Chrome against the built site,
// so it needs `npm run build` first and downloads no browser of its own.
export default defineConfig({
  testDir: "tests/e2e",
  fullyParallel: true,
  forbidOnly: Boolean(process.env.CI),
  reporter: "list",
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    channel: "chrome",
    trace: "retain-on-failure",
  },
  projects: [{ name: "chrome", use: { ...devices["Desktop Chrome"], channel: "chrome" } }],
  webServer: {
    command: `npx astro preview --ignore-lock --host 127.0.0.1 --port ${port}`,
    url: home,
    reuseExistingServer: false,
    timeout: 60_000,
  },
});
