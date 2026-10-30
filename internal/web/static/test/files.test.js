import test from 'node:test';
import assert from 'node:assert/strict';
import { Sum } from '../bulkframe.js';
import { dispatchControl } from '../control.js';
import { Clipboard } from '../clipboard.js';
import { Files } from '../files.js';
import { fromAgent } from './fakes.js';
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

// fileFromAgent sends one file transfer to the page as the agent would,
// with header's fields over the usual ones, and resolves with the page's
// verdict.
async function fileFromAgent(id, bytes, header = {}) {
  const { agent, verdict } = fromAgent({ kind: 'file', id, name: `${id}.bin`, size: bytes.length, ...header });
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
  assert.deepEqual(await fileFromAgent('d1', new Uint8Array([1, 2, 3])), { t: 'done' });
  assert.deepEqual(saved.map((s) => s.name), ['d1.bin']);
  // The host cannot spend that click again.
  const again = await fileFromAgent('d1', new Uint8Array([4, 5, 6]));
  assert.equal(again.t, 'error');
  assert.match(again.error, /nobody asked/);
  assert.equal(saved.length, 1);

  dispatchControl({ t: 'file-offer', file: { id: 'd2', name: 'd2.bin', size: 3 } });
  button(await row('d2'), 'save').click();
  const second = fileFromAgent('d2', new Uint8Array([7, 8, 9]));
  // Two channels for one click: the second is refused on its header.
  const third = await fileFromAgent('d2', new Uint8Array([7, 8, 9]));
  assert.match(third.error ?? '', /nobody asked/);
  assert.equal((await second).t, 'done');
  assert.equal(saved.length, 2);

  dispatchControl({ t: 'file-offer', file: { id: 'd3', name: 'd3.bin', size: 3 } });
  button(await row('d3'), 'save').click();
  const dismissed = fileFromAgent('d3', new Uint8Array([1, 1, 1]));
  button(await row('d3'), 'dismiss').click();
  assert.deepEqual(controls.at(-1), { t: 'file-decline', file: { id: 'd3' } });
  assert.equal((await dismissed).t, 'error');
  assert.equal(saved.length, 2, 'a dismissed download was saved');
});

test('withheld, uploads say why and go nowhere, until they are allowed again', async () => {
  const pick = overlay.section.find((el) => el.tagName === 'BUTTON' && el.className === 'upload');
  const note = overlay.section.find((el) => el.id === 'files-note');
  const input = overlay.section.find((el) => el.id === 'files-input');
  let picked = 0;
  input.click = () => { picked++; };
  files.withhold('Uploads need control from robin');
  assert.equal(pick.getAttribute('aria-disabled'), 'true');
  assert.equal(pick.getAttribute('aria-describedby'), 'files-note');
  assert.equal(note.textContent, 'Uploads need control from robin');
  assert.equal(note.hidden, false);
  pick.click();
  assert.equal(picked, 0, 'no file picker opens');
  const before = bulks.length;
  files.upload([{ name: 'dropped.txt', size: 1 }]);
  files.propose([{ name: 'pasted.txt', size: 1 }]);
  const pasted = files.list().find((t) => t.name === 'pasted.txt');
  button(await row(pasted.id), 'send').click();
  await later();
  assert.equal(bulks.length, before, 'nothing went to the host');
  assert.equal(files.list().some((t) => t.name === 'dropped.txt'), false);
  button(await row(pasted.id), 'dismiss').click();

  files.withhold('');
  assert.equal(pick.hasAttribute('aria-disabled'), false);
  assert.equal(note.hidden, true);
  pick.click();
  assert.equal(picked, 1, 'the file picker opens again');
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

test('the host cannot withdraw an upload', async () => {
  const before = bulks.length;
  files.upload([{ name: 'w1.bin', size: 1 }, { name: 'w2.bin', size: 2 }]);
  const ids = files.list().filter((t) => ['w1.bin', 'w2.bin'].includes(t.name)).map((t) => t.id);
  assert.deepEqual(files.list().filter((t) => ids.includes(t.id)).map((t) => t.state), ['sending', 'queued']);
  for (const id of ids) dispatchControl({ t: 'file-withdraw', file: { id } });
  await later();
  assert.deepEqual(files.list().filter((t) => ids.includes(t.id)).map((t) => t.state), ['kept', 'kept']);
  assert.deepEqual(bulks.slice(before).map((b) => b.header.name), ['w1.bin', 'w2.bin']);

  dispatchControl({ t: 'file-offer', file: { id: 'w3', name: 'w3.bin', size: 1 } });
  dispatchControl({ t: 'file-withdraw', file: { id: 'w3' } });
  assert.equal(files.list().some((t) => t.id === 'w3'), false, 'an offer the host withdrew is still listed');
});

test('a download may not be larger than its offer', async () => {
  const before = saved.length;
  dispatchControl({ t: 'file-offer', file: { id: 'd4', name: 'd4.bin', size: 3 } });
  button(await row('d4'), 'save').click();
  const bigger = await fileFromAgent('d4', new Uint8Array(6));
  assert.equal(bigger.t, 'error');
  assert.match(bigger.error, /larger than the 3 B offered/);
  assert.equal(saved.length, before, 'a download larger than its offer was saved');
  const t = files.list().find((x) => x.id === 'd4');
  assert.equal(t.state, 'offered');
  assert.match(t.error, /larger than/);

  // A file that shrank since its offer still comes, as it is now.
  button(await row('d4'), 'save').click();
  assert.deepEqual(await fileFromAgent('d4', new Uint8Array(2)), { t: 'done' });
  assert.equal(saved.length, before + 1);

  // The agent leaves out a size of 0.
  dispatchControl({ t: 'file-offer', file: { id: 'd5', name: 'd5.bin', size: 0 } });
  button(await row('d5'), 'save').click();
  assert.deepEqual(await fileFromAgent('d5', new Uint8Array(0), { size: undefined }), { t: 'done' });
  assert.equal(saved.length, before + 2);
  // An offer has to say how big it is.
  for (const size of ['lots', -1, 1.5]) dispatchControl({ t: 'file-offer', file: { id: 'd6', name: 'd6.bin', size } });
  assert.equal(files.list().some((t) => t.id === 'd6'), false, 'an offer of no size is listed');
});
