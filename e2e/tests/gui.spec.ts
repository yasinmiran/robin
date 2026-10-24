import { test, expect, type BrowserContext, type Page, type TestInfo } from '@playwright/test';
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { Cli, robin, sshProcesses } from './helpers/cli';
import { pagePoint } from './helpers/coords';
import { savePageCoverage, startPageCoverage } from './helpers/cover';
import { TONE_HZ, pcm, toneOf } from './helpers/fakemedia';
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

// Playwright's WebKit on Linux decodes WebRTC video but renders no remote
// track into a <video> element, not even a VP8 loopback between two peer
// connections in a blank page (e2e/README.md). There the checks that read
// the picture or map the pointer onto it are skipped, and decoding is read
// from the browser's stats instead.
const PICTURE = process.env.ROBIN_E2E_BROWSER !== 'webkit';
const NO_PICTURE = "Playwright's Linux WebKit renders no remote WebRTC video";

// run.sh offers the microphone and camera in the direct scenario, with
// Chrome's fake devices (helpers/fakemedia.ts) behind them.
const DEVICES = (process.env.ROBIN_CLI_ARGS ?? '').includes('--mic');

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

// decodedVideo is the browser's own account of the video it decoded: the
// negotiated codec, how many frames, and their size.
const decodedVideo = (page: Page) =>
  page.evaluate(async () => {
    const report = await ((window as any).robin.pc as RTCPeerConnection).getStats();
    let out = { codec: '', framesDecoded: 0, keyFramesDecoded: 0, width: 0, height: 0 };
    report.forEach((s: any) => {
      if (s.type !== 'inbound-rtp' || s.kind !== 'video') return;
      const c = report.get(s.codecId);
      out = {
        codec: `${c?.mimeType ?? ''} ${c?.sdpFmtpLine ?? ''}`.trim(),
        framesDecoded: s.framesDecoded ?? 0,
        keyFramesDecoded: s.keyFramesDecoded ?? 0,
        width: s.frameWidth ?? 0,
        height: s.frameHeight ?? 0,
      };
    });
    return out;
  });

// videoCounters reads what Chrome counts about the video it receives:
// keyframes it asked for and decoded, packets it NACKed, and freezes.
const videoCounters = (page: Page) =>
  page.evaluate(async () => {
    const report = await ((window as any).robin.pc as RTCPeerConnection).getStats();
    let out = { pliCount: 0, nackCount: 0, keyFramesDecoded: 0, framesDecoded: 0, freezeCount: 0, totalFreezesDuration: 0 };
    report.forEach((s: any) => {
      if (s.type !== 'inbound-rtp' || s.kind !== 'video') return;
      out = {
        pliCount: s.pliCount ?? 0,
        nackCount: s.nackCount ?? 0,
        keyFramesDecoded: s.keyFramesDecoded ?? 0,
        framesDecoded: s.framesDecoded ?? 0,
        freezeCount: s.freezeCount ?? 0,
        totalFreezesDuration: s.totalFreezesDuration ?? 0,
      };
    });
    return out;
  });

let cli: Cli | undefined;
let context: BrowserContext;
let page: Page;
let sessionStart = 0;

test.beforeAll(async ({ browser }, testInfo) => {
  expect(['direct', 'blocked', 'restart', 'resume', 'rejoin', 'attach', 'nomux'], 'SCENARIO').toContain(SCENARIO);
  testInfo.setTimeout(150_000);
  context = await browser.newContext({
    viewport: { width: 1280, height: 720 },
    recordVideo: { dir: path.join(OUT, 'video'), size: { width: 1280, height: 720 } },
  });
  page = await context.newPage();
  await startPageCoverage(page);
  const consoleLog = fs.createWriteStream(path.join(OUT, 'console.log'), { flags: 'a' });
  page.on('console', (m) => consoleLog.write(`${new Date().toISOString()} ${m.type()} ${m.text()}\n`));
  page.on('pageerror', (e) => consoleLog.write(`${new Date().toISOString()} pageerror ${e.stack ?? e}\n`));

  cli = await Cli.start(OUT);
  sessionStart = Date.now();
  await page.goto(cli.url);
  await expect
    .poll(() => page.evaluate(() => typeof (window as any).robin), { message: 'window.robin exists', timeout: 15_000 })
    .toBe('object');
  // Logs every state change, and anything the page's CSP blocks, to
  // console.log, for reading a failure afterwards.
  await page.evaluate(() => {
    let last: string | undefined;
    setInterval(() => {
      const s = (window as any).robin?.state;
      if (s !== last) console.log(`[robin-state] ${last} -> ${s}`);
      last = s;
    }, 100);
    document.addEventListener('securitypolicyviolation', (e) => {
      const where = `${e.sourceFile || '?'}:${e.lineNumber}`;
      console.log(`[robin-csp] ${e.effectiveDirective} blocked ${e.blockedURI || 'inline'} at ${where} ${e.sample}`);
    });
  });
});

test.afterEach(async ({}, testInfo) => {
  // Playwright already attaches a screenshot and trace on failure.
  if (testInfo.status === testInfo.expectedStatus || !page) return;
  const snapshot = await page
    .evaluate(async () => {
      const robin = (window as any).robin;
      // The browser's own view of the video stream: decoder, frames received
      // against decoded, keyframes, what it asked for.
      let inbound: unknown = null;
      (await (robin?.pc as RTCPeerConnection | undefined)?.getStats().catch(() => undefined))?.forEach((s: any) => {
        if (s.type === 'inbound-rtp' && s.kind === 'video') inbound = s;
      });
      return { state: robin?.state, stats: await robin?.stats?.().catch((e: Error) => String(e)), inbound };
    })
    .catch((e) => ({ error: String(e) }));
  await testInfo.attach('robin', { body: JSON.stringify(snapshot, null, 2), contentType: 'application/json' });
  if (cli) await testInfo.attach('cli stderr (tail)', { body: cli.stderrTail(), contentType: 'text/plain' });
  await testInfo.attach('host', { body: await host.diagnostics().catch(String), contentType: 'text/plain' });
});

