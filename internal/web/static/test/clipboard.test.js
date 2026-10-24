import test from 'node:test';
import assert from 'node:assert/strict';
import { acceptBulk } from '../bulk.js';
import { Sum } from '../bulkframe.js';
import { CLIP_KIND, CLIP_LIMIT, encodeClip } from '../clip.js';
import { Clipboard } from '../clipboard.js';
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

const clipboard = new Clipboard({ send: () => {}, bulk: async () => {}, propose: () => {} });

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
