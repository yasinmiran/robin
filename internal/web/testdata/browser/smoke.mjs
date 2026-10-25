// Drives the client in system Chrome against a live internal/web server.
// TestBrowserSmoke starts the server, plays the CLI's and the agent's side,
// and runs this script with the page URL, a directory for screenshots, a
// path to write the video frames its stand-in agent streams, and the launch
// link the CLI hands a browser opener.
import { chromium } from 'playwright-core';
import { mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const [url, shots, framesPath, launchURL] = process.argv.slice(2);
if (!url || !shots || !framesPath || !launchURL) {
  console.error('usage: node smoke.mjs <page url> <screenshot dir> <frames.json> <launch url>');
  process.exit(2);
}
mkdirSync(shots, { recursive: true });
const base = url.slice(0, url.indexOf('#'));

const browser = await launch();
const failures = [];
const check = (ok, what) => {
  if (!ok) failures.push(what);
  console.log(`${ok ? 'ok  ' : 'FAIL'} ${what}`);
};

try {
  writeFileSync(framesPath, JSON.stringify(await encodeDesktop()));
  await rejected('no token', base, 'This link is incomplete');
  await rejected('wrong token', `${base}#${'0'.repeat(64)}`, 'Could not connect');
  await connecting();
  // The first page arrives as a browser opener would bring it, through the
  // launch link, which works once.
  await connects('dark', launchURL);
  await connects('light', url);
  await spentLaunch();
} finally {
  await browser.close();
}

if (failures.length) {
  console.error(`${failures.length} check(s) failed`);
  process.exit(1);
}

async function launch() {
  try {
    return await chromium.launch({ channel: 'chrome' });
  } catch {
    return chromium.launch({ executablePath: '/usr/bin/google-chrome' });
  }
}

// encodeDesktop renders a stand-in desktop and encodes two seconds of it as
// Constrained Baseline H.264 access units (Annex B, base64), the shape the
// agent sends. A blinking cursor keeps the frames changing.
async function encodeDesktop() {
  const page = await browser.newPage();
  await page.goto(`${base}icon.svg`); // WebCodecs needs a secure context
  const frames = await page.evaluate(async () => {
    const W = 1280;
    const H = 720;
    const canvas = new OffscreenCanvas(W, H);
    const g = canvas.getContext('2d');
    const draw = (i) => {
      const sky = g.createLinearGradient(0, 0, W, H);
      sky.addColorStop(0, '#2e4a5c');
      sky.addColorStop(1, '#152430');
      g.fillStyle = sky;
      g.fillRect(0, 0, W, H);
      g.fillStyle = '#0d1418';
      g.fillRect(0, 0, W, 28);
      g.fillStyle = '#d6dde2';
      g.font = '14px sans-serif';
      g.fillText('Activities', 14, 19);
      g.fillText('Thu 16 Oct  23:14', W / 2 - 60, 19);
      g.fillStyle = '#1b1f22';
      g.fillRect(140, 110, 720, 440);
      g.fillStyle = '#2b3136';
      g.fillRect(140, 110, 720, 30);
      g.fillStyle = '#c9d1d6';
      g.fillText('yasin@devbox: ~/src/robin', 160, 130);
      g.font = '15px monospace';
      const lines = ['$ go test ./...', 'ok   internal/protocol   0.41s', 'ok   internal/web        1.07s', '$ uptime', ' 23:14:02 up 12 days,  load average: 0.21, 0.30, 0.28', '$ '];
      lines.forEach((l, n) => g.fillText(l, 160, 170 + n * 22));
      if (Math.floor(i / 15) % 2 === 0) g.fillRect(180, 170 + 5 * 22 - 14, 9, 18);
      g.fillStyle = '#e9ecee';
      g.fillRect(900, 200, 300, 220);
      g.fillStyle = '#4a5560';
      g.font = '14px sans-serif';
      ['Documents', 'Downloads', 'Pictures', 'notes.md'].forEach((l, n) => g.fillText(l, 924, 240 + n * 34));
    };

    const chunks = [];
    const encoder = new VideoEncoder({
      output: (chunk) => {
        const b = new Uint8Array(chunk.byteLength);
        chunk.copyTo(b);
        let bin = '';
        for (let i = 0; i < b.length; i += 0x8000) bin += String.fromCharCode(...b.subarray(i, i + 0x8000));
        chunks.push(btoa(bin));
      },
      error: (e) => { throw e; },
    });
    encoder.configure({
      codec: 'avc1.42e01f', width: W, height: H, bitrate: 1_500_000, framerate: 30,
      latencyMode: 'realtime', avc: { format: 'annexb' },
    });
    for (let i = 0; i < 60; i++) {
      draw(i);
      const frame = new VideoFrame(canvas, { timestamp: (i * 1e6) / 30 });
      encoder.encode(frame, { keyFrame: i % 30 === 0 });
      frame.close();
    }
    await encoder.flush();
    return chunks;
  });
  await page.close();
  return frames;
}

async function rejected(name, target, title) {
  const page = await browser.newPage({ viewport: { width: 1280, height: 800 }, colorScheme: 'dark' });
  let opened = false;
  page.on('websocket', (ws) => ws.on('framereceived', () => { opened = true; }));
  await page.goto(target);
  await page.waitForFunction(() => window.robin?.state === 'ended');
  check(!opened, `${name}: no signaling reaches the page`);
  check((await page.textContent('#state-label')) === title, `${name}: shows "${title}"`);
  check(await page.evaluate(() => window.robin.addSection('late').parentElement.hidden), `${name}: sections stay hidden once ended`);
  await page.screenshot({ path: join(shots, `ended-${name.replace(' ', '-')}.png`) });
  await page.close();
}

// connecting captures the HUD while signaling stalls, with the websocket
// mocked so the server never sees this page.
async function connecting() {
  const page = await browser.newPage({ viewport: { width: 1280, height: 800 }, colorScheme: 'dark' });
  await page.routeWebSocket(/\/ws$/, () => {});
  await page.goto(url);
  await page.waitForTimeout(700);
  check((await page.evaluate(() => window.robin.state)) === 'connecting', 'stalled signaling shows connecting');
  await page.screenshot({ path: join(shots, 'connecting-dark.png') });
  await page.close();
}

// spentLaunch checks a launch link that brought a page once finds nothing.
async function spentLaunch() {
  const page = await browser.newPage();
  const response = await page.goto(launchURL);
  check(response.status() === 404 && !page.url().includes('#'), 'a used launch link is not found');
  await page.close();
}

async function connects(scheme, target) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 }, colorScheme: scheme });
  await context.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: base.replace(/\/$/, '') });
  const page = await context.newPage();
  const errors = [];
  page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', (e) => errors.push(String(e)));

  await page.goto(target);
  await page.waitForFunction(() => window.robin.pc?.localDescription?.type === 'offer');
  check((await page.evaluate(() => location.hash)) === '', `${scheme}: token removed from the address bar`);
  check(page.url() === base, `${scheme}: page at ${page.url()}`);

  await page.waitForFunction(() => window.robin.state === 'connected', null, { timeout: 15000 });
  const stats = await page.evaluate(() => window.robin.stats());
  check(stats.transport === 'udp' && stats.local?.port > 0 && stats.remote?.port > 0,
    `${scheme}: stats report the selected pair (${stats.transport} ${stats.local?.candidateType} -> ${stats.remote?.candidateType})`);
  await page.waitForSelector('#transport:not([hidden])');

  await page.waitForFunction(() => window.robin.video.videoWidth > 0, null, { timeout: 10000 });
  await page.waitForTimeout(1500); // let the bitrate and frame rate settle
  const video = await page.evaluate(() => window.robin.stats());
  check(video.frameWidth === 1280 && video.frameHeight === 720 && video.framesDecoded > 0 && video.fps > 0 && video.kbps > 0,
    `${scheme}: stats report decoded video (${video.frameWidth}x${video.frameHeight}, ${video.fps} fps, ${video.kbps} kb/s)`);

  if (scheme === 'dark') await exerciseControls(page);
  await sections(page, scheme);
  await keys(page, scheme);

  await page.mouse.move(640, 10);
  await page.waitForTimeout(400);
  await sound(page, scheme);
  await page.screenshot({ path: join(shots, `connected-${scheme}.png`) });
  await page.locator('#hud').screenshot({ path: join(shots, `hud-${scheme}.png`) });
  await page.mouse.move(640, 400);
  await page.waitForFunction(() => document.getElementById('hud').classList.contains('tucked'), null, { timeout: 5000 });
  await page.waitForTimeout(400);
  await page.screenshot({ path: join(shots, `tucked-${scheme}.png`) });

  if (scheme === 'dark') {
    await page.reload();
    await page.waitForFunction(() => window.robin?.state === 'connected', null, { timeout: 15000 });
    check(true, 'reload reconnects with the token kept for this tab');
  }

  check(errors.length === 0, `${scheme}: no console errors${errors.length ? `: ${errors.join(' | ')}` : ''}`);
  await context.close();
}

