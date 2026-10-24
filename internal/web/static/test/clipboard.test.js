import test from 'node:test';
import assert from 'node:assert/strict';
import { acceptBulk } from '../bulk.js';
import { Sum } from '../bulkframe.js';
import { CLIP_KIND, CLIP_LIMIT, MAX_CLIP, encodeClip } from '../clip.js';
import { Clipboard } from '../clipboard.js';
import { dispatchControl } from '../control.js';
import { FakeChannel } from './fakes.js';

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

// fromAgent opens a clipboard transfer to the page as the agent would and
// sends its header; verdict resolves with the page's answer.
function fromAgent(size) {
  const page = new FakeChannel('bulk');
  const agent = new FakeChannel('bulk');
  page.peer = agent;
  agent.peer = page;
  page.readyState = agent.readyState = 'open';
  const verdict = new Promise((resolve) => agent.addEventListener('message', (e) => resolve(JSON.parse(e.data)), { once: true }));
  acceptBulk(page);
  agent.send(JSON.stringify({ t: 'header', kind: CLIP_KIND, size }));
  return { agent, verdict };
}

test('what the host copied reaches the browser in chunks', async () => {
  const html = `<p>${'robin '.repeat(20000)}</p>`;
  const bytes = encodeClip({ text: 'robin', html });
  const t = fromAgent(bytes.length);
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

test('headers alone reserve no memory, and a third transfer at once is refused', async () => {
  const before = process.memoryUsage().arrayBuffers;
  const held = [fromAgent(CLIP_LIMIT), fromAgent(CLIP_LIMIT)];
  for (let i = 0; i < 20; i++) await later();
  const grew = process.memoryUsage().arrayBuffers - before;
  assert.ok(grew < 1024 * 1024, `two headers took ${grew} bytes`);

  const third = await fromAgent(CLIP_LIMIT).verdict;
  assert.equal(third.t, 'error');
  assert.match(third.error, /too many clipboard transfers under way/);
  for (const t of held) t.agent.close();
  for (let i = 0; i < 20; i++) await later();
  const empty = fromAgent(0);
  empty.agent.send(JSON.stringify(new Sum(0).end()));
  assert.deepEqual(await empty.verdict, { t: 'done' });
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
  const t = fromAgent(bytes.length);
  const sum = new Sum(bytes.length);
  sum.add(bytes);
  t.agent.send(bytes);
  t.agent.send(JSON.stringify(sum.end()));
  assert.deepEqual(await t.verdict, { t: 'done' });
}
