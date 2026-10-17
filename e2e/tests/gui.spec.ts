import { test, expect, type BrowserContext, type Page, type TestInfo } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';
import { Cli } from './helpers/cli';
import { pagePoint } from './helpers/coords';
import { CHANGED_FLOOR, isNonBlack, sampleFrame, waitForStillFrame } from './helpers/frames';
import * as host from './helpers/host';

// One robin session per scenario, shared by the tests below in order.
// When a test fails Playwright restarts the worker, so beforeAll brings up a
// fresh session for the tests that remain. Playwright traces this context
// (trace: retain-on-failure in the config).

const SCENARIO = process.env.SCENARIO ?? '';
const OUT = path.resolve('out');
const SCREEN = '#screen';
const FIRST_FRAME_MS = 20_000;
const RESTART_TIMEOUT_MS = Number(process.env.ROBIN_RESTART_TIMEOUT_MS ?? 15_000);

type Endpoint = { address: string; port: number; protocol: string; candidateType: string };
type Stats = {
  transport: 'udp' | 'tcp' | null;
  local: Endpoint;
  remote: Endpoint;
  framesDecoded: number;
  frameWidth: number;
  frameHeight: number;
  fps: number;
  kbps: number;
};

const state = (page: Page) => page.evaluate(() => (window as any).robin?.state as string | undefined);
const stats = (page: Page) => page.evaluate(() => (window as any).robin.stats() as Promise<Stats>);

let cli: Cli | undefined;
let context: BrowserContext;
let page: Page;
let sessionStart = 0;

test.beforeAll(async ({ browser }, testInfo) => {
  expect(['direct', 'blocked', 'restart', 'attach'], 'SCENARIO').toContain(SCENARIO);
  testInfo.setTimeout(150_000);
  context = await browser.newContext({
    viewport: { width: 1280, height: 720 },
    recordVideo: { dir: path.join(OUT, 'video'), size: { width: 1280, height: 720 } },
  });
  page = await context.newPage();
  const consoleLog = fs.createWriteStream(path.join(OUT, 'console.log'), { flags: 'a' });
  page.on('console', (m) => consoleLog.write(`${new Date().toISOString()} ${m.type()} ${m.text()}\n`));
  page.on('pageerror', (e) => consoleLog.write(`${new Date().toISOString()} pageerror ${e.stack ?? e}\n`));

  cli = await Cli.start(OUT);
  sessionStart = Date.now();
  await page.goto(cli.url);
  await expect
    .poll(() => page.evaluate(() => typeof (window as any).robin), { message: 'window.robin exists', timeout: 15_000 })
    .toBe('object');
  // Logs every state change to console.log, for reading a failure afterwards.
  await page.evaluate(() => {
    let last: string | undefined;
    setInterval(() => {
      const s = (window as any).robin?.state;
      if (s !== last) console.log(`[robin-state] ${last} -> ${s}`);
      last = s;
    }, 100);
  });
});

test.afterEach(async ({}, testInfo) => {
  // Playwright already attaches a screenshot and trace on failure.
  if (testInfo.status === testInfo.expectedStatus || !page) return;
  const snapshot = await page
    .evaluate(async () => {
      const robin = (window as any).robin;
      return { state: robin?.state, stats: await robin?.stats?.().catch((e: Error) => String(e)) };
    })
    .catch((e) => ({ error: String(e) }));
  await testInfo.attach('robin', { body: JSON.stringify(snapshot, null, 2), contentType: 'application/json' });
  if (cli) await testInfo.attach('cli stderr (tail)', { body: cli.stderrTail(), contentType: 'text/plain' });
  await testInfo.attach('host', { body: await host.diagnostics().catch(String), contentType: 'text/plain' });
});

test.afterAll(async () => {
  await cli?.stop();
  await context?.close(); // flushes the page video
});

async function connectedStats(testInfo: TestInfo, label: string): Promise<Stats> {
  await expect.poll(() => state(page), { message: 'state is connected', timeout: 15_000 }).toBe('connected');
  let s!: Stats;
  await expect
    .poll(async () => (s = await stats(page)).transport, { message: 'stats().transport is set', timeout: 10_000 })
    .not.toBeNull();
  await testInfo.attach(`stats ${label}`, { body: JSON.stringify(s, null, 2), contentType: 'application/json' });
  return s;
}

