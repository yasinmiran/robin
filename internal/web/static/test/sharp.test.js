import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { FakeElement, fakeOverlay, installDom } from './dom.js';
import { marshal, pngHead } from './tilebatch.js';

// One SharpText for the whole file: it registers its control types and its
// bulk kind, which a page does once. The tests run in order on it.

// El adds what SharpText touches beyond FakeElement: style properties, a
// class list that toggles, and closest.
class El extends FakeElement {
  style = {
    setProperty(k, v) { this[k] = v; },
    removeProperty(k) { delete this[k]; },
  };
  classList = classes();
  hud = false; // closest('#hud') finds the status tab
  closest(sel) { return sel === '#hud' && this.hud ? this : null; }
}

function classes() {
  const set = new Set();
  return {
    toggle(name, on = !set.has(name)) {
      if (on) set.add(name);
      else set.delete(name);
      return on;
    },
    remove(name) { set.delete(name); },
    contains: (name) => set.has(name),
  };
}

installDom();
const drawn = [];
document.createElement = (tag) => {
  const el = new El(tag);
  if (tag === 'canvas') {
    el.getContext = () => ({
      drawImage: (image, sx, sy, w, h, x, y) => drawn.push({ image, sx, sy, x, y, w, h }),
      clearRect() {},
    });
  }
  return el;
};
document.documentElement = new EventTarget();
Object.assign(window, { innerWidth: 1000, innerHeight: 700, devicePixelRatio: 1 });
Object.defineProperty(globalThis, 'navigator', { value: { maxTouchPoints: 0 }, configurable: true });
globalThis.matchMedia = () => ({ matches: false });
globalThis.MutationObserver = class { observe() {} };
const warnings = [];
console.warn = (...args) => warnings.push(args.join(' '));

// Decodes wait until the test settles them.
const decodes = [];
globalThis.createImageBitmap = () => new Promise((resolve) => decodes.push(resolve));
const bitmap = (width, height) => ({ width, height, closed: false, close() { this.closed = true; } });

const video = new El('video');
video.videoWidth = 1000;
video.videoHeight = 700;
video.getBoundingClientRect = () => ({ left: 0, top: 0, width: 1000, height: 700 });
let canvas = null;
video.after = (el) => { canvas = el; };
let onFrame = null;
video.requestVideoFrameCallback = (cb) => { onFrame = cb; };
const showFrame = (meta) => onFrame(performance.now(), meta);

const sent = [];
const input = { view: { reset() {} }, onPinch: null };
const overlay = fakeOverlay();

const { SharpText } = await import('../sharp.js');
const { acceptBulk } = await import('../bulk.js');
const { dispatchControl } = await import('../control.js');
const sharp = new SharpText({ video, overlay, send: (m) => sent.push(m), input });

// until waits up to about two seconds for cond, a timer turn at a time, so
// animation frames run too.
async function until(what, cond) {
  for (let i = 0; i < 2000; i++) {
    if (cond()) return;
    await new Promise((resolve) => setTimeout(resolve, 1));
  }
  assert.fail(`never ${what}`);
}

// transfer sends payload to the page as the agent's one tile transfer, and
// returns the verdicts it hears.
function transfer(payload) {
  const ch = new EventTarget();
  const verdicts = [];
  ch.send = (d) => verdicts.push(JSON.parse(d));
  ch.close = () => {};
  acceptBulk(ch);
  const message = (data) => ch.dispatchEvent(Object.assign(new Event('message'), { data }));
  message(JSON.stringify({ t: 'header', kind: 'tile', size: payload.length }));
  message(payload.slice().buffer);
  message(JSON.stringify({ t: 'end', sha256: createHash('sha256').update(payload).digest('hex') }));
  return verdicts;
}

const oneTile = (rtp, gen = 1) => marshal({ rtp, gen, tiles: [0], png: pngHead(64, 64) });
const button = (key) => overlay.section.find((el) => el.dataset.switch === key);
const note = (id) => overlay.section.find((el) => el.id === id);

