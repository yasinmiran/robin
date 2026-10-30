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

// fresh starts a test on an open channel with both switches off and
// nothing waiting: control.js allows one PrivateSwitches, so every test
// shares this one.
function fresh() {
  channel.open = true;
  agentSays({ curtain: {}, silent: {} });
  channel.delivered.length = 0;
}

test('a switch clicked while the page reconnects waits for the next control channel', () => {
  fresh();
  assert.equal(button('curtain').getAttribute('aria-checked'), 'false');

  channel.open = false; // the CLI is redialing; there is no peer
  button('curtain').click();
  assert.deepEqual(channel.delivered, [], 'nothing could carry the click');
  assert.equal(button('curtain').getAttribute('aria-busy'), 'true', 'the switch waits, and shows it');

  // In app.js's order: the new peer's channel opens, and only then does
  // the agent tell its state, which does not answer the click.
  channel.open = true;
  switches.reopened();
  assert.deepEqual(channel.delivered, [{ t: 'curtain', on: true }]);
  agentSays({ curtain: {}, silent: {} });
  assert.equal(button('curtain').getAttribute('aria-busy'), 'true', 'the click still waits');
  assert.equal(channel.delivered.length, 1, 'and is not sent again');

  agentSays({ curtain: { on: true }, silent: {} });
  assert.equal(button('curtain').getAttribute('aria-checked'), 'true');
  assert.equal(button('curtain').getAttribute('aria-busy'), 'false');
  assert.deepEqual(switches.state, { curtain: { on: true }, silent: {} });

  // Nothing waits now, so the next channel is asked nothing.
  switches.reopened();
  assert.equal(channel.delivered.length, 1);
});

test('a click on an open channel goes at once, and an error ends the wait', () => {
  fresh();
  button('silent').click();
  assert.deepEqual(channel.delivered, [{ t: 'silent', on: true }]);
  agentSays({ curtain: {}, silent: { error: 'no null sink' } });
  assert.equal(button('silent').getAttribute('aria-busy'), 'false');
  const errors = overlay.section.children.filter((el) => el.className === 'switch-error');
  assert.equal(errors.length, 1);
  assert.equal(errors[0].textContent, 'Quiet host speakers: no null sink');
});

test('an error from the host shows the marks that reorder text', () => {
  fresh();
  agentSays({ curtain: { error: 'no gamma on ‮FDP' }, silent: {} });
  const errors = overlay.section.children.filter((el) => el.className === 'switch-error');
  assert.equal(errors.length, 1);
  assert.equal(errors[0].textContent, 'Black out host screen: no gamma on \\u202eFDP');
});
