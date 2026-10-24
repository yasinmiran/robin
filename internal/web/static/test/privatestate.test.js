import test from 'node:test';
import assert from 'node:assert/strict';
import { request, settle, view } from '../privatestate.js';

test('a spawned desktop shows no switches', () => {
  assert.deepEqual(view(undefined), []);
  assert.deepEqual(view(null), []);
});

test('the switches follow the agent', () => {
  const [curtain, silent] = view({ curtain: { on: true }, silent: {} });
  assert.equal(curtain.key, 'curtain');
  assert.equal(curtain.on, true);
  assert.match(curtain.title, /is black; this page still shows it/);
  assert.match(curtain.title, /keyboard and mouse still work/);
  assert.equal(silent.key, 'silent');
  assert.equal(silent.on, false);
  assert.match(silent.title, /^Keep the speakers at the host quiet/);
  assert.equal(curtain.busy || silent.busy, false);
  assert.equal(curtain.error + silent.error, '');
});

test('a switch the desktop cannot have is left out', () => {
  const shown = view({ curtain: { unsupported: 'KDE Plasma has none' }, silent: { error: 'refused' } });
  assert.deepEqual(shown.map((s) => s.key), ['silent']);
  assert.equal(shown[0].error, 'refused');
  assert.deepEqual(view({ curtain: { unsupported: 'x' }, silent: { unsupported: 'y' } }), []);
});

test('a switch waits for the agent, and an answer or a failure ends the wait', () => {
  const pending = { curtain: true, silent: false };
  assert.deepEqual(view({ curtain: {}, silent: { on: true } }, pending).map((s) => s.busy), [true, true]);

  settle(pending, { curtain: {}, silent: { on: true } });
  assert.deepEqual(pending, { curtain: true, silent: false }, 'no answer yet to either');
  settle(pending, { curtain: { on: true }, silent: { on: true } });
  assert.deepEqual(pending, { silent: false });
  settle(pending, { curtain: { on: true }, silent: { on: true, error: 'the sink is busy' } });
  assert.deepEqual(pending, {});
  assert.deepEqual(settle({ curtain: true }, null), {});
});

test('a request names the switch and the position', () => {
  assert.deepEqual(request('curtain', true), { t: 'curtain', on: true });
  assert.deepEqual(request('silent', undefined), { t: 'silent', on: false });
});
