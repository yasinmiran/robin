import { test, expect, type BrowserContext, type CDPSession, type Page } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';
import { Cli, sshProcesses } from './helpers/cli';
import { pagePoint } from './helpers/coords';
import { savePageCoverage, startPageCoverage } from './helpers/cover';
import { CHANGED_FLOOR, sampleFrame, waitForStillFrame } from './helpers/frames';
import * as host from './helpers/host';

// The touch scenario: one session in Chrome emulating a touch screen
// (Playwright's isMobile and hasTouch), against the X11 host. Fingers go in
// through the DevTools protocol, as a phone's would; what they do is
// checked on the host.

const OUT = path.resolve('out');
const SCREEN = '#screen';

let cli: Cli | undefined;
let context: BrowserContext;
let page: Page;
let cdp: CDPSession;

type Zoom = { scale: number; x: number; y: number };
const zoom = () => page.evaluate(() => (window as any).robin.zoom as Zoom);

// touch sends one touch event with the given fingers, in page pixels.
function touch(type: string, points: [number, number][]) {
  return cdp.send('Input.dispatchTouchEvent', { type, touchPoints: points.map(([x, y], id) => ({ x, y, id })) });
}

async function tap(x: number, y: number) {
  await touch('touchStart', [[x, y]]);
  await touch('touchEnd', []);
}

// pinch spreads two fingers about the viewport's centre from one distance
// apart to another.
async function pinch(from: number, to: number) {
  const vp = page.viewportSize()!;
  const cx = vp.width / 2;
  const cy = vp.height / 2;
  const steps = 8;
  await touch('touchStart', [[cx - from / 2, cy], [cx + from / 2, cy]]);
  for (let i = 1; i <= steps; i++) {
    const d = (from + ((to - from) * i) / steps) / 2;
    await touch('touchMove', [[cx - d, cy], [cx + d, cy]]);
  }
  await touch('touchEnd', []);
}

// pointerNear waits for the X pointer to sit within 2 px of want.
async function pointerNear(want: { x: number; y: number }) {
  await expect
    .poll(
      async () => {
        const got = await host.mouseLocation();
        return Math.max(Math.abs(got.x - want.x), Math.abs(got.y - want.y));
      },
      { message: `xdotool getmouselocation vs ${want.x},${want.y}`, timeout: 5_000 },
    )
    .toBeLessThanOrEqual(2);
}

async function terminal(): Promise<host.Rect> {
  let term: host.Rect | null = null;
  await expect.poll(async () => (term = await host.terminal()), { message: 'an xterm is mapped', timeout: 15_000 }).not.toBeNull();
  return term!;
}

test.beforeAll(async ({ browser }, testInfo) => {
  expect(process.env.SCENARIO, 'SCENARIO').toBe('touch');
  testInfo.setTimeout(150_000);
  context = await browser.newContext({
    viewport: { width: 1024, height: 640 },
    isMobile: true,
    hasTouch: true,
    recordVideo: { dir: path.join(OUT, 'video'), size: { width: 1024, height: 640 } },
  });
  page = await context.newPage();
  await startPageCoverage(page);
  const consoleLog = fs.createWriteStream(path.join(OUT, 'console.log'), { flags: 'a' });
  page.on('console', (m) => consoleLog.write(`${new Date().toISOString()} ${m.type()} ${m.text()}\n`));
  page.on('pageerror', (e) => consoleLog.write(`${new Date().toISOString()} pageerror ${e.stack ?? e}\n`));
  cli = await Cli.start(OUT);
  await page.goto(cli.url);
  await expect.poll(() => page.evaluate(() => (window as any).robin?.state), { message: 'connected', timeout: 30_000 }).toBe('connected');
  await expect.poll(() => page.evaluate(() => (window as any).robin.video.videoWidth), { message: 'a video frame', timeout: 20_000 }).toBeGreaterThan(0);
  cdp = await context.newCDPSession(page);
});

test.afterEach(async ({}, testInfo) => {
  if (testInfo.status === testInfo.expectedStatus || !page) return;
  if (cli) await testInfo.attach('cli stderr (tail)', { body: cli.stderrTail(), contentType: 'text/plain' });
  await testInfo.attach('host', { body: await host.diagnostics().catch(String), contentType: 'text/plain' });
});

test.afterAll(async () => {
  await cli?.stop();
  await savePageCoverage(page, 'touch');
  await context?.close();
});

test('touch: the page offers the keyboard button and keeps the status tab out', async () => {
  await expect(page.locator('#keyboard')).toBeVisible();
  expect(await page.evaluate(() => document.activeElement?.id), 'the soft keyboard stays down until asked for').not.toBe('typing');
  await page.waitForTimeout(3_000);
  expect(await page.evaluate(() => document.getElementById('hud')!.classList.contains('tucked')), 'the status tab tucked away').toBe(false);
});

