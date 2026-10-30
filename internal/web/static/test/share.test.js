import test from 'node:test';
import assert from 'node:assert/strict';
import { FakeElement, installDom } from './dom.js';
import { dispatchControl } from '../control.js';
import { Sharing } from '../share.js';
import {
  guestActions, guestRequest, guestStatus, inviteRequest, joinCommand, nextExpiry, openInvitations, sentence, shareStatus, until,
} from '../sharestate.js';
import { Session } from '../session.js';

installDom();

// overlay hands out a section per name, as Overlay.addSection does.
function overlayOf() {
  const sections = new Map();
  return {
    sections,
    addSection(name) {
      if (sections.has(name)) throw new Error(`section ${name} added twice`);
      const s = new FakeElement('section');
      sections.set(name, s);
      return s;
    },
    reveal() {},
  };
}

const later = (ms) => new Date(Date.now() + ms).toISOString();

test('an invite names an account the host could have', () => {
  assert.deepEqual(inviteRequest('  alice '), { t: 'invite', invite: { user: 'alice' } });
  assert.deepEqual(inviteRequest('svc.robin-2'), { t: 'invite', invite: { user: 'svc.robin-2' } });
  for (const bad of ['', '   ', '-n', 'a b', 'a:b', 'a/b', 'x'.repeat(33), null, undefined]) {
    assert.equal(inviteRequest(bad), null, JSON.stringify(bad));
  }
  assert.deepEqual(guestRequest('g1', 'grant'), { t: 'guest', guest: { id: 'g1', action: 'grant' } });
});

test('only invitations still open are shown, and the next expiry is known', () => {
  const now = Date.parse('2026-10-30T12:00:00Z');
  const guests = {
    invitations: [
      { user: 'alice', code: 'a-1-x', expires: '2026-10-30T12:10:00Z' },
      { user: 'bob', code: 'b-1-y', expires: '2026-10-30T11:59:59Z' },
      { user: 'carol', expires: '2026-10-30T12:05:00Z' },
      { user: 'dave', code: 'd-1-z', expires: '2026-10-30T12:03:00Z' },
    ],
  };
  assert.deepEqual(openInvitations(guests, now).map((i) => i.user), ['alice', 'dave']);
  assert.equal(nextExpiry(guests, now), 3 * 60 * 1000);
  assert.equal(nextExpiry({ invitations: [] }, now), null);
  assert.deepEqual(openInvitations(null, now), []);
  assert.equal(joinCommand({ code: 'a-1-x', user: 'alice' }, 'box.lan'), 'robin gui --join a-1-x alice@box.lan');
  assert.equal(joinCommand({ code: 'a-1-x', user: 'alice' }, ''), 'robin gui --join a-1-x alice@host');
  assert.match(until('2026-10-30T12:10:00Z'), /^until \d\d:\d0$/);
});

test('the owner has control to give and take back, and a guest is told which it has', () => {
  assert.deepEqual(guestActions({ control: false }).map((a) => a.action), ['grant', 'remove']);
  assert.deepEqual(guestActions({ control: true }).map((a) => a.action), ['revoke', 'remove']);
  assert.equal(guestStatus({ control: true }), 'Has control');
  assert.equal(guestStatus({}), 'Watching');
  assert.equal(shareStatus(null), null);
  assert.deepEqual(shareStatus({ owner: 'robin' }), {
    title: 'robin’s desktop', control: false, text: 'View only until robin gives you control',
  });
  assert.equal(shareStatus({ owner: 'robin', control: true }).text, 'You have control');
  assert.equal(shareStatus({}).title, 'the owner’s desktop');
});

test('a reason reads as a sentence', () => {
  assert.equal(sentence('the owner removed you from the session'), 'The owner removed you from the session.');
  assert.equal(sentence('Done!'), 'Done!');
  assert.equal(sentence('  '), '');
  assert.equal(sentence(undefined), '');
});

test('a guest leaving is its own notice, whatever closed the socket', () => {
  const s = new Session();
  s.configure({ share: { owner: 'robin' } });
  assert.equal(s.canKeep, false);
  assert.equal(s.closeReason(true), 'closed', 'a plain close');
  s.end();
  assert.equal(s.closeReason(true), 'left');
  s.ended();
  assert.equal(s.closeReason(true), 'left');
  assert.equal(s.closeReason(false), 'refused');
});

// One Sharing for the file, as a page has one: control.js takes each
// type's handler once. The tests run in order on it.
const sent = [];
const overlay = overlayOf();
const sharing = new Sharing(overlay, (m) => sent.push(m));
const section = () => overlay.sections.get('share');
const find = (match) => section()?.find(match);
const byClass = (name) => (el) => el.className === name;
const all = (root, match, out = []) => {
  if (match(root)) out.push(root);
  for (const c of root.children ?? []) all(c, match, out);
  return out;
};

test('the owner page shows nothing until the agent reports sharing', () => {
  sharing.configure({ host: 'box.lan' });
  assert.equal(section(), undefined);
  assert.equal(sharing.guest, false);
});

