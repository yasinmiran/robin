// Drives the client's text and touch input in system Chrome against a live
// internal/web server: an input method composing on a desktop, and a phone's
// taps, long press, two-finger scroll, pinch and soft keyboard. TestBrowserInput
// starts the server, plays the agent's side, checks what the page sends,
// and runs this script with the page URL and a path to write the video
// frames its stand-in agent streams.
import { chromium } from 'playwright-core';
import { writeFileSync } from 'node:fs';

const [url, framesPath] = process.argv.slice(2);
if (!url || !framesPath) {
  console.error('usage: node input.mjs <page url> <frames.json>');
  process.exit(2);
}
const base = url.slice(0, url.indexOf('#'));

const browser = await launch();
const failures = [];
const check = (ok, what) => {
  if (!ok) failures.push(what);
  console.log(`${ok ? 'ok  ' : 'FAIL'} ${what}`);
};

try {
  writeFileSync(framesPath, JSON.stringify(await encodeFrames()));
  await desktop();
  await phone();
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

// encodeFrames encodes a second of a plain 1280x720 picture as the agent
// would send it, so the page has a video size to map points onto.
async function encodeFrames() {
  const page = await browser.newPage();
  await page.goto(`${base}icon.svg`); // WebCodecs needs a secure context
  const frames = await page.evaluate(async () => {
    const canvas = new OffscreenCanvas(1280, 720);
    const g = canvas.getContext('2d');
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
      codec: 'avc1.42e01f', width: 1280, height: 720, bitrate: 500_000, framerate: 30,
      latencyMode: 'realtime', avc: { format: 'annexb' },
    });
    for (let i = 0; i < 30; i++) {
      g.fillStyle = `hsl(${i * 12} 40% 30%)`;
      g.fillRect(0, 0, 1280, 720);
      const frame = new VideoFrame(canvas, { timestamp: (i * 1e6) / 30 });
      encoder.encode(frame, { keyFrame: i % 15 === 0 });
      frame.close();
    }
    await encoder.flush();
    return chunks;
  });
  await page.close();
  return frames;
}

async function open(options) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 720 }, ...options });
  const page = await context.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e)));
  page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
  await page.goto(url);
  await page.waitForFunction(() => window.robin.state === 'connected', null, { timeout: 15000 });
  await page.waitForFunction(() => window.robin.video.videoWidth > 0, null, { timeout: 10000 });
  const cdp = await context.newCDPSession(page);
  return { context, page, cdp, errors };
}

function focused(page) {
  return page.evaluate(() => document.activeElement?.id ?? '');
}

function preedit(page) {
  return page.evaluate(() => {
    const p = document.getElementById('preedit');
    return p.hidden ? null : p.textContent;
  });
}

// desktop composes with an input method, as Chrome's IME interface does
// for one, in a desktop browser: the text box has the focus from the
// start and gets it back from the page's buttons.
async function desktop() {
  const { context, page, cdp, errors } = await open({});
  check(await focused(page) === 'typing', 'desktop: the text box has the focus');
  check(await page.locator('#keyboard').isHidden(), 'desktop: no keyboard button without a touch screen');
  await page.mouse.move(640, 10);
  await page.locator('#fullscreen').focus();
  await page.evaluate(() => document.getElementById('fullscreen').blur());
  await page.waitForFunction(() => document.activeElement?.id === 'typing', null, { timeout: 2000 })
    .then(() => check(true, 'desktop: the text box takes the focus back'), () => check(false, 'desktop: the text box takes the focus back'));

  // Chrome's first key of a composition is Process, keyCode 229.
  await cdp.send('Input.dispatchKeyEvent', { type: 'rawKeyDown', key: 'Process', code: 'KeyN', windowsVirtualKeyCode: 229 });
  await cdp.send('Input.imeSetComposition', { text: 'にほ', selectionStart: 2, selectionEnd: 2 });
  await cdp.send('Input.dispatchKeyEvent', { type: 'keyUp', key: 'Process', code: 'KeyN', windowsVirtualKeyCode: 229 });
  check(await preedit(page) === 'にほ', `desktop: the composition shows while it lasts (${await preedit(page)})`);
  await cdp.send('Input.imeSetComposition', { text: 'にほん', selectionStart: 3, selectionEnd: 3 });
  await cdp.send('Input.insertText', { text: '日本' });
  check(await preedit(page) === null, 'desktop: the composition goes once it commits');
  check(await page.evaluate(() => document.getElementById('typing').value) === '', 'desktop: the text box is left empty');
  // Ordinary keys still go as keys, and type nothing into the text box.
  await page.keyboard.press('z');
  check(await page.evaluate(() => document.getElementById('typing').value) === '', 'desktop: a key types nothing locally');
  await page.waitForTimeout(300);
  check(errors.length === 0, `desktop: no console errors${errors.length ? `: ${errors.join(' | ')}` : ''}`);
  await context.close();
}