test('the agent is asked for tiles once the frame clock and the channel are there, and an overtaken answer is dropped', () => {
  assert.equal(sent.length, 0, 'nothing asked before the browser shows a frame');
  showFrame({ rtpTimestamp: 1 });
  sharp.open();
  assert.deepEqual(sent, [{ t: 'tiles', on: true }]);
  assert.equal(button('sharp').getAttribute('aria-busy'), 'true');
  dispatchControl({ t: 'tiles', tiles: {} });
  assert.equal(sharp.state.answered, false, 'an answer to off is not one to on');
  dispatchControl({ t: 'tiles', tiles: { on: true } });
  assert.deepEqual([sharp.state.answered, sharp.state.on], [true, true]);
  assert.equal(button('sharp').getAttribute('aria-busy'), 'false');
});

test('a batch is drawn once the video shows its frame', async () => {
  dispatchControl({ t: 'frame', frame: { rtp: 5, gen: 1, w: 64, h: 64 } });
  assert.deepEqual([canvas.width, canvas.height], [64, 64]);
  const verdicts = transfer(oneTile(5));
  await until('a decode started', () => decodes.length === 1);
  const image = bitmap(64, 64);
  decodes.shift()(image);
  await until('a verdict', () => verdicts.length === 1);
  assert.deepEqual(verdicts, [{ t: 'done' }]);
  showFrame({ rtpTimestamp: 5 });
  assert.deepEqual(drawn.map(({ image: i, ...d }) => [i, d]), [[image, { sx: 0, sy: 0, x: 0, y: 0, w: 64, h: 64 }]]);
  assert.equal(image.closed, true, 'spent once drawn');
  assert.deepEqual([sharp.state.drawn, sharp.state.visible, sharp.state.batches], [1, true, 1]);
});

test('an atlas that decodes to another size than its header said is refused', async () => {
  const verdicts = transfer(oneTile(6));
  await until('a decode started', () => decodes.length === 1);
  const image = bitmap(65535, 65535);
  decodes.shift()(image);
  await until('a verdict', () => verdicts.length === 1);
  assert.equal(verdicts[0].t, 'error');
  assert.match(verdicts[0].error, /malformed tile batch/);
  assert.equal(image.closed, true);
  assert.deepEqual([sharp.state.grid, sharp.state.drawn, sharp.state.visible], [null, 0, false], 'every tile dropped');
  assert.equal(warnings.length, 1);
});

test('a transfer stays counted until its atlas is decoded', async () => {
  // A host that never waits for the verdicts may keep four decodes under
  // way, no more.
  const open = [1, 2, 3, 4].map((rtp) => transfer(oneTile(rtp)));
  await until('four decodes started', () => decodes.length === 4);
  const fifth = transfer(oneTile(5));
  await until('the fifth refused', () => fifth.length === 1);
  assert.deepEqual(fifth, [{ t: 'error', error: 'more than 4 tile transfers at once' }]);
  assert.equal(decodes.length, 4, 'the fifth was never decoded');
  for (const settle of decodes.splice(0)) settle(bitmap(64, 64));
  await until('the four ended', () => open.every((v) => v.length === 1));
  const after = transfer(oneTile(7));
  await until('a decode started', () => decodes.length === 1);
  decodes.shift()(bitmap(64, 64));
  await until('a verdict', () => after.length === 1);
  assert.deepEqual(after, [{ t: 'done' }], 'room again once the decodes ended');
});

