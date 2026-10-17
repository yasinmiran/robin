import { test, expect, chromium, type Page } from '@playwright/test';
import { clientPoint, videoBox } from './helpers/coords';
import { CHANGED_FLOOR, isNonBlack, sampleFrame, waitForStillFrame } from './helpers/frames';
import * as host from './helpers/host';

// Proves the harness without the CLI: the browser decodes WebRTC H.264, the
// frame and coordinate helpers measure what they claim, and the host helpers
// work against a display started by hand the way the agent would.

const h264 = (page: Page) =>
  page.evaluate(() =>
    (RTCRtpReceiver.getCapabilities('video')?.codecs ?? [])
      .filter((c) => c.mimeType.toLowerCase() === 'video/h264')
      .map((c) => c.sdpFmtpLine ?? ''),
  );

test('Chrome lists H.264 (Constrained Baseline, packetization-mode 1) for WebRTC receive', async ({ page, browser }, testInfo) => {
  const fmtps = await h264(page);
  testInfo.annotations.push({ type: `${browser.browserType().name()} ${browser.version()} H.264`, description: fmtps.join(' | ') });
  expect(fmtps.some((f) => /profile-level-id=42e0/i.test(f) && /packetization-mode=1/.test(f))).toBe(true);

  const bundled = await chromium.launch();
  try {
    const p = await bundled.newPage();
    const n = (await h264(p)).length;
    testInfo.annotations.push({ type: `bundled Chromium ${bundled.version()} H.264 entries`, description: String(n) });
  } finally {
    await bundled.close();
  }
});

// Receives an H.264 track from a canvas in the same page, shaped like a
// robin session: the receiving side offers recvonly with H.264 preferred.
async function startLoopback(page: Page) {
  await page.setContent(`<!doctype html>
    <body style="margin:0;background:#222">
      <canvas id="src" width="640" height="360" style="display:none"></canvas>
      <video id="screen" autoplay muted playsinline style="width:800px;height:600px"></video>
    </body>`);
  await page.evaluate(async () => {
    const g = window as any;
    const canvas = document.getElementById('src') as HTMLCanvasElement;
    const ctx = canvas.getContext('2d')!;
    let t = 0;
    g.animate = true;
    g.paint = (text: string) => {
      ctx.fillStyle = '#fff';
      ctx.fillRect(0, 0, 640, 360);
      ctx.fillStyle = '#000';
      ctx.font = '13px monospace';
      ctx.fillText(text, 20, 40);
      ctx.fillStyle = '#c33';
      ctx.fillRect((t * 11) % 600, 200, 40, 40);
    };
    // A timer, not requestAnimationFrame: headless pages may throttle rAF.
    setInterval(() => g.animate && g.paint(`robin-e2e frame ${++t}`), 33);
    g.paint('robin-e2e');

    const track = canvas.captureStream(30).getVideoTracks()[0];
    const sender = new RTCPeerConnection();
    const receiver = new RTCPeerConnection();
    sender.onicecandidate = (e) => e.candidate && receiver.addIceCandidate(e.candidate);
    receiver.onicecandidate = (e) => e.candidate && sender.addIceCandidate(e.candidate);
    receiver.ontrack = (e) => {
      (document.getElementById('screen') as HTMLVideoElement).srcObject = new MediaStream([e.track]);
    };

    const rx = receiver.addTransceiver('video', { direction: 'recvonly' });
    rx.setCodecPreferences(RTCRtpReceiver.getCapabilities('video')!.codecs.filter((c) => c.mimeType === 'video/H264'));
    await receiver.setLocalDescription();
    await sender.setRemoteDescription(receiver.localDescription!);
    const tx = sender.getTransceivers()[0];
    tx.direction = 'sendonly';
    await tx.sender.replaceTrack(track);
    await sender.setLocalDescription();
    await receiver.setRemoteDescription(sender.localDescription!);
    g.receiver = receiver;
  });
}

const inbound = (page: Page) =>
  page.evaluate(async () => {
    const report: RTCStatsReport = await (window as any).receiver.getStats();
    let rtp: any;
    report.forEach((s: any) => {
      if (s.type === 'inbound-rtp' && s.kind === 'video') rtp = s;
    });
    return { framesDecoded: rtp?.framesDecoded ?? 0, codec: rtp?.codecId ? report.get(rtp.codecId)?.mimeType : undefined };
  });

