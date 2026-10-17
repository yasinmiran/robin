import { test, expect, type BrowserContext, type Page } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';
import { Cli } from './helpers/cli';
import { pagePoint } from './helpers/coords';
import { isNonBlack, sampleFrame, waitForStillFrame } from './helpers/frames';
import * as wl from './helpers/wayland';

// The wayland scenario: `remotejump gui --attach auto` against a host whose
// user is logged in to a headless GNOME Wayland session, with a fullscreen
// probe window that logs the input it receives. The portal asks before
// anything is shared; the harness answers as the person at the screen would.
// One session for the tests below, in order, then a second run that must
// restore without asking.

const OUT = path.resolve('out');
const STDERR = path.join(OUT, 'cli.stderr.log');
const SCREEN = '#screen';
const MONITOR = { width: 1280, height: 720 };
const CONSENT = 'approve the screen sharing request';

let cli: Cli | undefined;
let context: BrowserContext;
let page: Page;

const stderrText = () => {
  try {
    return fs.readFileSync(STDERR, 'utf8');
  } catch {
    return '';
  }
};
// lastRun is the CLI's stderr since the newest run started.
const lastRun = () => {
  const s = stderrText();
  return s.slice(s.lastIndexOf('\n--- ') + 1);
};
const stats = (p: Page) => p.evaluate(() => (window as any).remotejump.stats() as Promise<{ framesDecoded: number }>);

async function open(url: string) {
  await page.goto(url);
  await expect
    .poll(() => page.evaluate(() => typeof (window as any).remotejump), { message: 'window.remotejump exists', timeout: 15_000 })
    .toBe('object');
  await expect
    .poll(async () => isNonBlack(await sampleFrame(page, SCREEN)), { message: 'a non-black frame', timeout: 20_000, intervals: [250] })
    .toBe(true);
}

// eventsSince returns what the probe window logged after the first n events.
async function eventsSince(n: number) {
  return (await wl.events()).slice(n);
}

test.beforeAll(async ({ browser }, testInfo) => {
  testInfo.setTimeout(180_000);
  context = await browser.newContext({
    viewport: { width: 1280, height: 720 },
    recordVideo: { dir: path.join(OUT, 'video'), size: { width: 1280, height: 720 } },
  });
  page = await context.newPage();
  const consoleLog = fs.createWriteStream(path.join(OUT, 'console.log'), { flags: 'a' });
  page.on('console', (m) => consoleLog.write(`${new Date().toISOString()} ${m.type()} ${m.text()}\n`));
  page.on('pageerror', (e) => consoleLog.write(`${new Date().toISOString()} pageerror ${e.stack ?? e}\n`));

  // The URL comes only once the desktop is shared, so the CLI is still
  // starting while the harness answers the dialog. After a failed test
  // Playwright reruns this for the tests left, and then the token restores
  // the share without one.
  const starting = Cli.start(OUT);
  let settled = false;
  starting.then(
    () => (settled = true),
    () => (settled = true),
  );
  for (const deadline = Date.now() + 60_000; !settled && !lastRun().includes(CONSENT) && Date.now() < deadline; ) {
    await new Promise((r) => setTimeout(r, 250));
  }
  if (!settled && lastRun().includes(CONSENT)) await wl.consent();
  cli = await starting;
  await open(cli.url);
});

test.afterEach(async ({}, testInfo) => {
  if (testInfo.status === testInfo.expectedStatus || !page) return;
  const snapshot = await page
    .evaluate(async () => {
      const rj = (window as any).remotejump;
      return { state: rj?.state, stats: await rj?.stats?.().catch((e: Error) => String(e)) };
    })
    .catch((e) => ({ error: String(e) }));
  await testInfo.attach('remotejump', { body: JSON.stringify(snapshot, null, 2), contentType: 'application/json' });
  if (cli) await testInfo.attach('cli stderr (tail)', { body: cli.stderrTail(), contentType: 'text/plain' });
  await testInfo.attach('host', { body: await wl.diagnostics().catch(String), contentType: 'text/plain' });
});

test.afterAll(async () => {
  await cli?.stop();
  await context?.close();
});

