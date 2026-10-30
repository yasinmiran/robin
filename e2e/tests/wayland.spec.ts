import { test, expect, type BrowserContext, type Page } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';
import { Cli } from './helpers/cli';
import { pagePoint } from './helpers/coords';
import { savePageCoverage, startPageCoverage } from './helpers/cover';
import { COLOUR, TONE_HZ, near, pcm, pixel, toneOf } from './helpers/fakemedia';
import { isNonBlack, sampleFrame, waitForStillFrame } from './helpers/frames';
import * as wl from './helpers/wayland';

// The wayland scenario: `robin gui --attach auto` against a host whose
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
const stats = (p: Page) => p.evaluate(() => (window as any).robin.stats() as Promise<{ framesDecoded: number }>);

async function open(url: string) {
  await page.goto(url);
  await expect
    .poll(() => page.evaluate(() => typeof (window as any).robin), { message: 'window.robin exists', timeout: 15_000 })
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
  await startPageCoverage(page);
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
      const robin = (window as any).robin;
      return { state: robin?.state, stats: await robin?.stats?.().catch((e: Error) => String(e)) };
    })
    .catch((e) => ({ error: String(e) }));
  await testInfo.attach('robin', { body: JSON.stringify(snapshot, null, 2), contentType: 'application/json' });
  if (cli) await testInfo.attach('cli stderr (tail)', { body: cli.stderrTail(), contentType: 'text/plain' });
  await testInfo.attach('host', { body: await wl.diagnostics().catch(String), contentType: 'text/plain' });
});

test.afterAll(async () => {
  await cli?.stop();
  await savePageCoverage(page, 'wayland');
  await context?.close();
});

test('consent: --attach auto picks the Wayland session and the CLI waits for the dialog', async () => {
  const log = stderrText();
  expect(log).toMatch(/msg=attaching to=wayland/);
  const asked = log.indexOf(CONSENT);
  expect(log.slice(asked).split('\n')[0], 'the consent line').toMatch(/on robinhost's screen.*Waiting up to 2m0s/);
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
    w.robinKeys = [];
    w.robinFrames = [];
    window.addEventListener('keydown', (e) => e.key === 'x' && w.robinKeys.push(performance.now()), { capture: true });
    const onFrame = (_: number, m: VideoFrameCallbackMetadata) => {
      w.robinFrames.push({ presented: m.presentationTime, received: m.receiveTime ?? m.presentationTime });
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
    keys: (window as any).robinKeys as number[],
    frames: (window as any).robinFrames as Shown[],
  }));
  const lag = keys.map((k) => Math.round((frames.find((f) => f.presented > k)?.presented ?? Infinity) - k));
  const held = frames.map((f) => Math.round(f.presented - f.received));
  testInfo.annotations.push({ type: 'latency', description: JSON.stringify({ lag, held }) });
  expect(keys, 'keystrokes seen by the page').toHaveLength(4);
  expect(Math.max(...lag), `ms from each keystroke to the next frame on screen: ${lag}`).toBeLessThan(400);
});

test('clipboard: browser to session and session to browser, through the portal', async () => {
  const text = `robin-clip-${Date.now().toString(36)}`;
  await page.evaluate((t) => (window as any).robin.sendClipboard(t), text);
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
    .poll(() => page.evaluate(() => (window as any).robin.lastRemoteClipboard), {
      message: 'robin.lastRemoteClipboard',
      timeout: 10_000,
    })
    .toBe('from-session');
});

