import test from 'node:test';
import assert from 'node:assert/strict';
import { acceptBulk } from '../bulk.js';
import { Sum } from '../bulkframe.js';
import { dispatchControl } from '../control.js';
import { Clipboard } from '../clipboard.js';
import { Files } from '../files.js';
import { FakeChannel } from './fakes.js';
import { fakeOverlay, installDom } from './dom.js';

const saved = installDom();
const overlay = fakeOverlay();
const later = (ms = 0) => new Promise((resolve) => setTimeout(resolve, ms));

// One Files and one Clipboard for the whole file, as bulk kinds register
// once; app.js wires them the same way.
const controls = [];
const bulks = [];
const files = new Files({
  overlay,
  send: (c) => controls.push(c),
  bulk: async (header, blob) => { bulks.push({ header, size: blob.size }); },
});
const clipboard = new Clipboard({
  send: (c) => controls.push(c),
  bulk: async () => {},
  propose: (list) => files.propose(list),
});

// fromAgent sends one file transfer to the page as the agent would, and
// resolves with the page's verdict.
async function fromAgent(id, bytes) {
  const page = new FakeChannel('bulk');
  const agent = new FakeChannel('bulk');
  page.peer = agent;
  agent.peer = page;
  page.readyState = agent.readyState = 'open';
  const verdict = new Promise((resolve) => agent.addEventListener('message', (e) => resolve(JSON.parse(e.data)), { once: true }));
  acceptBulk(page);
  agent.send(JSON.stringify({ t: 'header', kind: 'file', id, name: `${id}.bin`, size: bytes.length }));
  const sum = new Sum(bytes.length);
  sum.add(bytes);
  agent.send(bytes);
  agent.send(JSON.stringify(sum.end()));
  return verdict;
}

// rendered waits for the tab to draw and returns the row of id.
async function row(id) {
  await later();
  await later();
  return overlay.section.find((el) => el.tagName === 'LI' && el.dataset.id === id);
}

const button = (li, className) => li?.find((el) => el.tagName === 'BUTTON' && el.className === className);

test('one click on Save takes one download, and a dismissed one is not saved', async () => {
  dispatchControl({ t: 'file-offer', file: { id: 'd1', name: 'd1.bin', size: 3 } });
  button(await row('d1'), 'save').click();
  assert.deepEqual(controls.at(-1), { t: 'file-fetch', file: { id: 'd1' } });
  assert.deepEqual(await fromAgent('d1', new Uint8Array([1, 2, 3])), { t: 'done' });
  assert.deepEqual(saved.map((s) => s.name), ['d1.bin']);
  // The host cannot spend that click again.
  const again = await fromAgent('d1', new Uint8Array([4, 5, 6]));
  assert.equal(again.t, 'error');
  assert.match(again.error, /nobody asked/);
  assert.equal(saved.length, 1);

  dispatchControl({ t: 'file-offer', file: { id: 'd2', name: 'd2.bin', size: 3 } });
  button(await row('d2'), 'save').click();
  const second = fromAgent('d2', new Uint8Array([7, 8, 9]));
  // Two channels for one click: the second is refused on its header.
  const third = await fromAgent('d2', new Uint8Array([7, 8, 9]));
  assert.match(third.error ?? '', /nobody asked/);
  assert.equal((await second).t, 'done');
  assert.equal(saved.length, 2);

  dispatchControl({ t: 'file-offer', file: { id: 'd3', name: 'd3.bin', size: 3 } });
  button(await row('d3'), 'save').click();
  const dismissed = fromAgent('d3', new Uint8Array([1, 1, 1]));
  button(await row('d3'), 'dismiss').click();
  assert.deepEqual(controls.at(-1), { t: 'file-decline', file: { id: 'd3' } });
  assert.equal((await dismissed).t, 'error');
  assert.equal(saved.length, 2, 'a dismissed download was saved');
});

test('files pasted wait for a click on Upload', async () => {
  const pdf = { name: 'report.pdf', type: 'application/pdf', size: 4 };
  const data = { getData: (type) => (type === 'text/plain' ? 'report.pdf' : ''), files: [pdf] };
  await clipboard.paste(data);
  await later();
  assert.equal(bulks.length, 0, 'a pasted file went to the host unasked');
  const pending = files.list().find((t) => t.name === 'report.pdf');
  assert.equal(pending?.state, 'pending');
  assert.ok(button(await row(pending.id), 'dismiss'), 'a pending upload can be dismissed');
  button(await row(pending.id), 'send').click();
  await later();
  assert.equal(bulks.length, 1);
  assert.equal(bulks[0].header.name, 'report.pdf');
  assert.equal(files.list().find((t) => t.id === pending.id).state, 'kept');

  await clipboard.paste({ getData: () => '', files: [{ name: 'old.zip', type: 'application/zip', size: 1 }] });
  const old = files.list().find((t) => t.name === 'old.zip');
  button(await row(old.id), 'dismiss').click();
  await later();
  assert.equal(files.list().some((t) => t.name === 'old.zip'), false);
  assert.equal(bulks.length, 1);
});