test('Chrome decodes an H.264 WebRTC loopback and the frame helpers measure it', async ({ page }, testInfo) => {
  await startLoopback(page);
  await expect.poll(() => page.evaluate(() => (window as any).receiver.connectionState), { timeout: 15_000 }).toBe('connected');
  await expect.poll(async () => isNonBlack(await sampleFrame(page, '#screen')), { message: 'non-black frame', timeout: 20_000 }).toBe(true);

  const first = await inbound(page);
  expect(first.codec, 'negotiated codec').toBe('video/H264');
  await expect.poll(async () => (await inbound(page)).framesDecoded, { message: 'framesDecoded grows' }).toBeGreaterThan(first.framesDecoded);

  const s = await sampleFrame(page, '#screen', { save: 'moving' });
  testInfo.annotations.push({ type: 'frame', description: JSON.stringify(s) });
  expect(s).toMatchObject({ width: 640, height: 360 });
  await page.waitForTimeout(500);
  expect((await sampleFrame(page, '#screen', { compare: 'moving' })).changed, 'animation shows as change').toBeGreaterThan(CHANGED_FLOOR);

  // Still picture: H.264 noise on a static frame must stay under the floor.
  await page.evaluate(() => {
    const g = window as any;
    g.animate = false;
    g.paint('robin-e2e $');
  });
  expect(await waitForStillFrame(page, '#screen'), 'picture settles').toBe(true);
  await sampleFrame(page, '#screen', { save: 'still' });
  await page.waitForTimeout(800);
  const noise = (await sampleFrame(page, '#screen', { compare: 'still' })).changed!;
  testInfo.annotations.push({ type: 'still noise (pixels)', description: String(noise) });
  expect(noise).toBeLessThan(CHANGED_FLOOR / 3);

  // A short line of small text, about what typing one command adds to an xterm.
  await page.evaluate(() => (window as any).paint('robin-e2e $ echo robin-e2e > /tmp/x'));
  const region = { x: 0, y: 20, width: 640, height: 30 };
  await expect
    .poll(async () => (await sampleFrame(page, '#screen', { compare: 'still', region })).changed, { message: 'typed-size text shows as change' })
    .toBeGreaterThan(CHANGED_FLOOR);

  // The element is 800x600 around a 640x360 picture: contain letterboxes it.
  const box = await videoBox(page, '#screen');
  expect(box).toMatchObject({ left: 0, top: 0, width: 800, height: 600, fit: 'contain', videoWidth: 640, videoHeight: 360 });
  expect(clientPoint(box, 0, 0)).toEqual({ x: 0.625, y: 75.625 });
  expect(clientPoint(box, 320, 180)).toEqual({ x: 400.625, y: 300.625 });
});

test('clientPoint honours object-fit', () => {
  const box = { left: 10, top: 20, width: 1000, height: 500, videoWidth: 500, videoHeight: 500 };
  expect(clientPoint({ ...box, fit: 'contain' }, 0, 0)).toEqual({ x: 260.5, y: 20.5 });
  expect(clientPoint({ ...box, fit: 'fill' }, 0, 0)).toEqual({ x: 10 + 1, y: 20 + 0.5 });
  expect(clientPoint({ ...box, fit: 'cover' }, 0, 0)).toEqual({ x: 10 + 1, y: 20 - 250 + 1 });
  expect(clientPoint({ ...box, fit: 'none' }, 0, 0)).toEqual({ x: 260 + 0.5, y: 20 + 0.5 });
});

// Starts Xvfb, openbox and xterm as robin with a run dir laid out like the agent's.
const FAKE_SESSION = String.raw`
set -e
d=$HOME/.cache/robin/run/selftest
mkdir -p "$d"
touch "$d/Xauthority"
xauth -q -f "$d/Xauthority" add :7 . "$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n')"
setsid -f Xvfb :7 -screen 0 1280x720x24 -nolisten tcp -auth "$d/Xauthority" </dev/null >"$d/xvfb.log" 2>&1
for i in $(seq 100); do [ -S /tmp/.X11-unix/X7 ] && break; sleep 0.1; done
echo :7 >"$d/display"
export DISPLAY=:7 XAUTHORITY="$d/Xauthority"
setsid -f openbox </dev/null >"$d/openbox.log" 2>&1
# A terminal mapped while openbox is still starting can stay unmapped forever.
for i in $(seq 100); do xprop -root _NET_SUPPORTING_WM_CHECK 2>/dev/null | grep -q 'window id' && break; sleep 0.1; done
setsid -f xterm </dev/null >"$d/xterm.log" 2>&1
`;

test('host helpers work against a display laid out like the agent leaves it', async () => {
  await host.ssh(`bash -c ${host.shellQuote(FAKE_SESSION)}`);
  const dir = await host.runDir();
  expect(dir).toBe('/home/robin/.cache/robin/run/selftest');
  expect(await host.displayGeometry()).toEqual({ width: 1280, height: 720 });
  await expect.poll(host.terminal, { message: 'xterm mapped', timeout: 10_000 }).not.toBeNull();
  await host.retitleTerminal();

  await host.onDisplay('xdotool mousemove 100 200');
  expect(await host.mouseLocation()).toEqual({ x: 100, y: 200 });

  await host.setClipboard("robin-clip 'quoted'");
  expect(await host.clipboard()).toBe("robin-clip 'quoted'");

  const procs = await host.leftovers();
  for (const name of ['Xvfb', 'openbox', 'xterm']) expect(procs).toContain(name);

  await host.ssh(`pkill -u robin -x xterm; pkill -u robin -x openbox; pkill -u robin -x Xvfb; rm -rf ${dir}`);
  await expect.poll(host.leftovers, { message: 'leftovers after kill', timeout: 5_000 }).toBe('');
  expect(await host.exists(dir)).toBe(false);
  expect((await host.sshRaw('robin-rundir')).code).toBe(1);
});
