import { defineConfig } from '@playwright/test';

// Runs inside the client container, where out/ is e2e/out/<scenario> on the
// host. run.sh picks the spec file; SCENARIO tells gui.spec.ts what to expect.
export default defineConfig({
  testDir: 'tests',
  outputDir: 'out/test-results',
  workers: 1,
  fullyParallel: false,
  retries: 0,
  timeout: 90_000,
  globalTimeout: 15 * 60_000,
  expect: { timeout: 10_000 },
  reporter: [
    ['list'],
    ['html', { outputFolder: 'out/report', open: 'never' }],
    ['json', { outputFile: 'out/results.json' }],
  ],
  use: {
    channel: 'chrome',
    headless: true,
    viewport: { width: 1280, height: 720 },
    launchOptions: { args: ['--autoplay-policy=no-user-gesture-required'] },
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
});
