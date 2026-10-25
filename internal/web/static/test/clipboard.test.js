import test from 'node:test';
import assert from 'node:assert/strict';
import { BULK_WINDOW, CHUNK_SIZE, Sum } from '../bulkframe.js';
import { CLIP_KIND, CLIP_LIMIT, MAX_CLIP, encodeClip } from '../clip.js';
import { Clipboard } from '../clipboard.js';
import { dispatchControl } from '../control.js';
import { fromAgent, until, within } from './fakes.js';

const later = () => new Promise((resolve) => setTimeout(resolve, 0));

// What the page hands the browser's clipboard.
const written = [];
Object.defineProperty(globalThis, 'navigator', {
  configurable: true,
  value: { clipboard: { write: async (items) => written.push(items), writeText: async (text) => written.push(text) } },
});
globalThis.ClipboardItem = class {
  constructor(items) {
    this.types = Object.keys(items);
  }
};

const controls = [];
const bulks = [];
const proposed = [];
const clipboard = new Clipboard({
  send: (c) => controls.push(c),
  bulk: async (header, blob) => { bulks.push({ header, size: blob.size }); },
  propose: (files) => proposed.push(...files),
});

// clipFromAgent opens a clipboard transfer of size bytes to the page as
// the agent would and sends its header.
const clipFromAgent = (size) => fromAgent({ kind: CLIP_KIND, size });

test('what the host copied reaches the browser in chunks', async () => {
  const html = `<p>${'robin '.repeat(20000)}</p>`;
  const bytes = encodeClip({ text: 'robin', html });
  const t = clipFromAgent(bytes.length);
  const sum = new Sum(bytes.length);
  for (let at = 0; at < bytes.length; at += 64 * 1024) {
    const chunk = bytes.slice(at, at + 64 * 1024);
    sum.add(chunk);
    t.agent.send(chunk);
  }
  t.agent.send(JSON.stringify(sum.end()));
  assert.deepEqual(await t.verdict, { t: 'done' });
  assert.deepEqual(clipboard.last, { text: 'robin', html });
  assert.deepEqual(written.at(-1)[0].types, ['text/plain', 'text/html']);
});

test('a host copy past the credit window arrives as the page grants more', async () => {
  const html = `<p>${'robin '.repeat(250000)}</p>`;
  const bytes = encodeClip({ text: 'robin', html });
  const t = clipFromAgent(bytes.length);
  let credit = BULK_WINDOW;
  let grants = 0;
  t.agent.addEventListener('message', (e) => {
    const frame = JSON.parse(e.data);
    if (frame.t === 'credit') {
      credit = frame.credit;
      grants++;
    }
  });
  const sum = new Sum(bytes.length);
  for (let at = 0; at < bytes.length; at += CHUNK_SIZE) {
    const chunk = bytes.slice(at, at + CHUNK_SIZE);
    await until(`credit for ${at + chunk.length} bytes`, () => at + chunk.length <= credit);
    sum.add(chunk);
    t.agent.send(chunk);
  }
  t.agent.send(JSON.stringify(sum.end()));
  assert.deepEqual(await within(t.verdict), { t: 'done' });
  assert.ok(grants > 0, 'the page granted no credit');
  assert.deepEqual(clipboard.last, { text: 'robin', html });
});

test('headers alone reserve no memory, and a third at once pushes out the oldest', async () => {
  const before = process.memoryUsage().arrayBuffers;
  const [first, second, third] = [clipFromAgent(CLIP_LIMIT), clipFromAgent(CLIP_LIMIT), clipFromAgent(CLIP_LIMIT)];
  // The third pushes out the first only once the two before it hold their
  // places, and takes its own before the first hears, so by then the
  // sinks are what they are.
  const pushed = await within(first.verdict);
  assert.equal(pushed.t, 'error', `the first transfer: ${JSON.stringify(pushed)}`);
  assert.match(pushed.error, /superseded by a newer clipboard transfer/);
  const grew = process.memoryUsage().arrayBuffers - before;
  assert.ok(grew < 1024 * 1024, `two held headers took ${grew} bytes`);

  const held = [second, third];
  for (const t of held) t.agent.close();
  await until('both channels closed at the page', () => held.every((t) => t.page.readyState === 'closed'));
  const empty = clipFromAgent(0);
  empty.agent.send(JSON.stringify(new Sum(0).end()));
  assert.deepEqual(await empty.verdict, { t: 'done' });
});