// touch sends one CDP touch event with the given points.
function touch(cdp, type, points) {
  return cdp.send('Input.dispatchTouchEvent', { type, touchPoints: points.map(([x, y], id) => ({ x, y, id })) });
}

async function phone() {
  const { context, page, cdp, errors } = await open({ hasTouch: true, isMobile: true });
  check(await page.locator('#keyboard').isVisible(), 'phone: the keyboard button is shown');
  check(await focused(page) !== 'typing', 'phone: the soft keyboard stays down until asked for');
  await page.waitForTimeout(3000);
  check(!(await page.evaluate(() => document.getElementById('hud').classList.contains('tucked'))),
    'phone: the status tab stays out, as nothing hovers');

  // A tap clicks.
  await touch(cdp, 'touchStart', [[100, 200]]);
  await touch(cdp, 'touchEnd', []);
  // A long press right clicks.
  await touch(cdp, 'touchStart', [[300, 300]]);
  await page.waitForTimeout(700);
  await touch(cdp, 'touchEnd', []);
  // Two fingers moving up together scroll down.
  await touch(cdp, 'touchStart', [[500, 400], [600, 400]]);
  for (let y = 390; y >= 300; y -= 10) await touch(cdp, 'touchMove', [[500, y], [600, y]]);
  await touch(cdp, 'touchEnd', []);
  check((await page.evaluate(() => window.robin.zoom.scale)) === 1, 'phone: scrolling leaves the zoom alone');

  // Two fingers spreading from 200 to 400 px about the centre zoom in 2x.
  await touch(cdp, 'touchStart', [[540, 360], [740, 360]]);
  for (let d = 110; d <= 200; d += 10) await touch(cdp, 'touchMove', [[640 - d, 360], [640 + d, 360]]);
  await touch(cdp, 'touchEnd', []);
  const zoom = await page.evaluate(() => window.robin.zoom);
  check(Math.abs(zoom.scale - 2) < 0.01 && Math.abs(zoom.x + 640) < 1 && Math.abs(zoom.y + 360) < 1,
    `phone: a pinch zooms about the fingers (${JSON.stringify(zoom)})`);
  // Zoomed, the top-left corner of the page shows remote 320,180.
  await touch(cdp, 'touchStart', [[0, 0]]);
  await touch(cdp, 'touchEnd', []);

  // The keyboard button raises the soft keyboard through the text box.
  await page.locator('#keyboard').tap();
  check(await focused(page) === 'typing' && (await page.getAttribute('#keyboard', 'aria-pressed')) === 'true',
    'phone: the keyboard button focuses the text box');
  // A soft keyboard composes words and types what has no key as input.
  await cdp.send('Input.imeSetComposition', { text: 'robi', selectionStart: 4, selectionEnd: 4 });
  await cdp.send('Input.insertText', { text: 'robin' });
  await cdp.send('Input.insertText', { text: '!' });
  await page.locator('#keyboard').tap();
  check(await focused(page) !== 'typing' && (await page.getAttribute('#keyboard', 'aria-pressed')) === 'false',
    'phone: pressed again, it puts the keyboard away');
  await page.waitForTimeout(300);
  check(errors.length === 0, `phone: no console errors${errors.length ? `: ${errors.join(' | ')}` : ''}`);
  await context.close();
}