test('the owner invites by name and sees the command, its guests and their control', () => {
  dispatchControl({ t: 'guests', guests: { list: [], invitations: [] } });
  const form = find((el) => el.tagName === 'FORM');
  const input = find((el) => el.id === 'share-user');
  assert.ok(form && input, 'the invite form is there');
  assert.equal(section().dataset.keys, 'local', 'typing a name goes to the page, not the desktop');

  input.value = 'not a name!';
  form.dispatchEvent(Object.assign(new Event('submit'), { preventDefault() {} }));
  assert.deepEqual(sent, []);
  assert.equal(find(byClass('share-error')).textContent, 'Enter the name of an account on the host');
  assert.equal(find(byClass('share-error')).hidden, false);

  input.value = 'alice';
  form.dispatchEvent(Object.assign(new Event('submit'), { preventDefault() {} }));
  assert.deepEqual(sent, [{ t: 'invite', invite: { user: 'alice' } }]);
  assert.equal(input.value, '', 'the name is cleared once asked');
  assert.equal(find(byClass('share-error')).hidden, true);

  dispatchControl({ t: 'guests', guests: {
    list: [], invitations: [{ user: 'alice', code: 'a1b2-1000-xyz', expires: later(600_000) }],
  } });
  assert.equal(find(byClass('share-command')).textContent, 'robin gui --join a1b2-1000-xyz alice@box.lan');
  assert.equal(find((el) => el.id === 'share-user'), input, 'the input survived the render');

  dispatchControl({ t: 'guests', guests: {
    list: [{ id: 'g1', user: '<b>alice</b>' }], invitations: [], error: 'no user bob on this host',
  } });
  assert.equal(find(byClass('share-error')).textContent, 'no user bob on this host');
  const row = find((el) => el.dataset.id === 'g1');
  assert.equal(row.children[0].textContent, '<b>alice</b>', 'a name goes in as text');
  assert.equal(row.children[0].children.length, 0);
  assert.equal(row.dataset.control, 'false');
  sent.length = 0;
  row.find((el) => el.dataset.action === 'grant').click();
  assert.deepEqual(sent, [{ t: 'guest', guest: { id: 'g1', action: 'grant' } }]);
  assert.equal(row.dataset.control, 'false', 'the row shows what the agent says, not the click');

  dispatchControl({ t: 'guests', guests: { list: [{ id: 'g1', user: 'alice', control: true }], invitations: [] } });
  const again = find((el) => el.dataset.id === 'g1');
  assert.equal(again.dataset.control, 'true');
  assert.equal(again.find(byClass('share-state')).textContent, 'Has control');
  sent.length = 0;
  again.find((el) => el.dataset.action === 'revoke').click();
  again.find((el) => el.dataset.action === 'remove').click();
  assert.deepEqual(sent.map((m) => m.guest.action), ['revoke', 'remove']);

  dispatchControl({ t: 'guests', guests: { list: [], invitations: [] } });
  assert.equal(all(section(), (el) => el.className === 'share-guest').length, 0);
  assert.equal(sharing.state.guests.list.length, 0);
});

test('the tab stays out while the owner types a name', () => {
  section().dispatchEvent(new Event('focusin'));
  assert.equal(section().hasAttribute('data-pinned'), true);
  section().dispatchEvent(new Event('focusout'));
  assert.equal(section().hasAttribute('data-pinned'), false);
});

test('an invitation drops off the list once it expires', async () => {
  dispatchControl({ t: 'guests', guests: { list: [], invitations: [{ user: 'bob', code: 'c-1-d', expires: later(30) }] } });
  assert.equal(all(section(), byClass('share-invitation')).length, 1);
  await new Promise((resolve) => setTimeout(resolve, 150));
  assert.equal(all(section(), byClass('share-invitation')).length, 0);
});

test('a copy button copies the command', async () => {
  const copied = [];
  Object.defineProperty(globalThis, 'navigator', {
    value: { clipboard: { writeText: async (t) => { copied.push(t); } } }, configurable: true,
  });
  dispatchControl({ t: 'guests', guests: { list: [], invitations: [{ user: 'bob', code: 'c-1-d', expires: later(600_000) }] } });
  find((el) => el.tagName === 'BUTTON' && el.textContent === 'Copy').click();
  assert.deepEqual(copied, ['robin gui --join c-1-d bob@box.lan']);
});

// Last, since a config with a share makes this page a guest's for good.
test('a guest page says whose desktop it is and follows its control', () => {
  sharing.configure({ share: { owner: 'robin' } });
  assert.equal(sharing.guest, true);
  const status = find(byClass('share-status'));
  assert.equal(find(byClass('share-head')).textContent, 'robin’s desktop');
  assert.equal(status.textContent, 'View only until robin gives you control');
  assert.equal(status.dataset.control, 'false');
  dispatchControl({ t: 'share', share: { owner: 'robin', control: true } });
  assert.equal(find(byClass('share-status')).textContent, 'You have control');
  dispatchControl({ t: 'share', share: { owner: 'robin' } });
  assert.equal(find(byClass('share-status')).dataset.control, 'false');
  assert.equal(find((el) => el.tagName === 'FORM'), null, 'a guest invites nobody');
});