// sound checks the page autoplays muted and offers the speaker toggle once
// the stand-in's audio track arrives, and that a click turns sound on.
async function sound(page, scheme) {
  const button = page.locator('#sound');
  const shown = await button.waitFor({ state: 'visible', timeout: 5000 }).then(() => true, () => false);
  check(shown, `${scheme}: speaker toggle shown for the audio track`);
  if (!shown) return;
  check(await page.evaluate(() => window.robin.muted) && (await button.getAttribute('aria-pressed')) === 'false',
    `${scheme}: sound starts muted`);
  await page.locator('#hud').screenshot({ path: join(shots, `hud-muted-${scheme}.png`) });
  await button.click();
  await page.mouse.move(200, 10); // off the button, so its hover does not show
  check(!(await page.evaluate(() => window.robin.muted)) && (await button.getAttribute('aria-pressed')) === 'true'
    && !(await page.evaluate(() => window.robin.video.paused)),
    `${scheme}: a click turns sound on and the video keeps playing`);
  check(await page.evaluate(() => document.activeElement?.id !== 'sound'), `${scheme}: the toggle gives focus back`);
}

// sections checks a feature can add its own part of the status tab, below
// the row of stats and above the session's buttons, and that an empty one
// takes no room.
async function sections(page, scheme) {
  const got = await page.evaluate(() => {
    const s = window.robin.addSection('smoke');
    s.textContent = 'A section of its own';
    const empty = window.robin.addSection('empty');
    let twice = 'no error';
    try {
      window.robin.addSection('smoke');
    } catch (e) {
      twice = e.message;
    }
    return {
      inHud: document.getElementById('hud').contains(s),
      belowRow: s.parentElement.previousElementSibling?.classList.contains('row'),
      shown: s.offsetHeight > 0,
      emptyHeight: empty.offsetHeight,
      sessionLast: s.parentElement.lastElementChild?.dataset.section === 'session',
      twice,
    };
  });
  check(got.inHud && got.belowRow && got.shown && got.emptyHeight === 0 && got.sessionLast && /added twice/.test(got.twice),
    `${scheme}: a feature adds its own section to the status tab (${JSON.stringify(got)})`);
}