test('the newest host copy reaches the page while two older ones have yet to close', async () => {
  const [a, b, c] = ['a', 'b', 'c'].map((text) => {
    const clip = { text, html: `<b>${text}</b>` };
    const bytes = encodeClip(clip);
    return { clip, bytes, ...clipFromAgent(bytes.length) };
  });
  // The agent cancelled a and b for c, but c's header got here first.
  const pushed = await within(a.verdict);
  assert.equal(pushed.t, 'error', `the oldest copy: ${JSON.stringify(pushed)}`);
  assert.match(pushed.error, /superseded/);
  a.agent.close();
  b.agent.close();
  const sum = new Sum(c.bytes.length);
  sum.add(c.bytes);
  c.agent.send(c.bytes);
  c.agent.send(JSON.stringify(sum.end()));
  assert.deepEqual(await within(c.verdict), { t: 'done' });
  assert.deepEqual(clipboard.last, c.clip);
  await until('the cancelled copy closed at the page', () => b.page.readyState === 'closed');
});

test('put sends short text as a control message, the rest as a transfer, and leaves out an image too big', async () => {
  await clipboard.put({ text: 'hi' });
  assert.deepEqual(controls.at(-1), { t: 'clipboard', text: 'hi' });
  await clipboard.put({ text: 'hi', html: '<b>hi</b>' });
  assert.deepEqual(bulks.at(-1), { header: { kind: CLIP_KIND }, size: encodeClip({ text: 'hi', html: '<b>hi</b>' }).length });

  const warn = console.warn;
  console.warn = () => {};
  try {
    await clipboard.put({ text: 'beside it', png: new Uint8Array(MAX_CLIP + 1) });
    assert.deepEqual(controls.at(-1), { t: 'clipboard', text: 'beside it' });
    const sent = controls.length + bulks.length;
    await clipboard.put({ html: 'x'.repeat(MAX_CLIP + 1) });
    assert.equal(controls.length + bulks.length, sent, 'content over the limit went');
  } finally {
    console.warn = warn;
  }
});

test('a paste sends an image as the clipboard and other files as uploads that wait', async () => {
  const png = { name: 'shot.png', type: 'image/png', size: 4, arrayBuffer: async () => new Uint8Array([1, 2, 3, 4]).buffer };
  const paste = (text, files) => ({ getData: (type) => (type === 'text/plain' ? text : ''), files });
  await clipboard.paste(paste('a shot', [png]));
  assert.deepEqual(bulks.at(-1).header, { kind: CLIP_KIND });
  await clipboard.paste(paste('notes.txt', [{ name: 'notes.txt', type: 'text/plain', size: 1 }]));
  assert.deepEqual(proposed.map((f) => f.name), ['notes.txt']);
  await clipboard.paste(paste('just text', []));
  assert.deepEqual(controls.at(-1), { t: 'clipboard', text: 'just text' });
});

test('a host copy reaches the browser as text where rich content cannot go', async () => {
  dispatchControl({ t: 'clipboard', text: 'copied' });
  assert.deepEqual(clipboard.last, { text: 'copied' });
  assert.equal(written.at(-1), 'copied');

  const item = globalThis.ClipboardItem;
  delete globalThis.ClipboardItem;
  try {
    await copyFromHost({ text: 'plain', html: '<i>rich</i>' });
    assert.equal(written.at(-1), 'plain');
  } finally {
    globalThis.ClipboardItem = item;
  }
  const write = navigator.clipboard.write;
  navigator.clipboard.write = async () => { throw new Error('not focused'); };
  try {
    await copyFromHost({ text: 'fallback', png: new Uint8Array([1]) });
    await later();
    assert.equal(written.at(-1), 'fallback');
  } finally {
    navigator.clipboard.write = write;
  }
});

// copyFromHost sends clip to the page as the agent sends a host copy.
async function copyFromHost(clip) {
  const bytes = encodeClip(clip);
  const t = clipFromAgent(bytes.length);
  const sum = new Sum(bytes.length);
  sum.add(bytes);
  t.agent.send(bytes);
  t.agent.send(JSON.stringify(sum.end()));
  assert.deepEqual(await t.verdict, { t: 'done' });
}
