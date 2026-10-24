import { defineConfig } from '@playwright/test';
import { fakeMediaArgs } from './tests/helpers/fakemedia';

// Runs inside the client container, where out/ is e2e/out/<scenario> on the
// host. run.sh picks the spec file and the project (the browser); SCENARIO
// tells gui.spec.ts what to expect.
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
    headless: true,
    viewport: { width: 1280, height: 720 },
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    {
      name: 'chrome',
      use: { channel: 'chrome', launchOptions: { args: ['--autoplay-policy=no-user-gesture-required', ...fakeMediaArgs()] } },
    },
    // Playwright's WebKit runs Safari's WebRTC stack (libwebrtc) on Linux.
    { name: 'webkit', use: { browserName: 'webkit' } },
    {
      name: 'firefox',
      use: {
        browserName: 'firefox',
        launchOptions: {
          // The OpenH264 plugin the client image installs under MOZ_GMP_PATH,
          // set up the way distributions ship it: enabled, never updated.
          firefoxUserPrefs: {
            'media.gmp-gmpopenh264.enabled': true,
            'media.gmp-gmpopenh264.provider.enabled': true,
            'media.gmp-gmpopenh264.autoupdate': false,
            'media.gmp-gmpopenh264.visible': true,
            'media.gmp-gmpopenh264.version': 'system-installed',
            'media.gmp-manager.updateEnabled': false,
            'media.peerconnection.video.h264_enabled': true,
            'media.autoplay.default': 0,
          },
        },
      },
    },
  ],
});