// keys checks the keys section starts folded behind its toggle, lists the
// built-in keys and the macros the Config carries once open, holds the tab
// out while it is open, and fits a phone's width, long names included.
async function keys(page, scheme) {
  await page.mouse.move(640, 10);
  const hud = page.locator('#hud');
  const toggle = page.locator('#keys');
  const folded = await toggle.getAttribute('aria-expanded') === 'false'
    && !(await hud.getByRole('button', { name: 'Ctrl+Alt+Delete' }).isVisible());
  check(folded, `${scheme}: the keys section starts folded away`);
  await toggle.click();
  const shown = await hud.getByRole('button', { name: 'Ctrl+Alt+Delete' }).isVisible()
    && await hud.getByRole('button', { name: 'Terminal', exact: true }).isVisible()
    && await toggle.getAttribute('aria-expanded') === 'true';
  check(shown, `${scheme}: open, the keys section shows the built-in keys and the CLI's macros`);
  await hud.getByRole('button', { name: 'Edit Terminal' }).click();
  const pinned = await page.evaluate(() => document.getElementById('keys-section').hasAttribute('data-pinned')
    && !document.getElementById('hud').classList.contains('tucked'));
  check(pinned, `${scheme}: the open keys section holds the tab out`);
  await hud.screenshot({ path: join(shots, `hud-keys-${scheme}.png`) });
  await hud.getByRole('button', { name: 'Cancel' }).click();

  const was = page.viewportSize();
  await page.setViewportSize({ width: 390, height: 844 });
  await hud.getByRole('button', { name: /^Edit Restart/ }).click();
  const outside = await page.evaluate(() => [...document.querySelectorAll('#keys-section button, #keys-section li')]
    .filter((el) => el.offsetParent && el.getBoundingClientRect().right > innerWidth)
    .map((el) => el.textContent || el.title));
  check(outside.length === 0, `${scheme}: at 390 px every key, macro and step stays on the screen (${JSON.stringify(outside)})`);
  await page.screenshot({ path: join(shots, `keys-phone-${scheme}.png`) });
  await hud.getByRole('button', { name: 'Cancel' }).click();
  await page.setViewportSize(was);

  await page.mouse.move(640, 10);
  await toggle.click();
  const unpinned = await page.evaluate(() => !document.getElementById('keys-section').hasAttribute('data-pinned'));
  check(unpinned, `${scheme}: folded, the keys section lets the tab tuck`);
}

