import test from 'node:test';
import assert from 'node:assert/strict';
import { dispatchControl, onControl } from '../control.js';

test('each control type reaches its one handler', () => {
  const got = [];
  onControl('test-a', (c) => got.push(['a', c.n]));
  onControl('test-b', (c) => got.push(['b', c.n]));
  dispatchControl({ t: 'test-a', n: 1 });
  dispatchControl({ t: 'test-b', n: 2 });
  dispatchControl({ t: 'from-a-newer-agent', n: 3 });
  dispatchControl(null);
  assert.deepEqual(got, [['a', 1], ['b', 2]]);
  assert.throws(() => onControl('test-a', () => {}), /handled twice/);
});
