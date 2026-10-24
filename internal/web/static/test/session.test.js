import test from 'node:test';
import assert from 'node:assert/strict';
import { Session } from '../session.js';

test('a kept desktop is one with a keep window', () => {
  const s = new Session();
  assert.equal(s.canKeep, false);
  s.configure({ keep: 28800 });
  assert.equal(s.canKeep, true);
  s.configure({});
  assert.equal(s.canKeep, false);
  s.configure(null);
  assert.equal(s.canKeep, false);
});

test('detaching and ending send their signals and name the close', () => {
  const s = new Session();
  assert.equal(s.closeReason(false), 'refused');
  assert.equal(s.closeReason(true), 'closed');
  assert.deepEqual(s.detach(), { type: 'detach' });
  assert.equal(s.closeReason(true), 'kept');
  assert.equal(s.closeReason(false), 'refused');
  assert.deepEqual(s.end(), { type: 'bye' });
  assert.equal(s.closeReason(true), 'unconfirmed');
  s.ended();
  assert.equal(s.closeReason(true), 'ended');
});

test('the host ending the session needs no request first', () => {
  const s = new Session();
  s.ended();
  assert.equal(s.closeReason(true), 'ended');
});