// exerciseControls sends what TestBrowserSmoke expects on the data channel.
async function exerciseControls(page) {
  await page.setViewportSize({ width: 1000, height: 700 });
  await page.waitForTimeout(400); // resize is debounced

  // 1280x720 contained in 1000x700 is scaled by 0.78125 and letterboxed
  // 68.75 px top and bottom.
  await page.mouse.move(10, 30); // in the letterbox: not sent
  await page.mouse.move(500, 350);
  await page.mouse.down();
  await page.mouse.up();
  await page.mouse.move(999, 630, { steps: 1 });
  await page.mouse.down({ button: 'right' });
  await page.mouse.up({ button: 'right' });
  await page.mouse.wheel(0, 120);
  await page.keyboard.press('a');
  await page.evaluate(() => window.robin.sendClipboard('from the local hook'));

  await page.evaluate(() => window.robin.sendClipboard('echo'));
  await page.waitForFunction(() => window.robin.lastRemoteClipboard === 'from the remote', null, { timeout: 5000 })
    .then(() => check(true, 'remote clipboard reaches the page'), () => check(false, 'remote clipboard reaches the page'));

  await page.evaluate(() => navigator.clipboard.writeText('pasted locally'));
  await page.keyboard.press('Control+V');
  await page.waitForTimeout(200);
  await page.evaluate(() => window.dispatchEvent(new Event('blur')));
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.waitForTimeout(400);

  const ufrag = () => page.evaluate(() => window.robin.pc.localDescription.sdp.match(/a=ice-ufrag:(\S+)/)[1]);
  const before = await ufrag();
  const decoded = (await page.evaluate(() => window.robin.stats())).framesDecoded;
  await page.evaluate(() => window.robin.pc.restartIce());
  await page.waitForFunction((u) => {
    const pc = window.robin.pc;
    return pc.signalingState === 'stable' && !pc.localDescription.sdp.includes(`a=ice-ufrag:${u}`);
  }, before, { timeout: 10000 });
  await page.waitForFunction(() => window.robin.state === 'connected', null, { timeout: 10000 });
  // Poll rather than sleep: under a loaded machine frames can take a few
  // seconds to resume on the new path.
  const flowing = await page.waitForFunction(async (d) => (await window.robin.stats()).framesDecoded > d, decoded, { timeout: 5000 })
    .then(() => true, () => false);
  check(flowing, `ICE restart renegotiates and video keeps flowing (${before} -> ${await ufrag()})`);
}