// Text an input method commits goes as a text control: as keys where the
// layout has every character, and otherwise pasted through the session's
// clipboard, which gets back what it held a second later.
test('text: Latin text types as keys, the rest is pasted once, and the clipboard is put back', async () => {
  const center = await pagePoint(page, SCREEN, 640, 360);
  await page.mouse.click(center.x, center.y);
  expect(await page.evaluate(() => document.activeElement?.id), 'the text box holds the focus').toBe('typing');
  const entered = async (n: number) =>
    (await eventsSince(n))
      .filter((e) => (e.ev === 'key' && e.text && e.text !== '\u0000') || e.ev === 'paste')
      .map((e) => e.text)
      .join('');
  const cdp = await context.newCDPSession(page);
  try {
    let n = (await wl.events()).length;
    await cdp.send('Input.insertText', { text: 'robin ok!' });
    await expect.poll(() => entered(n), { message: 'Latin text the probe window received', timeout: 5_000 }).toBe('robin ok!');
    expect((await eventsSince(n)).filter((e) => e.ev === 'paste'), 'no paste for Latin text').toEqual([]);

    n = (await wl.events()).length;
    await cdp.send('Input.insertText', { text: '日本語ü' });
    await expect.poll(() => entered(n), { message: 'non-Latin text the probe window received', timeout: 5_000 }).toBe('日本語ü');
    expect((await eventsSince(n)).filter((e) => e.ev === 'paste-key'), 'one paste').toHaveLength(1);
  } finally {
    await cdp.detach();
  }
  // The clipboard test left "from-session" there.
  await page.waitForTimeout(1_500);
  await expect
    .poll(
      async () => {
        await page.keyboard.press('F2');
        await page.waitForTimeout(300);
        return (await wl.events()).filter((e) => e.ev === 'clipboard').pop()?.text;
      },
      { message: "the session's clipboard after the paste", timeout: 10_000 },
    )
    .toBe('from-session');
});

test('sound: what the desktop plays through PipeWire is heard once the speaker is on', async ({}, testInfo) => {
  expect(cli!.stderrTail(), 'the CLI reports no missing sound').not.toContain('no sound from');
  const audio = () =>
    page.evaluate(async () => {
      let out = { packets: 0, energy: 0 };
      (await ((window as any).robin.pc as RTCPeerConnection).getStats()).forEach((s: any) => {
        if (s.type === 'inbound-rtp' && s.kind === 'audio') out = { packets: s.packetsReceived ?? 0, energy: s.totalAudioEnergy ?? 0 };
      });
      return out;
    });
  const sound = page.locator('#sound');
  await page.mouse.move(640, 5); // brings the tucked HUD back
  await expect(sound, 'the speaker toggle').toBeVisible({ timeout: 10_000 });
  await expect.poll(async () => (await audio()).packets, { message: 'audio packets arrive' }).toBeGreaterThan(0);

  await sound.click();
  expect(await page.evaluate(() => (window as any).robin.muted), 'muted after the click').toBe(false);
  const before = (await audio()).energy;
  const playing = wl.playNoise(5);
  await expect
    .poll(async () => (await audio()).energy, { message: 'totalAudioEnergy grows while the desktop plays noise', timeout: 15_000 })
    .toBeGreaterThan(before);
  await playing;
  testInfo.annotations.push({ type: 'audio', description: JSON.stringify(await audio()) });
});

test("microphone: a PipeWire source that hears the page once it is switched on", async ({}, testInfo) => {
  testInfo.setTimeout(120_000);
  expect(stderrText()).toContain('your microphone can reach robinhost as Robin microphone (PipeWire)');
  const { mic: node } = await wl.deviceNodes();
  expect(node, "the microphone's node").toMatch(/^robin-mic-\d+$/);
  const mic = page.locator('button.device[data-device="mic"]');
  const micState = () => page.evaluate(() => (window as any).robin.device('mic'));
  const localSDP = () => page.evaluate(() => ((window as any).robin.pc as RTCPeerConnection).localDescription?.sdp);
  await page.mouse.move(640, 5); // brings the tucked HUD back
  await expect(mic, 'the microphone switch').toBeVisible({ timeout: 10_000 });
  await expect(mic).toHaveAttribute('data-state', 'off');
  expect(toneOf(pcm(await wl.recordSource(node, 1.5))).loud, 'the desktop hears nothing before the switch').toBeLessThan(0.05);

  const offered = await localSDP();
  await mic.click();
  await expect.poll(micState, { message: 'the switch turns on' }).toBe('on');
  let heard = { hz: 0, loud: 0 };
  await expect
    .poll(async () => (heard = toneOf(pcm(await wl.recordSource(node, 2)))).loud, { message: 'the tone on the desktop', timeout: 30_000, intervals: [0] })
    .toBeGreaterThan(0.4);
  testInfo.annotations.push({ type: 'microphone', description: JSON.stringify(heard) });
  await page.mouse.move(640, 5);
  await page.waitForTimeout(400); // the tab slides back in
  await testInfo.attach('status tab, microphone on', { body: await page.locator('#hud').screenshot(), contentType: 'image/png' });
  expect(heard.hz, "the page's tone, at its pitch").toBeGreaterThan(TONE_HZ * 0.95);
  expect(heard.hz).toBeLessThan(TONE_HZ * 1.05);

  await page.mouse.move(640, 5);
  await mic.click();
  await expect.poll(micState, { message: 'the switch turns off' }).toBe('off');
  expect(await localSDP(), 'switching the microphone on and off renegotiated nothing').toBe(offered);
});