test('touch: a tap puts the remote pointer where the finger is, within 2 px', async () => {
  const { width, height } = await host.displayGeometry();
  const want = { x: Math.round(width * 0.3), y: Math.round(height * 0.6) };
  const p = await pagePoint(page, SCREEN, want.x, want.y);
  await tap(p.x, p.y);
  await pointerNear(want);
});

test('touch: pinching zooms the picture about the fingers, and a tap lands where the zoomed picture shows', async () => {
  const before = await sampleFrame(page, SCREEN);
  await pinch(200, 400);
  const z = await zoom();
  expect(z.scale, 'zoom after spreading two fingers from 200 to 400 px').toBeCloseTo(2, 1);
  expect(await sampleFrame(page, SCREEN), 'the zoom is local: the remote picture keeps its size')
    .toMatchObject({ width: before.width, height: before.height });

  // pagePoint reads the video's box as laid out, zoom included.
  const { width, height } = await host.displayGeometry();
  const want = { x: Math.round(width / 2) + 40, y: Math.round(height / 2) - 30 };
  const p = await pagePoint(page, SCREEN, want.x, want.y);
  const vp = page.viewportSize()!;
  expect(p.x > 0 && p.y > 0 && p.x < vp.width && p.y < vp.height, `${JSON.stringify(p)} is on the page`).toBe(true);
  await tap(p.x, p.y);
  await pointerNear(want);

  await pinch(400, 100);
  expect((await zoom()).scale, 'pinching in goes back to the whole picture').toBe(1);
});

test('touch: two fingers dragged down scroll the xterm back', async () => {
  const t = await terminal();
  const centre = await pagePoint(page, SCREEN, t.x + t.width / 2, t.y + t.height / 2);
  await tap(centre.x, centre.y);
  // Something to scroll back to: more lines than the xterm shows.
  await page.locator('#keyboard').tap();
  await cdp.send('Input.insertText', { text: 'seq 200' });
  await page.keyboard.press('Enter');
  await page.locator('#keyboard').tap();
  await waitForStillFrame(page, SCREEN);
  await sampleFrame(page, SCREEN, { save: 'before-scroll' });

  const [x0, x1] = [centre.x - 40, centre.x + 40];
  await touch('touchStart', [[x0, centre.y - 80], [x1, centre.y - 80]]);
  for (let dy = -60; dy <= 120; dy += 20) await touch('touchMove', [[x0, centre.y + dy], [x1, centre.y + dy]]);
  await touch('touchEnd', []);
  expect((await zoom()).scale, 'scrolling leaves the zoom alone').toBe(1);
  await expect
    .poll(async () => (await sampleFrame(page, SCREEN, { compare: 'before-scroll', region: t })).changed, {
      message: 'pixels changed in the xterm as it scrolled back',
      timeout: 5_000,
    })
    .toBeGreaterThan(CHANGED_FLOOR);
});

test('touch: the keyboard button raises the soft keyboard, and what it types runs on the host', async () => {
  const file = '/tmp/robin-e2e-touch';
  await host.ssh(`rm -f ${file}`);
  const t = await terminal();
  const centre = await pagePoint(page, SCREEN, t.x + t.width / 2, t.y + t.height / 2);
  await tap(centre.x, centre.y);

  await page.locator('#keyboard').tap();
  expect(await page.evaluate(() => document.activeElement?.id), 'the text box has the focus').toBe('typing');
  await expect(page.locator('#keyboard')).toHaveAttribute('aria-pressed', 'true');
  // A soft keyboard types letters it names no key for, and composes words.
  for (const ch of 'echo ') await cdp.send('Input.insertText', { text: ch });
  await cdp.send('Input.imeSetComposition', { text: 'touc', selectionStart: 4, selectionEnd: 4 });
  await cdp.send('Input.insertText', { text: 'touch' });
  await cdp.send('Input.insertText', { text: `-ü > ${file}` });
  await page.keyboard.press('Enter');
  await expect
    .poll(async () => (await host.sshRaw(`cat ${file}`)).stdout.trim(), { message: `${file} on the host`, timeout: 10_000 })
    .toBe('touch-ü');

  await page.locator('#keyboard').tap();
  await expect(page.locator('#keyboard')).toHaveAttribute('aria-pressed', 'false');
});

test('teardown: SIGINT ends the CLI and leaves nothing behind', async ({}, testInfo) => {
  const dir = await host.runDir();
  const stop = await cli!.stop(10_000);
  testInfo.annotations.push({ type: 'exit', description: JSON.stringify(stop) });
  expect(stop.killed, 'CLI exited by itself within 10 s of SIGINT').toBe(false);
  expect(stop.code, 'CLI exit code').toBe(0);
  await expect.poll(host.leftovers, { message: 'session processes on the host', timeout: 10_000 }).toBe('');
  await expect.poll(() => host.exists(dir), { message: `run dir ${dir} exists`, timeout: 1_000 }).toBe(false);
  await expect.poll(sshProcesses, { message: "the CLI's ssh processes on the client", timeout: 5_000 }).toEqual([]);
});