// typeIntoTerminal focuses the remote xterm through the video, types command
// and Enter, and checks both the effect on the host (file holds want) and the
// picture (the xterm region changed from the frame before typing).
async function typeIntoTerminal(command: string, file: string, want: string) {
  await host.ssh(`rm -f ${file}`);
  let term: host.Rect | null = null;
  await expect.poll(async () => (term = await host.terminal()), { message: 'an xterm is mapped', timeout: 15_000 }).not.toBeNull();
  const t = term!;

  const centre = await pagePoint(page, SCREEN, t.x + t.width / 2, t.y + t.height / 2);
  await page.mouse.click(centre.x, centre.y);
  await waitForStillFrame(page, SCREEN);
  await sampleFrame(page, SCREEN, { save: 'pre-typing' });
  await page.waitForTimeout(600);
  const noise = (await sampleFrame(page, SCREEN, { compare: 'pre-typing', region: t })).changed!;

  await page.keyboard.type(command, { delay: 20 });
  await page.keyboard.press('Enter');

  await expect
    .poll(async () => (await host.sshRaw(`cat ${file}`)).stdout.trim(), { message: `${file} on the host`, timeout: 10_000 })
    .toBe(want);
  const floor = Math.max(CHANGED_FLOOR, noise * 3 + 50);
  await expect
    .poll(async () => (await sampleFrame(page, SCREEN, { compare: 'pre-typing', region: t })).changed, {
      message: `pixels changed in the xterm since before typing (noise ${noise})`,
      timeout: 5_000,
    })
    .toBeGreaterThan(floor);
}

test('frames decode: non-black within 20 s, framesDecoded increasing', async () => {
  const left = Math.max(1_000, FIRST_FRAME_MS - (Date.now() - sessionStart));
  await expect
    .poll(async () => isNonBlack(await sampleFrame(page, SCREEN)), { message: 'a non-black frame', timeout: left, intervals: [250] })
    .toBe(true);
  const first = (await stats(page)).framesDecoded;
  expect(first, 'framesDecoded').toBeGreaterThan(0);
  // An encoder that skips idle frames needs damage to send another one.
  await host.retitleTerminal().catch(() => {});
  await expect.poll(async () => (await stats(page)).framesDecoded, { message: 'framesDecoded grows', timeout: 10_000 }).toBeGreaterThan(first);
});

test('keyboard: a typed command runs on the host and shows in the picture', async () => {
  await typeIntoTerminal(`echo robin-e2e > /tmp/robin-e2e-${SCENARIO}`, `/tmp/robin-e2e-${SCENARIO}`, 'robin-e2e');
});