test('camera: a PipeWire camera that shows black, then the page, then black again', async ({}, testInfo) => {
  testInfo.setTimeout(120_000);
  expect(stderrText()).toContain('your camera can reach robinhost as Robin camera (PipeWire)');
  const { camera: node } = await wl.deviceNodes();
  expect(node, "the camera's node").toMatch(/^robin-camera-\d+$/);
  const camera = page.locator('button.device[data-device="camera"]');
  const cameraState = () => page.evaluate(() => (window as any).robin.device('camera'));
  const localSDP = () => page.evaluate(() => ((window as any).robin.pc as RTCPeerConnection).localDescription?.sdp);
  const middle = async () => pixel(await wl.cameraFrame(node), 1280, 720, 640, 400);
  await page.mouse.move(640, 5);
  await expect(camera, 'the camera switch').toBeVisible({ timeout: 10_000 });
  await expect(camera).toHaveAttribute('data-state', 'off');
  let seen = await middle();
  expect(seen.y, 'black before the switch').toBeLessThan(30);

  const offered = await localSDP();
  await camera.click();
  await expect.poll(cameraState, { message: 'the switch turns on' }).toBe('on');
  await expect
    .poll(async () => {
      seen = await middle();
      return near(seen.y, COLOUR.y) && near(seen.u, COLOUR.u) && near(seen.v, COLOUR.v);
    }, { message: "the page's picture on the desktop", timeout: 30_000, intervals: [500] })
    .toBe(true);
  testInfo.annotations.push({ type: 'camera', description: JSON.stringify(seen) });
  await page.mouse.move(640, 5);
  await page.waitForTimeout(400); // the tab slides back in
  await testInfo.attach('status tab, camera on', { body: await page.locator('#hud').screenshot(), contentType: 'image/png' });

  await page.mouse.move(640, 5);
  await camera.click();
  await expect.poll(cameraState, { message: 'the switch turns off' }).toBe('off');
  expect(await localSDP(), 'switching the camera on and off renegotiated nothing').toBe(offered);
  await expect
    .poll(async () => (await middle()).y, { message: 'black again, not the last picture', timeout: 15_000, intervals: [500] })
    .toBeLessThan(30);
});

test('teardown: SIGINT ends the CLI, the share and the agent; the desktop lives on and the token stays private', async ({}, testInfo) => {
  expect(await wl.leftovers(), 'the agent and its share are visible before teardown').not.toBe('');
  const stop = await cli!.stop(10_000);
  testInfo.annotations.push({ type: 'exit', description: JSON.stringify(stop) });
  expect(stop.killed, 'CLI exited by itself within 10 s of SIGINT').toBe(false);
  expect(stop.code, 'CLI exit code').toBe(0);

  const left = Math.max(1_000, 10_000 - stop.afterMs);
  await expect.poll(wl.leftovers, { message: 'agent, gst-launch-1.0 and portal or mutter sessions on the host', timeout: left }).toBe('');
  expect(await wl.sessionAlive(), "robin's GNOME session still runs").toBe(true);
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