test('consent: --attach auto picks the Wayland session and the CLI waits for the dialog', async () => {
  const log = stderrText();
  expect(log).toMatch(/msg=attaching to=wayland/);
  const asked = log.indexOf(CONSENT);
  expect(log.slice(asked).split('\n')[0], 'the consent line').toMatch(/on rjhost's screen.*Waiting up to 2m0s/);
  expect(log.indexOf('agent ready', asked), 'Ready after the consent line').toBeGreaterThan(asked);
  expect(log).toMatch(/agent ready" mode=attach display=wayland .*size=1280x720/);
});

test('frames decode: the monitor at its own size, changing when the desktop redraws', async () => {
  const size = await page.$eval(SCREEN, (v) => `${(v as HTMLVideoElement).videoWidth}x${(v as HTMLVideoElement).videoHeight}`);
  expect(size).toBe(`${MONITOR.width}x${MONITOR.height}`);
  const before = (await stats(page)).framesDecoded;
  expect(before, 'framesDecoded').toBeGreaterThan(0);
  await wl.redraw();
  await expect.poll(async () => (await stats(page)).framesDecoded, { message: 'framesDecoded grows', timeout: 10_000 }).toBeGreaterThan(before);
});

test('cursor: the page hides its own pointer and the picture carries the remote one', async () => {
  const cursorOf = () => page.evaluate(() => getComputedStyle(document.getElementById('screen')!).cursor);
  await expect.poll(cursorOf, { message: 'the video hides the local pointer', timeout: 5_000 }).toBe('none');

  const from = await pagePoint(page, SCREEN, 300, 200);
  await page.mouse.move(from.x, from.y, { steps: 3 });
  await waitForStillFrame(page, SCREEN);
  await sampleFrame(page, SCREEN, { save: 'pointer-away' });
  const to = { x: 900, y: 500 };
  const p = await pagePoint(page, SCREEN, to.x, to.y);
  await page.mouse.move(p.x, p.y, { steps: 3 });
  await expect
    .poll(async () => (await sampleFrame(page, SCREEN, { compare: 'pointer-away', region: { x: to.x - 4, y: to.y - 4, width: 32, height: 32 } })).changed, {
      message: 'pixels changed where the pointer went',
      timeout: 5_000,
    })
    .toBeGreaterThan(40);
});

test('pointer: motion and a click land in the probe window at the mapped pixel, within 2 px', async () => {
  const want = { x: 400, y: 300 };
  const p = await pagePoint(page, SCREEN, want.x, want.y);
  await page.mouse.move(p.x, p.y, { steps: 4 });
  await expect
    .poll(
      async () => {
        const m = (await wl.events()).filter((e) => e.ev === 'motion').pop();
        return m ? Math.max(Math.abs(m.x! - want.x), Math.abs(m.y! - want.y)) : Infinity;
      },
      { message: `the probe's last motion vs ${want.x},${want.y}`, timeout: 5_000 },
    )
    .toBeLessThanOrEqual(2);

  const n = (await wl.events()).length;
  const c = { x: 640, y: 360 };
  const cp = await pagePoint(page, SCREEN, c.x, c.y);
  await page.mouse.click(cp.x, cp.y);
  await expect
    .poll(
      async () =>
        (await eventsSince(n))
          .filter((e) => (e.ev === 'press' || e.ev === 'release') && Math.abs(e.x! - c.x) <= 2 && Math.abs(e.y! - c.y) <= 2)
          .map((e) => `${e.ev} ${e.button}`)
          .join(', '),
      { message: `a left click at ${c.x},${c.y}`, timeout: 5_000 },
    )
    .toBe('press 1, release 1');
});

test('keyboard: typed text arrives in the probe window', async () => {
  const n = (await wl.events()).length;
  await page.keyboard.type('Hi!', { delay: 40 });
  await expect
    .poll(
      async () =>
        (await eventsSince(n))
          .filter((e) => e.ev === 'key' && e.text && e.text !== '\u0000')
          .map((e) => e.text)
          .join(''),
      { message: 'characters the probe window received', timeout: 5_000 },
    )
    .toBe('Hi!');
});

// The picture only changes when the desktop draws, so a still Wayland screen
// sends what a still X11 one does: the keyframes Chrome asks for.
test('latency: after 20 s of a still screen, keystrokes show within 400 ms', async ({}, testInfo) => {
  testInfo.setTimeout(60_000);
  await page.waitForTimeout(20_000);
  await page.evaluate((selector) => {
    const w = window as any;
    const video = document.querySelector(selector) as HTMLVideoElement;
    w.rjKeys = [];
    w.rjFrames = [];
    window.addEventListener('keydown', (e) => e.key === 'x' && w.rjKeys.push(performance.now()), { capture: true });
    const onFrame = (_: number, m: VideoFrameCallbackMetadata) => {
      w.rjFrames.push({ presented: m.presentationTime, received: m.receiveTime ?? m.presentationTime });
      video.requestVideoFrameCallback(onFrame);
    };
    video.requestVideoFrameCallback(onFrame);
  }, SCREEN);
  for (let i = 0; i < 4; i++) {
    await page.keyboard.press('x');
    await page.waitForTimeout(250);
  }
  await page.waitForTimeout(1_500);

  type Shown = { presented: number; received: number };
  const { keys, frames } = await page.evaluate(() => ({
    keys: (window as any).rjKeys as number[],
    frames: (window as any).rjFrames as Shown[],
  }));
  const lag = keys.map((k) => Math.round((frames.find((f) => f.presented > k)?.presented ?? Infinity) - k));
  const held = frames.map((f) => Math.round(f.presented - f.received));
  testInfo.annotations.push({ type: 'latency', description: JSON.stringify({ lag, held }) });
  expect(keys, 'keystrokes seen by the page').toHaveLength(4);
  expect(Math.max(...lag), `ms from each keystroke to the next frame on screen: ${lag}`).toBeLessThan(400);
});

test('clipboard: browser to session and session to browser, through the portal', async () => {
  const text = `rj-clip-${Date.now().toString(36)}`;
  await page.evaluate((t) => (window as any).remotejump.sendClipboard(t), text);
  // F2 has the probe window read the session's clipboard.
  await expect
    .poll(
      async () => {
        await page.keyboard.press('F2');
        await page.waitForTimeout(300);
        return (await wl.events()).filter((e) => e.ev === 'clipboard').pop()?.text;
      },
      { message: "the session's clipboard, read by the probe window", timeout: 10_000 },
    )
    .toBe(text);

  // F3 has it take the clipboard with "from-session".
  await page.keyboard.press('F3');
  await expect
    .poll(() => page.evaluate(() => (window as any).remotejump.lastRemoteClipboard), {
      message: 'remotejump.lastRemoteClipboard',
      timeout: 10_000,
    })
    .toBe('from-session');
});

test('sound: what the desktop plays through PipeWire is heard once the speaker is on', async ({}, testInfo) => {
  expect(cli!.stderrTail(), 'the CLI reports no missing sound').not.toContain('no sound from');
  const audio = () =>
    page.evaluate(async () => {
      let out = { packets: 0, energy: 0 };
      (await ((window as any).remotejump.pc as RTCPeerConnection).getStats()).forEach((s: any) => {
        if (s.type === 'inbound-rtp' && s.kind === 'audio') out = { packets: s.packetsReceived ?? 0, energy: s.totalAudioEnergy ?? 0 };
      });
      return out;
    });
  const sound = page.locator('#sound');
  await page.mouse.move(640, 5); // brings the tucked HUD back
  await expect(sound, 'the speaker toggle').toBeVisible({ timeout: 10_000 });
  await expect.poll(async () => (await audio()).packets, { message: 'audio packets arrive' }).toBeGreaterThan(0);

  await sound.click();
  expect(await page.evaluate(() => (window as any).remotejump.muted), 'muted after the click').toBe(false);
  const before = (await audio()).energy;
  const playing = wl.playNoise(5);
  await expect
    .poll(async () => (await audio()).energy, { message: 'totalAudioEnergy grows while the desktop plays noise', timeout: 15_000 })
    .toBeGreaterThan(before);
  await playing;
  testInfo.annotations.push({ type: 'audio', description: JSON.stringify(await audio()) });
});

test('teardown: SIGINT ends the CLI, the share and the agent; the desktop lives on and the token stays private', async ({}, testInfo) => {
  expect(await wl.leftovers(), 'the agent and its share are visible before teardown').not.toBe('');
  const stop = await cli!.stop(10_000);
  testInfo.annotations.push({ type: 'exit', description: JSON.stringify(stop) });
  expect(stop.killed, 'CLI exited by itself within 10 s of SIGINT').toBe(false);
  expect(stop.code, 'CLI exit code').toBe(0);

  const left = Math.max(1_000, 10_000 - stop.afterMs);
  await expect.poll(wl.leftovers, { message: 'agent, gst-launch-1.0 and portal or mutter sessions on the host', timeout: left }).toBe('');
  expect(await wl.sessionAlive(), "rj's GNOME session still runs").toBe(true);
  expect(await wl.tokenMode(), 'the restore token, owner-only').toBe('600');
  expect(cli!.extraStdout, 'stdout carries only the URL').toEqual([]);
});

test('restore: a second run shares the desktop without asking', async ({}, testInfo) => {
  testInfo.setTimeout(120_000);
  await cli?.stop(); // already stopped unless the teardown test failed
  cli = await Cli.start(OUT);
  const run = lastRun();
  expect(run, 'no consent notice on the second run').not.toContain(CONSENT);
  expect(run).toMatch(/agent ready" mode=attach display=wayland/);
  await open(cli.url);
  const before = (await stats(page)).framesDecoded;
  await wl.redraw();
  await expect.poll(async () => (await stats(page)).framesDecoded, { message: 'framesDecoded grows', timeout: 10_000 }).toBeGreaterThan(before);

  const stop = await cli.stop(10_000);
  expect(stop.code, 'CLI exit code').toBe(0);
  await expect.poll(wl.leftovers, { message: 'leftovers after the second run', timeout: 10_000 }).toBe('');
});