// A still screen sends only the keyframes Chrome asks for every 3 s, and
// Chrome's jitter estimate grows on them until it holds new frames back for
// about a second, so what is typed after a pause shows late.
test('latency: after 20 s of a still screen, keystrokes show within 400 ms', async ({}, testInfo) => {
  testInfo.setTimeout(60_000);
  let term: host.Rect | null = null;
  await expect.poll(async () => (term = await host.terminal()), { message: 'an xterm is mapped', timeout: 15_000 }).not.toBeNull();
  const t = term!;
  const centre = await pagePoint(page, SCREEN, t.x + t.width / 2, t.y + t.height / 2);
  await page.mouse.click(centre.x, centre.y);
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
  // Held frames still surface, so a failure says by how much.
  await page.waitForTimeout(1_500);
  await page.keyboard.press('Control+U');

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

test('pointer: the remote cursor lands where the page points, within 2 px', async () => {
  const { width, height } = await host.displayGeometry();
  for (const [fx, fy] of [
    [0.23, 0.31],
    [0.71, 0.64],
  ]) {
    const want = { x: Math.round(width * fx), y: Math.round(height * fy) };
    const p = await pagePoint(page, SCREEN, want.x, want.y);
    await page.mouse.move(p.x, p.y, { steps: 4 });
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
});

test('cursor: the page wears the remote pointer image, which changes over the xterm', async () => {
  const cursorOf = () => page.evaluate(() => getComputedStyle(document.getElementById('screen')!).cursor);
  const { width, height } = await host.displayGeometry();
  const desk = await pagePoint(page, SCREEN, width - 20, height - 20);
  await page.mouse.move(desk.x, desk.y, { steps: 3 });
  await expect.poll(cursorOf, { message: 'a remote cursor image on the video', timeout: 5_000 }).toContain('url(');
  const onDesk = await cursorOf();

  let term: host.Rect | null = null;
  await expect.poll(async () => (term = await host.terminal()), { message: 'an xterm is mapped', timeout: 15_000 }).not.toBeNull();
  const t = term!;
  const over = await pagePoint(page, SCREEN, t.x + t.width / 2, t.y + t.height / 2);
  await page.mouse.move(over.x, over.y, { steps: 3 });
  await expect.poll(cursorOf, { message: 'the xterm text cursor replaces the desktop arrow', timeout: 5_000 }).not.toBe(onDesk);
});

test('clipboard: browser to host and host to browser', async () => {
  const n = Date.now().toString(36);
  await page.evaluate((t) => (window as any).robin.sendClipboard(t), `robin-clip-${n}`);
  await expect.poll(host.clipboard, { message: 'host clipboard', timeout: 10_000 }).toBe(`robin-clip-${n}`);

  await host.setClipboard(`robin-back-${n}`);
  await expect
    .poll(() => page.evaluate(() => (window as any).robin.lastRemoteClipboard), {
      message: 'robin.lastRemoteClipboard',
      timeout: 10_000,
    })
    .toBe(`robin-back-${n}`);
});

test('resize: display and video follow a 1000x700 viewport within 10 s', async () => {
  test.skip(SCENARIO === 'attach', 'an attached desktop keeps its size; see the attach resize test');
  await page.setViewportSize({ width: 1000, height: 700 });
  await expect
    .poll(
      async () => {
        const video = await page.$eval(SCREEN, (v) => `${(v as HTMLVideoElement).videoWidth}x${(v as HTMLVideoElement).videoHeight}`);
        const g = await host.displayGeometry();
        return `video ${video}, display ${g.width}x${g.height}`;
      },
      { timeout: 10_000 },
    )
    .toBe('video 1000x700, display 1000x700');
});

test('sound: the desktop is heard once the speaker is turned on', async ({}, testInfo) => {
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
  expect(await page.evaluate(() => (window as any).robin.muted), 'muted until clicked').toBe(true);
  // The agent sends packets even for silence, so the track is live before
  // anything plays.
  await expect.poll(async () => (await audio()).packets, { message: 'audio packets arrive' }).toBeGreaterThan(0);

  await sound.click();
  expect(await page.evaluate(() => (window as any).robin.muted), 'muted after the click').toBe(false);
  // Chrome reports energy only for audio it plays, which a muted element
  // does not.
  const before = (await audio()).energy;
  const playing = host.playNoise(5);
  await expect
    .poll(async () => (await audio()).energy, { message: 'totalAudioEnergy grows while the host plays noise', timeout: 15_000 })
    .toBeGreaterThan(before);
  await playing;
  testInfo.annotations.push({ type: 'audio', description: JSON.stringify(await audio()) });
  // A spawned session's programs get a cookie in the run dir, so libpulse
  // writes none. An attached desktop's own server keeps its cookie there.
  if (SCENARIO !== 'attach') {
    expect(await host.exists('/home/robin/.config/pulse'), '~/.config/pulse on the host').toBe(false);
  }
});

if (SCENARIO === 'direct') {
  test('transport: direct session uses UDP', async ({}, testInfo) => {
    expect((await connectedStats(testInfo, 'direct')).transport).toBe('udp');
  });
}

if (SCENARIO === 'blocked') {
  test('transport: with UDP blocked the session runs on ICE-TCP through the SSH forward', async ({}, testInfo) => {
    const s = await connectedStats(testInfo, 'blocked');
    expect(s.transport).toBe('tcp');
    expect(s.remote.address).toBe('127.0.0.1');
  });
}

if (SCENARIO === 'restart') {
  test('transport: blocking UDP mid-session falls back to TCP and keeps working', async ({}, testInfo) => {
    testInfo.setTimeout(RESTART_TIMEOUT_MS + 60_000);
    expect((await connectedStats(testInfo, 'before block')).transport).toBe('udp');

    await host.blockUdp();
    const blockedAt = Date.now();
    const seen: string[] = [];
    await expect
      .poll(
        async () => {
          const st = (await state(page)) ?? 'none';
          const now = st === 'connected' ? `connected/${(await stats(page)).transport}` : st;
          if (seen.at(-1) !== now) seen.push(now);
          return now;
        },
        { message: 'state recovers to connected over tcp', timeout: RESTART_TIMEOUT_MS, intervals: [250] },
      )
      .toBe('connected/tcp');
    testInfo.annotations.push({ type: 'recovery', description: `${Date.now() - blockedAt} ms via ${seen.join(' -> ')}` });

    const after = await connectedStats(testInfo, 'after block');
    expect(after.remote.address).toBe('127.0.0.1');
    await typeIntoTerminal('echo robin-e2e-tcp > /tmp/robin-e2e-restart-tcp', '/tmp/robin-e2e-restart-tcp', 'robin-e2e-tcp');
  });
}

if (SCENARIO === 'attach') {
  test('attach: a 1000x700 viewport scales the video and leaves the desktop its size', async () => {
    const desk = await host.displayGeometry();
    await page.setViewportSize({ width: 1000, height: 700 });
    // Past the page's resize debounce and the agent's answer, had there been one.
    await page.waitForTimeout(2_000);
    const v = await page.$eval(SCREEN, (el) => {
      const e = el as HTMLVideoElement;
      return { w: e.videoWidth, h: e.videoHeight, box: e.getBoundingClientRect().width };
    });
    const g = await host.displayGeometry();
    expect(`video ${v.w}x${v.h}, display ${g.width}x${g.height}`).toBe(
      `video ${desk.width}x${desk.height}, display ${desk.width}x${desk.height}`,
    );
    expect(v.box, 'the video element shrank with the viewport').toBeLessThanOrEqual(1000);
  });

  test('transport: attached session uses UDP', async ({}, testInfo) => {
    expect((await connectedStats(testInfo, 'attach')).transport).toBe('udp');
  });

  test('attach: a character the keymap lacks borrows a spare key, and autorepeat is off meanwhile', async () => {
    // Playwright types only characters of a US layout as key events.
    await page.evaluate(() => {
      for (const type of ['keydown', 'keyup']) {
        window.dispatchEvent(new KeyboardEvent(type, { key: 'ж', code: 'Semicolon', bubbles: true, cancelable: true }));
      }
    });
    await expect
      .poll(() => host.onDisplay(`sh -c 'xmodmap -pke | grep -cE "U0436|Cyrillic_zhe" || true'`), { message: 'a keycode bound to ж' })
      .not.toBe('0');
    expect(await host.onDisplay(`sh -c 'xset q | grep -o "auto repeat: *[a-z]*"'`)).toMatch(/off$/);
    await page.keyboard.press('Backspace');
  });

  test('teardown: SIGINT ends the CLI and leaves the attached desktop as it found it', async ({}, testInfo) => {
    expect(await host.agentLeftovers(), 'the agent is visible before teardown').not.toBe('');
    const stop = await cli!.stop(10_000);
    testInfo.annotations.push({ type: 'exit', description: JSON.stringify(stop) });
    expect(stop.killed, 'CLI exited by itself within 10 s of SIGINT').toBe(false);
    expect(stop.code, 'CLI exit code').toBe(0);

    const left = Math.max(1_000, 10_000 - stop.afterMs);
    await expect.poll(host.agentLeftovers, { message: 'agent processes and run dirs on the host', timeout: left }).toBe('');
    // Same Xvfb, openbox and xterm, same size, autorepeat and keymaps.
    expect(await host.liveState()).toBe(await host.liveBefore());
    expect(cli!.extraStdout, 'stdout carries only the URL').toEqual([]);
  });
}

test('teardown: SIGINT ends the CLI and leaves nothing behind on the host', async ({}, testInfo) => {
  test.skip(SCENARIO === 'attach', 'the attached desktop outlives the session; see the attach teardown');
  const dir = await host.runDir();
  expect(await host.leftovers(), 'session processes are visible before teardown').not.toBe('');

  const stop = await cli!.stop(10_000);
  testInfo.annotations.push({ type: 'exit', description: JSON.stringify(stop) });
  expect(stop.killed, 'CLI exited by itself within 10 s of SIGINT').toBe(false);
  expect(stop.code, 'CLI exit code').toBe(0);

  const left = Math.max(1_000, 10_000 - stop.afterMs);
  await expect.poll(host.leftovers, { message: 'session processes on the host', timeout: left }).toBe('');
  await expect.poll(() => host.exists(dir), { message: `run dir ${dir} exists`, timeout: 1_000 }).toBe(false);
  expect(cli!.extraStdout, 'stdout carries only the URL').toEqual([]);
  testInfo.annotations.push({ type: 'page state after teardown', description: String(await state(page)) });
});