test.afterAll(async () => {
  await cli?.stop();
  await savePageCoverage(page, 'gui');
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
  test.skip(!PICTURE, NO_PICTURE);
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

if (!PICTURE) {
  test('frames decode: Constrained Baseline H.264 keeps decoding, read from getStats', async ({}, testInfo) => {
    const left = Math.max(1_000, FIRST_FRAME_MS - (Date.now() - sessionStart));
    await expect
      .poll(async () => (await decodedVideo(page)).framesDecoded, { message: 'framesDecoded', timeout: left })
      .toBeGreaterThan(0);
    const first = await decodedVideo(page);
    testInfo.annotations.push({ type: 'decoded', description: JSON.stringify(first) });
    expect(first.codec, 'negotiated codec').toMatch(/^video\/H264 /i);
    expect(first.codec).toContain('packetization-mode=1');
    expect(first.codec).toContain('profile-level-id=42e01f');
    expect(first.keyFramesDecoded, 'keyframes decoded').toBeGreaterThan(0);
    expect(first.width * first.height, 'decoded frame size').toBeGreaterThan(0);
    await host.retitleTerminal().catch(() => {});
    await expect
      .poll(async () => (await decodedVideo(page)).framesDecoded, { message: 'framesDecoded grows', timeout: 10_000 })
      .toBeGreaterThan(first.framesDecoded);
  });
}

test('keyboard: a typed command runs on the host and shows in the picture', async () => {
  test.skip(!PICTURE, NO_PICTURE);
  await typeIntoTerminal(`echo robin-e2e > /tmp/robin-e2e-${SCENARIO}`, `/tmp/robin-e2e-${SCENARIO}`, 'robin-e2e');
});

if (!PICTURE) {
  // Without a picture there is nothing to click, so the host focuses the
  // xterm the way that click would.
  test('keyboard: a typed command runs on the host', async () => {
    const file = `/tmp/robin-e2e-${SCENARIO}`;
    await host.ssh(`rm -f ${file}`);
    await expect.poll(host.terminal, { message: 'an xterm is mapped', timeout: 15_000 }).not.toBeNull();
    await host.focusTerminal();
    await page.keyboard.type(`echo robin-e2e > ${file}`, { delay: 20 });
    await page.keyboard.press('Enter');
    await expect
      .poll(async () => (await host.sshRaw(`cat ${file}`)).stdout.trim(), { message: `${file} on the host`, timeout: 10_000 })
      .toBe('robin-e2e');
  });
}

// Chrome asks for a keyframe after 3 s without a frame, and its jitter
// estimate grows on those keyframes until it holds new frames back. The
// agent sends a still picture again every 500 ms, so a still screen costs
// no keyframe request and what is typed after a pause shows at once.
test('latency: after 20 s of a still screen, keystrokes show within 400 ms', async ({}, testInfo) => {
  test.skip(!PICTURE, NO_PICTURE);
  testInfo.setTimeout(60_000);
  let term: host.Rect | null = null;
  await expect.poll(async () => (term = await host.terminal()), { message: 'an xterm is mapped', timeout: 15_000 }).not.toBeNull();
  const t = term!;
  const centre = await pagePoint(page, SCREEN, t.x + t.width / 2, t.y + t.height / 2);
  await page.mouse.click(centre.x, centre.y);
  // The click may still be on its way to the picture; count from after it.
  await page.waitForTimeout(1_000);
  const before = await videoCounters(page);
  await page.waitForTimeout(19_000);
  const after = await videoCounters(page);
  const still = Object.fromEntries(Object.entries(after).map(([k, v]) => [k, Number((v - (before as any)[k]).toFixed(3))]));
  testInfo.annotations.push({ type: 'still screen', description: JSON.stringify(still) });
  expect.soft(still.pliCount, `keyframes Chrome asked for during 19 s of a still screen: ${JSON.stringify(still)}`).toBe(0);

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
  test.skip(!PICTURE, NO_PICTURE);
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
  test.skip(!PICTURE, NO_PICTURE);
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
        // Without a picture, the size of the frames the browser decoded.
        const video = PICTURE
          ? await page.$eval(SCREEN, (v) => `${(v as HTMLVideoElement).videoWidth}x${(v as HTMLVideoElement).videoHeight}`)
          : await decodedVideo(page).then((v) => `${v.width}x${v.height}`);
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

test("microphone: off until switched on, then the desktop's default source hears the page", async ({}, testInfo) => {
  test.skip(!DEVICES, 'this scenario offers no microphone');
  testInfo.setTimeout(120_000);
  expect(cli!.stderrTail(Infinity)).toContain('your microphone can reach robinhost as Robin microphone (the private pulseaudio)');
  const mic = page.locator('button.device[data-device="mic"]');
  const micState = () => page.evaluate(() => (window as any).robin.device('mic'));
  const localSDP = () => page.evaluate(() => ((window as any).robin.pc as RTCPeerConnection).localDescription?.sdp);
  await page.mouse.move(640, 5); // brings the tucked HUD back
  await expect(mic, 'the microphone switch').toBeVisible({ timeout: 10_000 });
  await expect(mic).toHaveAttribute('data-state', 'off');
  expect(toneOf(pcm(await host.recordMicrophone(1.5))).loud, 'the host hears nothing before the switch').toBeLessThan(0.05);

  const offered = await localSDP();
  await mic.click();
  await expect.poll(micState, { message: 'the switch turns on' }).toBe('on');
  let heard = { hz: 0, loud: 0 };
  await expect
    .poll(async () => (heard = toneOf(pcm(await host.recordMicrophone(2)))).loud, { message: 'the tone on the host', timeout: 30_000, intervals: [0] })
    .toBeGreaterThan(0.4);
  testInfo.annotations.push({ type: 'microphone', description: JSON.stringify(heard) });
  expect(heard.hz, "the page's tone, at its pitch").toBeGreaterThan(TONE_HZ * 0.95);
  expect(heard.hz).toBeLessThan(TONE_HZ * 1.05);

  await page.mouse.move(640, 5);
  await mic.click();
  await expect.poll(micState, { message: 'the switch turns off' }).toBe('off');
  expect(await localSDP(), 'switching the microphone on and off renegotiated nothing').toBe(offered);
  await expect
    .poll(async () => toneOf(pcm(await host.recordMicrophone(1.5))).loud, { message: 'silence on the host again', timeout: 15_000, intervals: [0] })
    .toBeLessThan(0.05);
});

test('camera: a spawned desktop without v4l2loopback says why, and the page offers no camera', async () => {
  test.skip(!DEVICES, 'this scenario offers no camera');
  expect(cli!.stderrTail(Infinity)).toContain('no camera on robinhost: a spawned desktop takes the camera only through v4l2loopback');
  await expect(page.locator('button.device[data-device="camera"]')).toHaveCount(0);
});

// Files and the rich clipboard run where the picture can be clicked: in
// direct with a spawned desktop, whose xterm has robin-send on its PATH,
// and in attach, where robin-agent send finds the one session by itself.
const FILES = ['direct', 'attach'].includes(SCENARIO) && PICTURE;
const NO_FILES = 'files run in direct and attach, where the xterm can be clicked';
const sha256 = (data: Buffer | Uint8Array) => crypto.createHash('sha256').update(data).digest('hex');
// A 4x3 PNG, small enough to type, real enough for any image reader.
const PNG_B64 = 'iVBORw0KGgoAAAANSUhEUgAAAAQAAAADCAYAAAC09K7GAAAAFUlEQVR4nGP4z8DwHwwZ/oMBA0EBAM2BHeM8fgyIAAAAAElFTkSuQmCC';

// typeCommand clicks the xterm through the picture and runs command there.
async function typeCommand(command: string) {
  let term: host.Rect | null = null;
  await expect.poll(async () => (term = await host.terminal()), { message: 'an xterm is mapped', timeout: 15_000 }).not.toBeNull();
  const t = term!;
  const centre = await pagePoint(page, SCREEN, t.x + t.width / 2, t.y + t.height / 2);
  await page.mouse.click(centre.x, centre.y);
  await page.keyboard.type(command, { delay: 15 });
  await page.keyboard.press('Enter');
}

// offerFromHost offers file to the page as a program on the host would:
// robin-send typed in the session's xterm, or in attach, where the desktop
// was not started by the agent, robin-agent send over ssh. It resolves
// with what the command printed and its exit status.
async function offerFromHost(file: string): Promise<string> {
  const out = `/tmp/robin-e2e-send-${Date.now()}.out`;
  if (SCENARIO === 'attach') {
    const r = await host.sshRaw(`"$(ls ~/.cache/robin/robin-agent-* | head -n1)" send ${host.shellQuote(file)}; echo "exit $?"`, 60_000);
    return r.stdout + r.stderr;
  }
  await typeCommand(`robin-send ${host.shellQuote(file)} > ${out} 2>&1; echo "exit $?" >> ${out}`);
  let printed = '';
  await expect
    .poll(async () => (printed = (await host.sshRaw(`cat ${out}`)).stdout), { message: `${out} on the host`, timeout: 60_000 })
    .toMatch(/exit \d+/);
  return printed;
}

test('files: uploads land in the download folder and never replace a file', async () => {
  test.skip(!FILES, NO_FILES);
  await host.ssh('rm -rf ~/Downloads');
  const content = crypto.randomBytes(300_000);
  const file = { name: 'robin-e2e-upload.bin', mimeType: 'application/octet-stream', buffer: content };
  await page.locator('#files-input').setInputFiles(file);
  await expect.poll(() => host.fileSha256('~/Downloads/robin-e2e-upload.bin'), { timeout: 15_000 }).toBe(sha256(content));
  await page.locator('#files-input').setInputFiles(file);
  await expect.poll(() => host.fileSha256('~/Downloads/robin-e2e-upload (1).bin'), { timeout: 15_000 }).toBe(sha256(content));
  await expect(page.locator('.transfer[data-dir="up"] .status').filter({ hasText: 'Uploaded as robin-e2e-upload (1).bin' })).toHaveCount(1);

  // A drop anywhere on the page uploads too.
  await page.evaluate(() => {
    const dt = new DataTransfer();
    dt.items.add(new File(['dropped on the page'], 'robin-e2e-drop.txt', { type: 'text/plain' }));
    window.dispatchEvent(new DragEvent('dragover', { dataTransfer: dt, cancelable: true }));
    window.dispatchEvent(new DragEvent('drop', { dataTransfer: dt, cancelable: true }));
  });
  await expect
    .poll(async () => (await host.sshRaw('cat ~/Downloads/robin-e2e-drop.txt')).stdout, { timeout: 15_000 })
    .toBe('dropped on the page');
  // Only the files: no partial file, nothing outside the folder.
  expect((await host.ssh('ls -A ~/Downloads')).split('\n').sort()).toEqual(
    ['robin-e2e-drop.txt', 'robin-e2e-upload (1).bin', 'robin-e2e-upload.bin'],
  );
});

test('files: the host offers a file, shown as text, saved only on a click', async () => {
  test.skip(!FILES, NO_FILES);
  // Markup in the name must show as the name, not as markup.
  const name = '<b>robin<b> & co.bin';
  const path = `/tmp/${name}`;
  await host.ssh(`head -c 200000 /dev/urandom > ${host.shellQuote(path)}`);
  const want = (await host.ssh(`sha256sum ${host.shellQuote(path)}`)).split(' ')[0];
  let downloads = 0;
  page.on('download', () => downloads++);

  const sending = offerFromHost(path);
  const row = page.locator('.transfer[data-dir="down"]');
  await expect(row).toHaveCount(1, { timeout: 20_000 });
  await expect(row.locator('.name')).toHaveText(name);
  expect(await row.locator('.name').evaluate((el) => el.children.length), 'elements inside the name').toBe(0);
  await expect(row.locator('.size')).toHaveText('195.3 KiB');
  await expect(row.locator('.status')).toHaveText('Offered by the host');
  // The tab stays out while the offer waits.
  await page.waitForTimeout(3_500);
  await expect(page.locator('#hud')).not.toHaveClass(/tucked/);
  expect(downloads, 'downloads before any click').toBe(0);

  const [download] = await Promise.all([page.waitForEvent('download'), row.locator('.save').click()]);
  // The browser makes the name safe for the client's disk: Chrome turns
  // < and > into _.
  expect(download.suggestedFilename()).toMatch(/^.b.robin.b. & co\.bin$/);
  const saved = await download.path();
  expect(sha256(fs.readFileSync(saved!)), 'the downloaded file').toBe(want);
  const printed = await sending;
  expect(printed).toContain(`${name}: saved by the browser`);
  expect(printed).toContain('exit 0');

  // Dismissed: the host hears it, and nothing downloads.
  const again = offerFromHost(path);
  await expect(row).toHaveCount(1, { timeout: 20_000 });
  await row.locator('.dismiss').click();
  const declined = await again;
  expect(declined).toContain(`${name}: declined in the browser`);
  expect(declined).toContain('exit 1');
  expect(downloads).toBe(1);
});

test('clipboard: an image and HTML pasted in the page reach the host, an image copied there reaches the page', async () => {
  test.skip(!FILES, NO_FILES);
  const png = Buffer.from(PNG_B64, 'base64');
  const html = `<b>robin-e2e</b> ${Date.now()}`;
  // Firefox builds a ClipboardEvent's clipboardData afresh and keeps none of
  // what a script passes it, so there the DataTransfer goes in directly.
  const asEvent = process.env.ROBIN_E2E_BROWSER !== 'firefox';
  await page.evaluate(({ b64, html, asEvent }) => {
    const dt = new DataTransfer();
    dt.setData('text/html', html);
    dt.items.add(new File([Uint8Array.from(atob(b64), (c) => c.charCodeAt(0))], 'image.png', { type: 'image/png' }));
    if (asEvent) window.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, cancelable: true }));
    else (window as any).robin.paste(dt);
  }, { b64: PNG_B64, html, asEvent });
  await expect
    .poll(async () => (await host.sshRaw('robin-x xclip -o -selection clipboard -t image/png | sha256sum')).stdout.split(' ')[0], {
      message: 'the image on the host clipboard',
      timeout: 15_000,
    })
    .toBe(sha256(png));
  expect((await host.sshRaw('robin-x xclip -o -selection clipboard -t text/html')).stdout).toBe(html);

  // The other way: an image copied on the host.
  const other = Buffer.concat([png, Buffer.from(`robin-e2e-${Date.now()}`)]);
  await host.ssh(`echo ${other.toString('base64')} | base64 -d | robin-x xclip -selection clipboard -t image/png -i >/dev/null 2>&1`);
  await expect
    .poll(
      () =>
        page.evaluate(async () => {
          const png = (window as any).robin.lastRemoteClip?.png as Uint8Array | undefined;
          if (!png) return '';
          const digest = new Uint8Array(await globalThis.crypto.subtle.digest('SHA-256', png));
          return [...digest].map((b) => b.toString(16).padStart(2, '0')).join('');
        }),
      { message: 'robin.lastRemoteClip.png', timeout: 15_000 },
    )
    .toBe(sha256(other));
});

if (SCENARIO === 'direct') {
  test('transport: direct session uses UDP', async ({}, testInfo) => {
    expect((await connectedStats(testInfo, 'direct')).transport).toBe('udp');
  });
}

if (SCENARIO === 'blocked' || SCENARIO === 'nomux') {
  test('transport: with UDP blocked the session runs on ICE-TCP through the SSH forward', async ({}, testInfo) => {
    const s = await connectedStats(testInfo, SCENARIO);
    expect(s.transport).toBe('tcp');
    expect(s.remote.address).toBe('127.0.0.1');
  });
}

if (SCENARIO === 'nomux') {
  test('nomux: no ControlMaster; the agent and the forward each have an ssh of their own', async ({}, testInfo) => {
    const ssh = sshProcesses();
    await testInfo.attach('ssh processes', { body: ssh.join('\n'), contentType: 'text/plain' });
    expect(ssh.filter((a) => a.includes('ControlMaster=yes')), 'a ControlMaster').toEqual([]);
    expect(ssh.filter((a) => / -N .*-L \/\S+\/fwd\d+\.sock:127\.0\.0\.1:\d+ /.test(a)), 'the forward, on a socket in a private directory').toHaveLength(1);
    expect(ssh.filter((a) => / serve( |$)/.test(a)), 'the agent').toHaveLength(1);
    expect(ssh.every((a) => a.includes('ControlPath=none')), 'every ssh keeps clear of any master in the config').toBe(true);
  });
}

test('ime: text an input method commits types on the host, characters outside the keymap included', async () => {
  test.skip(!PICTURE, NO_PICTURE);
  test.skip(process.env.ROBIN_E2E_BROWSER !== 'chrome', "input methods are driven through Chrome's DevTools protocol");
  const file = `/tmp/robin-e2e-ime-${SCENARIO}`;
  await host.ssh(`rm -f ${file}`);
  let term: host.Rect | null = null;
  await expect.poll(async () => (term = await host.terminal()), { message: 'an xterm is mapped', timeout: 15_000 }).not.toBeNull();
  const t = term!;
  const centre = await pagePoint(page, SCREEN, t.x + t.width / 2, t.y + t.height / 2);
  await page.mouse.click(centre.x, centre.y);
  expect(await page.evaluate(() => document.activeElement?.id), 'the text box holds the focus').toBe('typing');

  const cdp = await context.newCDPSession(page);
  await page.keyboard.type('echo ', { delay: 20 });
  // What Chrome does for an input method: Process keys, a composition that
  // changes, and the text it commits.
  await cdp.send('Input.dispatchKeyEvent', { type: 'rawKeyDown', key: 'Process', code: 'KeyN', windowsVirtualKeyCode: 229 });
  await cdp.send('Input.imeSetComposition', { text: 'にほんご', selectionStart: 4, selectionEnd: 4 });
  await cdp.send('Input.dispatchKeyEvent', { type: 'keyUp', key: 'Process', code: 'KeyN', windowsVirtualKeyCode: 229 });
  await expect(page.locator('#preedit')).toHaveText('にほんご');
  await cdp.send('Input.insertText', { text: '日本語ü' });
  await expect(page.locator('#preedit')).toBeHidden();
  await cdp.detach();
  await page.keyboard.type(` > ${file}`, { delay: 20 });
  await page.keyboard.press('Enter');
  await expect
    .poll(async () => (await host.sshRaw(`cat ${file}`)).stdout.trim(), { message: `${file} on the host`, timeout: 10_000 })
    .toBe('日本語ü');
});

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

// A kept session: what the desktop looks like from the host, to compare
// across a reconnect.
type Kept = { session: host.Session; xvfb: number; xterm: string };

async function keptState(): Promise<Kept> {
  const [session, xvfb, xterm] = await Promise.all([host.session(), host.xvfbPid(), host.terminalWindow()]);
  return { session, xvfb, xterm };
}

// markTerminal types a marker command into the xterm, which leaves the
// command line on screen, and waits for the picture to settle around it.
async function markTerminal(marker: string) {
  await typeIntoTerminal(`echo ${marker} > /tmp/${marker}`, `/tmp/${marker}`, marker);
  await waitForStillFrame(page, SCREEN);
}

// expectSameDesktop checks the desktop that answers is the one from before:
// same daemon, Xvfb and xterm, and the marker's file still there.
async function expectSameDesktop(before: Kept, marker: string) {
  const after = await keptState();
  expect(after.session.id, 'session id').toBe(before.session.id);
  expect(after.session.pid, 'daemon pid').toBe(before.session.pid);
  expect(after.xvfb, 'Xvfb pid').toBe(before.xvfb);
  expect(after.xterm, 'xterm window').toBe(before.xterm);
  expect(await host.exists(`/tmp/${marker}`), `/tmp/${marker} on the host`).toBe(true);
}

if (SCENARIO === 'resume') {
  test('resume: SSH cut mid-session; the CLI reconnects to the same desktop and the xterm still shows the marker', async ({}, testInfo) => {
    testInfo.setTimeout(120_000);
    const before = await keptState();
    expect(before.session.connected, 'session.json says connected').toBe(true);
    expect(before.session.keep, 'keep window').toBe(8 * 3600);
    expect(before.xvfb, 'Xvfb pid').toBeGreaterThan(0);
    expect(before.xterm, 'xterm window').not.toBe('');
    const marker = 'robin-e2e-resume-marker';
    await markTerminal(marker);
    let term: host.Rect | null = null;
    await expect.poll(async () => (term = await host.terminal())).not.toBeNull();
    await sampleFrame(page, SCREEN, { save: 'pre-cut' });

    await host.cutSSH();
    const cutAt = Date.now();
    const seen: string[] = [];
    const watch = async () => {
      const st = (await state(page)) ?? 'none';
      if (seen.at(-1) !== st) seen.push(st);
      return st;
    };
    await expect.poll(watch, { message: 'the page is told the CLI is reconnecting', timeout: 30_000, intervals: [100] }).toBe('reconnecting');
    const noticedAfterMs = Date.now() - cutAt;
    await expect.poll(watch, { message: 'the page is connected again', timeout: 60_000, intervals: [250] }).toBe('connected');
    testInfo.annotations.push({
      type: 'recovery',
      description: `noticed after ${noticedAfterMs} ms, connected after ${Date.now() - cutAt} ms via ${seen.join(' -> ')}`,
    });
    expect(cli!.running, 'the CLI is still running').toBe(true);
    expect(cli!.stderrTail(), 'the CLI said it reconnected').toContain('reconnected to robinhost');

    await expectSameDesktop(before, marker);
    expect((await host.session()).connected, 'session.json says connected again').toBe(true);
    // The picture is the same xterm with the same text: the marker line is
    // still on screen.
    await waitForStillFrame(page, SCREEN);
    const diff = await sampleFrame(page, SCREEN, { compare: 'pre-cut', region: term! });
    testInfo.annotations.push({ type: 'xterm diff', description: `${diff.changed} of ${diff.compared} pixels changed` });
    expect(diff.changed, 'pixels changed in the xterm across the reconnect').toBeLessThan(CHANGED_FLOOR);
    // And input still works.
    await typeIntoTerminal('echo robin-e2e-after > /tmp/robin-e2e-resume-after', '/tmp/robin-e2e-resume-after', 'robin-e2e-after');
  });
}

if (SCENARIO === 'rejoin') {
  test('rejoin: the CLI is killed; a new robin gui resumes the kept desktop, detaches, and robin end ends it', async ({}, testInfo) => {
    testInfo.setTimeout(150_000);
    const before = await keptState();
    const marker = 'robin-e2e-rejoin-marker';
    await markTerminal(marker);

    // The CLI dies without a word, ssh master and all.
    const exit = await cli!.kill();
    testInfo.annotations.push({ type: 'kill', description: JSON.stringify(exit) });
    await expect.poll(() => state(page), { message: 'the page ends with its CLI', timeout: 15_000 }).toBe('ended');
    await expect
      .poll(async () => (await host.session()).connected, { message: 'session.json says kept', timeout: 30_000 })
      .toBe(false);
    const kept = await host.session();
    expect(kept.until, 'the keep window has an end').toBeTruthy();
    expect(await host.xvfbPid(), 'Xvfb outlives the CLI').toBe(before.xvfb);

    const ls = await robin(['ls', 'robinhost']);
    testInfo.annotations.push({ type: 'robin ls', description: ls.stdout + ls.stderr });
    expect(ls.code, 'robin ls exit code').toBe(0);
    expect(ls.stdout).toContain(before.session.id);
    expect(ls.stdout).toContain('kept until');

    // A new robin gui finds the one kept session and resumes it.
    cli = await Cli.start(OUT);
    // stderr reaches the log a moment after stdout printed the URL.
    const resumed = cli;
    await expect.poll(() => resumed.stderrTail(), { message: 'the CLI said it resumed' }).toContain(`resumed session ${before.session.id}`);
    await page.goto(cli.url);
    await expect.poll(() => state(page), { message: 'the new page connects', timeout: 60_000 }).toBe('connected');
    await expectSameDesktop(before, marker);
    await typeIntoTerminal('echo robin-e2e-after > /tmp/robin-e2e-rejoin-after', '/tmp/robin-e2e-rejoin-after', 'robin-e2e-after');

    // Disconnect, keep desktop: the CLI exits on its own, the page says so,
    // the host keeps the session.
    await page.mouse.move(640, 5);
    const detach = page.locator('#detach');
    await expect(detach, 'the detach button').toBeVisible({ timeout: 10_000 });
    await detach.click();
    const left = await cli.waitExit(15_000);
    testInfo.annotations.push({ type: 'detach exit', description: JSON.stringify(left) });
    expect(left, 'the CLI exited after the page detached').not.toBeNull();
    expect(left!.code, 'CLI exit code').toBe(0);
    expect(cli.stderrTail()).toContain('desktop kept on robinhost');
    await expect(page.locator('#state-label')).toHaveText('Desktop kept');
    expect((await host.session()).connected, 'session.json says kept').toBe(false);
    expect(await host.xvfbPid(), 'Xvfb still there').toBe(before.xvfb);

    const end = await robin(['end', 'robinhost']);
    testInfo.annotations.push({ type: 'robin end', description: end.stdout + end.stderr });
    expect(end.code, 'robin end exit code').toBe(0);
    expect(end.stderr).toContain(`ended session ${before.session.id}`);
    await expect.poll(host.leftovers, { message: 'session processes on the host', timeout: 10_000 }).toBe('');
    expect(await host.agentLeftovers(), 'agents and run dirs').toBe('');
    expect(cli.extraStdout, 'stdout carries only the URL').toEqual([]);
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
    // The video shows the primary monitor of robin-live's two.
    expect(`video ${v.w}x${v.h}, display ${g.width}x${g.height}`).toBe(
      `video 1280x800, display ${desk.width}x${desk.height}`,
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

  // robin-live runs two monitors: LEFT 1280x800 (primary) and RIGHT 1024x768
  // beside it on a 2304x800 screen.
  test('attach: the page shows the primary monitor, switches to the other and to all, and points on the one shown', async () => {
    const monitors = () => page.evaluate(() => (window as any).robin.monitors);
    const videoSize = () => page.$eval(SCREEN, (el) => `${(el as HTMLVideoElement).videoWidth}x${(el as HTMLVideoElement).videoHeight}`);
    await expect.poll(async () => (await monitors())?.current, { message: 'the page was told the monitors' }).toBe('LEFT');
    const listed = (await monitors()).list
      .map((m: any) => `${m.name} ${m.w}x${m.h}+${m.x}+${m.y}${m.primary ? ' primary' : ''}`)
      .sort();
    expect(listed).toEqual(['LEFT 1280x800+0+0 primary', 'RIGHT 1024x768+1280+0']);

    const pick = async (name: string) => {
      await page.mouse.move(300, 4); // near the top edge, where the status tab comes out
      const button = page.locator('#monitor');
      await expect(button).toBeVisible();
      await button.click();
      await expect(page.locator('#monitor-menu [role="menuitemradio"]')).toHaveCount(3);
      await page.locator(`#monitor-menu [data-name="${name}"]`).click();
      await expect(page.locator('#monitor-menu')).toBeHidden();
    };
    const pointerLands = async (vx: number, vy: number, want: { x: number; y: number }) => {
      const p = await pagePoint(page, SCREEN, vx, vy);
      await page.mouse.move(p.x, p.y);
      await expect
        .poll(async () => {
          const m = await host.mouseLocation();
          return Math.abs(m.x - want.x) <= 2 && Math.abs(m.y - want.y) <= 2 ? 'there' : `${m.x},${m.y}`;
        }, { message: `pointer near ${want.x},${want.y}` })
        .toBe('there');
    };

    await expect(page.locator('#monitor .label')).toHaveText('LEFT');
    await pick('RIGHT');
    await expect.poll(videoSize, { message: 'the video takes RIGHT\'s size' }).toBe('1024x768');
    await expect.poll(async () => (await monitors())?.current).toBe('RIGHT');
    await pointerLands(100, 100, { x: 1380, y: 100 });

    await pick('all');
    // 2304x800 shrinks to fit the --max-size 1920x1080 run.sh gives this
    // scenario.
    await expect.poll(videoSize, { message: 'the video shows the whole screen, scaled' }).toMatch(/^19(18|20)x666$/);
    await expect(page.locator('#monitor .label')).toHaveText('All monitors');

    await pick('LEFT');
    await expect.poll(videoSize).toBe('1280x800');
    await pointerLands(100, 100, { x: 100, y: 100 });
  });

  test('private: the CLI tells the user the room sees the session, and how to stop that', async () => {
    expect(cli!.stderrTail()).toContain('--curtain blacks out the screen and --silent keeps the speakers quiet');
  });

  // flip clicks a private session switch in the status tab and waits for
  // the agent to report it switched.
  const flip = async (key: 'curtain' | 'silent', on: boolean) => {
    await page.mouse.move(300, 4); // near the top edge, where the status tab comes out
    const sw = page.locator(`[data-switch="${key}"]`);
    await expect(sw, `the ${key} switch`).toBeVisible();
    await sw.click();
    await expect(sw, `the ${key} switch, as the agent reports it`).toHaveAttribute('aria-checked', String(on), { timeout: 10_000 });
    await expect(sw).toHaveAttribute('aria-busy', 'false');
    await expect(page.locator('.switch-error'), 'an error under the switches').toHaveCount(0);
  };

  test('private: the curtain blacks out the host screen while the page still shows it', async () => {
    await expect
      .poll(() => page.evaluate(() => (window as any).robin.private), { message: 'the page was told the private session' })
      .toEqual({ curtain: {}, silent: {} });
    expect(await host.roomBrightness(), 'the host screen before').toBeGreaterThan(0.5);
    await flip('curtain', true);
    await expect.poll(host.roomBrightness, { message: 'the host screen with the curtain drawn' }).toBe(0);
    if (PICTURE) {
      await host.retitleTerminal(); // something new to draw, past the curtain
      await expect.poll(async () => isNonBlack(await sampleFrame(page, SCREEN)), { message: 'the picture in the page' }).toBe(true);
    }
    await flip('curtain', false);
    await expect.poll(host.roomBrightness, { message: 'the host screen once lifted' }).toBeGreaterThan(0.5);
  });

  test('private: the silent room keeps the host speakers quiet while the page still plays the sound', async ({}, testInfo) => {
    const energy = () =>
      page.evaluate(async () => {
        let e = 0;
        (await ((window as any).robin.pc as RTCPeerConnection).getStats()).forEach((s: any) => {
          if (s.type === 'inbound-rtp' && s.kind === 'audio') e = s.totalAudioEnergy ?? 0;
        });
        return e;
      });
    expect(await page.evaluate(() => (window as any).robin.muted), 'the speaker, on since the sound test').toBe(false);
    await flip('silent', true);
    const before = await energy();
    const silenced = host.playNoise(12);
    await expect.poll(energy, { message: 'totalAudioEnergy grows while the host plays noise', timeout: 15_000 }).toBeGreaterThan(before);
    const quiet = await host.roomLoudness();
    await flip('silent', false);
    // Noise of its own for the second reading, which the first may outlast
    // the end of on a slow run.
    const heard = host.playNoise(5);
    let loud = 0;
    await expect
      .poll(async () => (loud = await host.roomLoudness()), { message: 'the host speakers once the room is no longer silent', timeout: 10_000 })
      .toBeGreaterThan(1000);
    testInfo.annotations.push({ type: 'room RMS', description: `silent ${quiet}, after ${loud}` });
    expect(quiet, 'the host speakers while silent').toBeLessThan(50);
    await Promise.all([silenced, heard]);
    // Both on for the teardown below, which must give the room its screen
    // and speakers back.
    await flip('curtain', true);
    await flip('silent', true);
    expect(await host.roomBrightness(), 'the host screen before teardown').toBe(0);
  });

  test('teardown: SIGINT ends the CLI and leaves the attached desktop as it found it', async ({}, testInfo) => {
    expect(await host.agentLeftovers(), 'the agent is visible before teardown').not.toBe('');
    const stop = await cli!.stop(10_000);
    testInfo.annotations.push({ type: 'exit', description: JSON.stringify(stop) });
    expect(stop.killed, 'CLI exited by itself within 10 s of SIGINT').toBe(false);
    expect(stop.code, 'CLI exit code').toBe(0);

    const left = Math.max(1_000, 10_000 - stop.afterMs);
    await expect.poll(host.agentLeftovers, { message: 'agent processes and run dirs on the host', timeout: left }).toBe('');
    // Same Xvfb, openbox and xterm, same size, autorepeat, keymaps, gamma and sinks.
    expect(await host.liveState()).toBe(await host.liveBefore());
    expect(cli!.extraStdout, 'stdout carries only the URL').toEqual([]);
  });
}

if (SCENARIO === 'direct') {
  // showKeys brings the status tab out and opens the keys section folded
  // in it.
  const showKeys = async () => {
    await page.mouse.move(640, 5);
    const toggle = page.locator('#keys');
    if ((await toggle.getAttribute('aria-expanded')) !== 'true') await toggle.click();
    await expect(page.locator('#hud').getByRole('button', { name: 'Ctrl+Alt+Delete', exact: true })).toBeVisible();
  };
  // macrosFile is where the CLI under test keeps the macros.
  const macrosFile = () => path.join(process.env.XDG_CONFIG_HOME || path.join(os.homedir(), '.config'), 'robin', 'macros.json');

  test('macros: a macro recorded in the page outlives a reload, kept by the CLI, and plays into the xterm', async ({}, testInfo) => {
    testInfo.setTimeout(120_000);
    const n = Date.now().toString(36);
    const file = `/tmp/robin-e2e-macro-${n}`;
    const marker = '/tmp/robin-e2e-macro-T';
    const sentinel = `/tmp/robin-e2e-macro-after-${n}`;
    await host.ssh(`rm -f ${file} ${marker} ${sentinel}`);
    // A client container kept from an earlier run keeps its macros too.
    const saved = macrosFile();
    fs.rmSync(saved, { force: true });
    await expect.poll(host.terminal, { message: 'an xterm is mapped', timeout: 15_000 }).not.toBeNull();
    await host.focusTerminal();
    const hud = page.locator('#hud');
    await showKeys();
    await hud.getByRole('button', { name: 'New macro', exact: true }).click();
    await hud.getByLabel('Macro name').fill('E2E touch');
    await hud.getByLabel('Text to type').fill(`echo robin-macro-${n} > ${file}; `);
    await hud.getByRole('button', { name: 'Add text', exact: true }).click();

    // While recording, keys go into the macro and never to the xterm. The
    // typed letters join the text before them; Enter is a key of its own.
    await hud.getByRole('button', { name: 'Record keys', exact: true }).click();
    await page.keyboard.type(`touch ${marker.slice(0, -1)}`, { delay: 20 });
    await page.keyboard.press('Shift+KeyT');
    await page.keyboard.press('Enter');
    await hud.getByRole('button', { name: 'Stop recording', exact: true }).click();
    const steps = await hud.locator('.steps li span').allTextContents();
    expect(steps.length, `the steps ${JSON.stringify(steps)}`).toBe(2);
    expect(steps[0], 'the text step').toMatch(/^“echo robin-macro-.*…”$/);
    expect(steps[1], 'the last chord').toBe('Enter');

    await hud.getByRole('button', { name: 'Save', exact: true }).click();
    const play = hud.getByRole('button', { name: 'E2E touch', exact: true });
    await expect(play, 'the saved macro').toBeVisible();
    await expect(hud.locator('.macro-editor')).toBeHidden();

    // The CLI wrote it, private, with the text in plain text as the page
    // said it would be.
    expect(fs.statSync(saved).mode & 0o777, 'macros.json mode').toBe(0o600);
    expect(fs.statSync(path.dirname(saved)).mode & 0o777, 'its directory mode').toBe(0o700);
    const kept = JSON.parse(fs.readFileSync(saved, 'utf8'));
    expect(kept.version).toBe(1);
    expect(kept.macros.map((m: { name: string }) => m.name)).toEqual(['E2E touch']);
    expect(kept.macros[0].steps[0]).toEqual({ text: `echo robin-macro-${n} > ${file}; touch ${marker}` });

    // WebKit's Linux build crashes the page on some reloads, the more
    // often the longer the page has stood, with this page and the one
    // before it alike. What a reload shows here is the CLI's doing, the
    // same in every browser, so WebKit plays the macro without one.
    if (process.env.ROBIN_E2E_BROWSER !== 'webkit') {
      await page.reload();
      await expect.poll(() => state(page), { message: 'connected after the reload', timeout: 20_000 }).toBe('connected');
      await expect(page.locator('#keys'), 'a new page starts with the keys section folded').toHaveAttribute('aria-expanded', 'false');
      await showKeys();
      await expect(play, 'the macro after a reload').toBeVisible({ timeout: 10_000 });
    }

    // Anything the recording let through would have reached the xterm
    // before a line typed now does.
    await host.focusTerminal();
    await page.keyboard.type(`touch ${sentinel}`, { delay: 20 });
    await page.keyboard.press('Enter');
    await expect.poll(() => host.exists(sentinel), { message: `${sentinel} on the host`, timeout: 10_000 }).toBe(true);
    expect(await host.exists(marker), 'the recorded keys reached the xterm').toBe(false);
    const t0 = Date.now();
    await play.click();
    await expect
      .poll(async () => (await host.sshRaw(`cat ${file}`)).stdout.trim(), { message: `${file} on the host`, timeout: 15_000 })
      .toBe(`robin-macro-${n}`);
    await expect.poll(() => host.exists(marker), { message: `${marker} on the host`, timeout: 5_000 }).toBe(true);
    testInfo.annotations.push({ type: 'macro played', description: `${steps.length} steps in ${Date.now() - t0} ms` });
  });

  test('keys: Ctrl+W from the keys section reaches the xterm instead of closing the tab', async () => {
    const file = `/tmp/robin-e2e-ctrlw-${Date.now().toString(36)}`;
    await host.focusTerminal();
    await page.keyboard.type('echo robin-kept robin-gone', { delay: 20 });
    await showKeys();
    await page.locator('#hud').getByRole('button', { name: 'Ctrl+W', exact: true }).click();
    await page.keyboard.type(` > ${file}`, { delay: 20 });
    await page.keyboard.press('Enter');
    await expect
      .poll(async () => (await host.sshRaw(`cat ${file}`)).stdout.trim(), { message: `${file} on the host`, timeout: 10_000 })
      .toBe('robin-kept');
    expect(await state(page), 'the tab is still open and connected').toBe('connected');
  });

  test('keys: Type clipboard types the local clipboard into the xterm, as keys', async ({}, testInfo) => {
    test.skip(process.env.ROBIN_E2E_BROWSER !== 'chrome', 'Playwright grants clipboard reads in Chrome only');
    const file = `/tmp/robin-e2e-typed-${Date.now().toString(36)}`;
    await context.grantPermissions(['clipboard-read', 'clipboard-write']);
    const line = `echo robin-typed-${'x'.repeat(200)} > ${file}`;
    await page.evaluate((t) => navigator.clipboard.writeText(t), `${line}\n`);
    await host.focusTerminal();
    await showKeys();
    const t0 = Date.now();
    await page.locator('#hud').getByRole('button', { name: 'Type clipboard', exact: true }).click();
    await expect
      .poll(async () => (await host.sshRaw(`cat ${file}`)).stdout.trim(), { message: `${file} on the host`, timeout: 15_000 })
      .toBe(`robin-typed-${'x'.repeat(200)}`);
    testInfo.annotations.push({ type: 'clipboard typed', description: `${line.length + 1} characters in ${Date.now() - t0} ms` });
  });

  test('end: the End session button ends the desktop on the host, and the page is told once it did', async ({}, testInfo) => {
    testInfo.setTimeout(60_000);
    const dir = await host.runDir();
    expect(await host.leftovers(), 'session processes are visible before the end').not.toBe('');
    await page.mouse.move(640, 5);
    const end = page.locator('#end');
    await expect(end, 'the end button').toBeVisible({ timeout: 10_000 });
    await end.click();
    const exit = await cli!.waitExit(15_000);
    testInfo.annotations.push({ type: 'end exit', description: JSON.stringify(exit) });
    expect(exit, 'the CLI exited after the page ended the session').not.toBeNull();
    expect(exit!.code, 'CLI exit code').toBe(0);
    await expect.poll(() => state(page), { message: 'the page says the session ended' }).toBe('ended');
    await expect(page.locator('#state-label')).toHaveText('Session ended');
    await expect(page.locator('#hud .notice')).toContainText('the desktop on the host is gone');
    await expect.poll(host.leftovers, { message: 'session processes on the host', timeout: 10_000 }).toBe('');
    await expect.poll(() => host.exists(dir), { message: `run dir ${dir} exists`, timeout: 1_000 }).toBe(false);
    expect(await host.agentLeftovers(), 'agents and run dirs').toBe('');
    expect(cli!.extraStdout, 'stdout carries only the URL').toEqual([]);
  });
}

test('teardown: SIGINT ends the CLI and leaves nothing behind on the host', async ({}, testInfo) => {
  test.skip(SCENARIO === 'attach', 'the attached desktop outlives the session; see the attach teardown');
  test.skip(SCENARIO === 'rejoin', 'robin end tore the session down; see the rejoin test');
  test.skip(SCENARIO === 'direct', 'the page ended the session; see the end test');
  const dir = await host.runDir();
  expect(await host.leftovers(), 'session processes are visible before teardown').not.toBe('');

  const stop = await cli!.stop(10_000);
  testInfo.annotations.push({ type: 'exit', description: JSON.stringify(stop) });
  expect(stop.killed, 'CLI exited by itself within 10 s of SIGINT').toBe(false);
  expect(stop.code, 'CLI exit code').toBe(0);

  const left = Math.max(1_000, 10_000 - stop.afterMs);
  await expect.poll(host.leftovers, { message: 'session processes on the host', timeout: left }).toBe('');
  await expect.poll(() => host.exists(dir), { message: `run dir ${dir} exists`, timeout: 1_000 }).toBe(false);
  await expect.poll(sshProcesses, { message: "the CLI's ssh processes on the client", timeout: 5_000 }).toEqual([]);
  expect(cli!.extraStdout, 'stdout carries only the URL').toEqual([]);
  testInfo.annotations.push({ type: 'page state after teardown', description: String(await state(page)) });
});