test('a manifest outside its grid drops every tile, warning once', () => {
  dispatchControl({ t: 'frame', frame: { rtp: 10, gen: 2, w: 128, h: 64 } });
  assert.deepEqual(sharp.state.grid, { gen: 2, w: 128, h: 64 });
  dispatchControl({ t: 'frame', frame: { rtp: 11, gen: 2, w: 128, h: 64, gone: [0, 2 ** 31] } });
  assert.equal(sharp.state.grid, null);
  assert.deepEqual([canvas.width, canvas.height, canvas.style.visibility], [0, 0, 'hidden']);
  dispatchControl({ t: 'frame', frame: { rtp: 12, gen: 2, w: 0, h: 64 } });
  assert.equal(warnings.length, 1, 'one warning for the page');
  dispatchControl({ t: 'frame', frame: { rtp: 13, gen: 3, w: 128, h: 64 } });
  assert.deepEqual(sharp.state.grid, { gen: 3, w: 128, h: 64 }, 'a good manifest starts again');
});

test('actual size pans with the pointer at the edge, not under the status tab, and says how', async () => {
  dispatchControl({ t: 'frame', frame: { rtp: 20, gen: 4, w: 2000, h: 700 } });
  button('actual').click();
  assert.equal(sharp.state.actual, true);
  assert.equal(video.style['--actual-width'], '2000px');
  assert.equal(button('actual').getAttribute('aria-describedby'), 'actual-note');
  assert.match(note('actual-note').textContent, /pointer at an edge/);
  const move = (x, y, target) => {
    const e = Object.assign(new Event('pointermove'), { pointerType: 'mouse', clientX: x, clientY: y });
    Object.defineProperty(e, 'target', { value: target });
    window.dispatchEvent(e);
  };
  const tab = new El('header');
  tab.hud = true;
  move(999, 350, tab);
  // An animation frame asked for then would run before this timer.
  await new Promise((resolve) => setTimeout(resolve, 5));
  assert.equal(video.style['--actual-left'], '0px', 'no pan under the status tab');
  move(999, 350, new El('video'));
  await until('a pan', () => video.style['--actual-left'] !== '0px');
  assert.ok(parseFloat(video.style['--actual-left']) < 0);
  // Two fingers pan rather than zoom.
  const left = parseFloat(video.style['--actual-left']);
  assert.equal(input.onPinch({ from: { cx: 150, cy: 100 }, to: { cx: 100, cy: 100 } }), true);
  assert.equal(parseFloat(video.style['--actual-left']), left - 50);
  button('actual').click();
  assert.equal(video.style['--actual-width'], undefined);
  assert.equal(note('actual-note'), null, 'no hint while the picture fits');
  assert.equal(input.onPinch({ from: { cx: 0, cy: 0 }, to: { cx: 1, cy: 1 } }), false, 'the zoom has the pinch back');
});

test('off asks the agent and empties the canvas; its answer counts', () => {
  sent.length = 0;
  button('sharp').click();
  assert.deepEqual(sent, [{ t: 'tiles', on: false }]);
  assert.deepEqual([sharp.state.want, sharp.state.answered, sharp.state.grid], [false, false, null]);
  dispatchControl({ t: 'tiles', tiles: { on: true } });
  assert.equal(sharp.state.answered, false, 'an answer to on is not one to off');
  dispatchControl({ t: 'tiles', tiles: {} });
  assert.equal(sharp.state.answered, true);
  sharp.toggle('sharp');
  assert.deepEqual(sent.at(-1), { t: 'tiles', on: true });
});

test('in a browser without a frame clock the switch says why, stays focusable and does nothing', () => {
  showFrame({});
  const b = button('sharp');
  assert.equal(b.getAttribute('aria-disabled'), 'true');
  assert.equal(b.disabled, undefined, 'focusable, so its reason can be reached');
  assert.equal(b.getAttribute('aria-describedby'), 'sharp-note');
  assert.match(note('sharp-note').textContent, /^Not available in this browser/);
  assert.equal(sent.at(-1).on, false, 'tiles asked off');
  b.click();
  assert.equal(sharp.state.want, true, 'the click did nothing');
});

test('reset forgets the peer', () => {
  sharp.reset();
  assert.deepEqual([sharp.state.answered, sharp.state.grid, sharp.state.visible], [false, null, false]);
  sharp.trace();
  assert.deepEqual(sharp.stopTrace(), { frames: [], manifests: [] });
});
