import test from 'node:test';
import assert from 'node:assert/strict';
import { fakeOverlay, installDom } from './dom.js';
import { dispatchControl } from '../control.js';
import { PrivateSwitches } from '../private.js';

installDom();

// channel stands in for the control channel: what is sent while it is
// closed goes nowhere, as app.js's sendControl drops it.
const channel = { open: true, delivered: [] };
const overlay = fakeOverlay();
const switches = new PrivateSwitches(overlay, (c) => {
  if (channel.open) channel.delivered.push(c);
});

const button = (key) => overlay.section.find((el) => el.dataset.switch === key);
const agentSays = (state) => dispatchControl({ t: 'private', private: state });

test('a switch clicked while the page reconnects waits for the next control channel', () => {
  agentSays({ curtain: {}, silent: {} });
  assert.equal(button('curtain').getAttribute('aria-checked'), 'false');

  channel.open = false; // the CLI is redialing; there is no peer
  button('curtain').click();
  assert.deepEqual(channel.delivered, [], 'nothing could carry the click');
  assert.equal(button('curtain').getAttribute('aria-busy'), 'true', 'the switch waits, and shows it');

  // The new peer's channel opens; the agent tells its state first, which
  // does not answer the click.
  channel.open = true;
  agentSays({ curtain: {}, silent: {} });
  switches.reopened();
  assert.deepEqual(channel.delivered, [{ t: 'curtain', on: true }]);
  assert.equal(button('curtain').getAttribute('aria-busy'), 'true');

  agentSays({ curtain: { on: true }, silent: {} });
  assert.equal(button('curtain').getAttribute('aria-checked'), 'true');
  assert.equal(button('curtain').getAttribute('aria-busy'), 'false');
  assert.deepEqual(switches.state, { curtain: { on: true }, silent: {} });

  // Nothing waits now, so the next channel is asked nothing.
  switches.reopened();
  assert.equal(channel.delivered.length, 1);
});

test('a click on an open channel goes at once, and an error ends the wait', () => {
  channel.delivered.length = 0;
  button('silent').click();
  assert.deepEqual(channel.delivered, [{ t: 'silent', on: true }]);
  agentSays({ curtain: { on: true }, silent: { error: 'no null sink' } });
  assert.equal(button('silent').getAttribute('aria-busy'), 'false');
  const errors = overlay.section.children.filter((el) => el.className === 'switch-error');
  assert.equal(errors.length, 1);
  assert.equal(errors[0].textContent, 'Quiet host speakers: no null sink');
});
