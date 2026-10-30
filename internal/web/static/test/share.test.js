import test from 'node:test';
import assert from 'node:assert/strict';
import { FakeElement, installDom } from './dom.js';
import { dispatchControl } from '../control.js';
import { Sharing } from '../share.js';
import {
  WHY_GUEST_LEFT, guestActions, guestRequest, guestStatus, guestsChanged, guestsSummary, holderOf, inviteRequest, joinCommand,
  leftNotice, nextExpiry, openInvitations, sentence, shareStatus, until,
} from '../sharestate.js';
import { Session } from '../session.js';

installDom();

// overlay hands out a section per name, as Overlay.addSection does, and
// keeps what the row was told and how often the tab came out.
function overlayOf() {
  const sections = new Map();
  return {
    sections,
    reveals: 0,
    guests: '',
    role: null,
    addSection(name) {
      if (sections.has(name)) throw new Error(`section ${name} added twice`);
      const s = new FakeElement('section');
      sections.set(name, s);
      return s;
    },
    reveal() { this.reveals++; },
    setGuests(text) { this.guests = text; },
    setRole(label, viewOnly) { this.role = { label, viewOnly }; },
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
  assert.equal(until('2026-10-30T12:10:00Z'),
    `until ${new Date('2026-10-30T12:10:00Z').toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })}`);
});

test('the owner has control to give and take back, and a guest is told which it has', () => {
  assert.deepEqual(guestActions({ control: false }).map((a) => a.action), ['grant', 'remove']);
  assert.deepEqual(guestActions({ control: true }).map((a) => a.action), ['revoke', 'remove']);
  assert.equal(guestActions({ control: false }, 'alice')[0].label, 'Give control (takes it from alice)');
  assert.equal(guestActions({ control: true }, 'alice')[0].label, 'Take back control');
  assert.equal(holderOf([{ user: 'bob' }, { user: 'alice', control: true }]), 'alice');
  assert.equal(holderOf(undefined), '');
  assert.equal(guestStatus({ control: true }), 'Has control');
  assert.equal(guestStatus({}), 'Watching');
  assert.equal(shareStatus(null), null);
  assert.deepEqual(shareStatus({ owner: 'robin' }), {
    title: 'robin’s desktop', control: false, text: 'View only until robin gives you control', role: 'Watching robin’s desktop',
  });
  assert.equal(shareStatus({ owner: 'robin', control: true }).text, 'You have control');
  assert.equal(shareStatus({ owner: 'robin', control: true }).role, 'Controlling robin’s desktop');
  assert.equal(shareStatus({}).title, 'the owner’s desktop');
});

test('the row says who watches, and who has control', () => {
  assert.equal(guestsSummary([]), '');
  assert.equal(guestsSummary(undefined), '');
  assert.equal(guestsSummary([{ user: 'alice' }, { user: 'bob' }]), 'alice, bob watching');
  assert.equal(guestsSummary([{ user: 'alice', control: true }]), 'alice has control');
  assert.equal(guestsSummary([{ user: 'alice', control: true }, { user: 'bob' }]), 'alice has control, bob watching');
  const many = ['a', 'b', 'c', 'd'].map((user) => ({ user }));
  assert.equal(guestsSummary(many), '4 guests watching');
  const list = (control) => ({ list: [{ id: 'g1', user: 'alice', control }] });
  assert.equal(guestsChanged(null, list(false)), true, 'a guest joined');
  assert.equal(guestsChanged(list(false), list(false)), false);
  assert.equal(guestsChanged(list(false), list(true)), true, 'a guest got control');
  assert.equal(guestsChanged(list(true), { list: [] }), true, 'a guest went');
});

test('a guest that left or was let go is told whose desktop, and how to come back', () => {
  assert.deepEqual(leftNotice('', 'robin'), ['You left robin’s desktop', 'Ask robin for a new invitation to watch again.']);
  assert.deepEqual(leftNotice(WHY_GUEST_LEFT, 'robin')[0], 'You left robin’s desktop');
  assert.deepEqual(leftNotice('the owner removed you from the session', 'robin'), [
    'Removed from robin’s desktop', 'The owner removed you from the session. Ask robin for a new invitation to watch again.',
  ]);
  assert.equal(leftNotice('', '')[0], 'You left the owner’s desktop');
});

test('a reason reads as a sentence', () => {
  assert.equal(sentence('the owner removed you from the session'), 'The owner removed you from the session.');
  assert.equal(sentence('Done!'), 'Done!');
  assert.equal(sentence('  '), '');
  assert.equal(sentence(undefined), '');
});

test('names and reasons from the host show the marks that reorder text', () => {
  const rlo = '\u202e';
  const esc = '\\u202e';
  assert.equal(shareStatus({ owner: `bob${rlo}` }).title, `bob${esc}’s desktop`);
  assert.deepEqual(leftNotice(`removed${rlo}`, `bob${rlo}`), [
    `Removed from bob${esc}’s desktop`, `Removed${esc}. Ask bob${esc} for a new invitation to watch again.`,
  ]);
  assert.equal(sentence(`no user ${rlo}eci`), `No user ${esc}eci.`);
  const list = [{ id: 'g1', user: `al${rlo}ice`, control: true }, { id: 'g2', user: `ca${rlo}rol` }];
  assert.equal(guestsSummary(list), `al${esc}ice has control, ca${esc}rol watching`);
  assert.equal(guestActions(list[1], holderOf(list))[0].label, `Give control (takes it from al${esc}ice)`);
  assert.equal(joinCommand({ code: `c${rlo}1`, user: `al${rlo}ice` }, 'h'), `robin gui --join c${esc}1 al${esc}ice@h`);
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
// runAfter keeps what the page would run after a while, for a test to run.
const pending = [];
const runAfter = (fn, ms) => {
  const entry = { fn, ms, cancelled: false };
  pending.push(entry);
  return () => { entry.cancelled = true; };
};
const runPending = (ms) => {
  for (const e of pending.splice(0)) if (e.ms === ms && !e.cancelled) e.fn();
};
const sharing = new Sharing(overlay, (m) => sent.push(m), { later: runAfter });
const section = () => overlay.sections.get('share');
const find = (match) => section()?.find(match);
const byClass = (name) => (el) => el.className === name;
const all = (root, match, out = []) => {
  if (match(root)) out.push(root);
  for (const c of root.children ?? []) all(c, match, out);
  return out;
};

test('the owner page keeps the section in its place, empty, until the agent reports sharing', () => {
  sharing.configure({ host: 'box.lan' });
  assert.ok(section(), 'the section is reserved from the start');
  assert.equal(section().children.length, 0);
  assert.equal(sharing.guest, false);
  assert.equal(sharing.owner, '');
});

test('the owner invites by name and sees the command, its guests and their control', () => {
  dispatchControl({ t: 'guests', guests: { list: [], invitations: [] } });
  const form = find((el) => el.tagName === 'FORM');
  const input = find((el) => el.id === 'share-user');
  assert.ok(form && input, 'the invite form is there');
  assert.equal(section().dataset.keys, 'local', 'typing a name goes to the page, not the desktop');
  const label = find((el) => el.tagName === 'LABEL');
  assert.equal(label.textContent, 'Share');
  assert.equal(label.htmlFor, 'share-user', 'the field has a label that stays once typed in');
  assert.equal(overlay.guests, '', 'nobody watches yet');

  input.value = 'not a name!';
  form.dispatchEvent(Object.assign(new Event('submit'), { preventDefault() {} }));
  assert.deepEqual(sent, []);
  assert.equal(find(byClass('share-error')).textContent, 'Enter the name of an account on the host.');
  assert.equal(find(byClass('share-error')).hidden, false);
  assert.equal(input.getAttribute('aria-invalid'), 'true');
  assert.equal(input.getAttribute('aria-describedby'), 'share-error');
  input.dispatchEvent(new Event('input'));
  assert.equal(input.hasAttribute('aria-invalid'), false, 'typing clears the mark');

  input.value = 'alice';
  form.dispatchEvent(Object.assign(new Event('submit'), { preventDefault() {} }));
  assert.deepEqual(sent, [{ t: 'invite', invite: { user: 'alice' } }]);
  assert.equal(input.value, '', 'the name is cleared once asked');
  assert.equal(find(byClass('share-error')).hidden, true);
  dispatchControl({ t: 'guests', guests: { list: [], invitations: [], error: 'no user alcie on this host' } });
  assert.equal(find(byClass('share-error')).textContent, 'No user alcie on this host.', 'the reason reads as a sentence');
  assert.equal(sharing.state.error, 'no user alcie on this host', 'the state keeps what the agent said');

  dispatchControl({ t: 'guests', guests: {
    list: [], invitations: [{ user: 'alice', code: 'a1b2-1000-xyz', expires: later(600_000) }],
  } });
  assert.equal(find(byClass('share-command')).textContent, 'robin gui --join a1b2-1000-xyz alice@box.lan');
  assert.equal(find((el) => el.id === 'share-user'), input, 'the input survived the render');

  const reveals = overlay.reveals;
  dispatchControl({ t: 'guests', guests: { list: [{ id: 'g1', user: '<b>alice</b>' }], invitations: [] } });
  assert.equal(overlay.reveals, reveals + 1, 'a guest joining brings the tab out');
  assert.equal(overlay.guests, '<b>alice</b> watching', 'the row says who watches');
  const row = find((el) => el.dataset.id === 'g1');
  assert.equal(row.children[0].textContent, '<b>alice</b>', 'a name goes in as text');
  assert.equal(row.children[0].title, '<b>alice</b>', 'the whole name shows on hover');
  assert.equal(row.children[0].children.length, 0);
  assert.equal(row.dataset.control, 'false');
  sent.length = 0;
  row.find((el) => el.dataset.action === 'grant').click();
  assert.deepEqual(sent, [{ t: 'guest', guest: { id: 'g1', action: 'grant' } }]);
  assert.equal(row.dataset.control, 'false', 'the row shows what the agent says, not the click');
  dispatchControl({ t: 'guests', guests: { list: [{ id: 'g1', user: 'alice' }], invitations: [], error: 'no guest "g1"' } });
  assert.equal(find(byClass('share-error')).hidden, true, 'a change that failed is not the form’s');
  const failed = find((el) => el.dataset.id === 'g1').find(byClass('share-error'));
  assert.equal(failed.textContent, 'No guest "g1".', 'it says so under the row it is about');
  assert.equal(overlay.reveals, reveals + 1, 'the same guests are no news');

  dispatchControl({ t: 'guests', guests: { list: [{ id: 'g1', user: 'alice', control: true }, { id: 'g2', user: 'bob' }], invitations: [] } });
  assert.equal(overlay.reveals, reveals + 2, 'a guest getting control brings the tab out');
  assert.equal(overlay.guests, 'alice has control, bob watching');
  const again = find((el) => el.dataset.id === 'g1');
  assert.equal(again.dataset.control, 'true');
  assert.equal(again.find(byClass('share-state')).textContent, 'Has control');
  assert.equal(find((el) => el.dataset.id === 'g2').find((el) => el.dataset.action === 'grant').textContent,
    'Give control (takes it from alice)');
  sent.length = 0;
  again.find((el) => el.dataset.action === 'revoke').click();
  assert.deepEqual(sent.map((m) => m.guest.action), ['revoke']);
});

test('Remove asks for a second click, and a first one lapses', () => {
  const remove = () => find((el) => el.dataset.id === 'g1').find((el) => el.dataset.action === 'remove');
  sent.length = 0;
  remove().click();
  assert.deepEqual(sent, [], 'one click removes nobody');
  assert.equal(remove().textContent, 'Remove alice?');
  assert.equal(remove().hasAttribute('data-confirm') || 'confirm' in remove().dataset, true);
  dispatchControl({ t: 'guests', guests: { list: [{ id: 'g1', user: 'alice', control: true }, { id: 'g2', user: 'bob' }], invitations: [] } });
  assert.equal(remove().textContent, 'Remove alice?', 'the guests changing keeps the question');
  find((el) => el.dataset.id === 'g2').find((el) => el.dataset.action === 'remove').click();
  assert.equal(remove().textContent, 'Remove', 'asking about another guest drops the first question');
  find((el) => el.dataset.id === 'g2').find((el) => el.dataset.action === 'remove').click();
  assert.deepEqual(sent, [{ t: 'guest', guest: { id: 'g2', action: 'remove' } }]);

  remove().click();
  runPending(3000);
  assert.equal(remove().textContent, 'Remove', 'unconfirmed, it goes back');
  remove().click();
  remove().click();
  assert.deepEqual(sent.at(-1), { t: 'guest', guest: { id: 'g1', action: 'remove' } });

  dispatchControl({ t: 'guests', guests: { list: [], invitations: [] } });
  assert.equal(all(section(), (el) => el.className === 'share-guest').length, 0);
  assert.equal(sharing.state.guests.list.length, 0);
  assert.equal(overlay.guests, '', 'nobody watches any more');
});

test('the tab stays out while the owner types a name, and may tuck after', () => {
  section().dispatchEvent(new Event('focusin'));
  assert.equal(section().hasAttribute('data-pinned'), true);
  const reveals = overlay.reveals;
  section().dispatchEvent(new Event('focusout'));
  assert.equal(section().hasAttribute('data-pinned'), false);
  assert.equal(overlay.reveals, reveals + 1, 'dropping the pin lets the tab tuck again');
});

test('an invitation drops off the list once it expires', async () => {
  dispatchControl({ t: 'guests', guests: { list: [], invitations: [{ user: 'bob', code: 'c-1-d', expires: later(30) }] } });
  assert.equal(all(section(), byClass('share-invitation')).length, 1);
  await new Promise((resolve) => setTimeout(resolve, 150));
  assert.equal(all(section(), byClass('share-invitation')).length, 0);
});

test('a copy button copies the command and says so, or selects it', async () => {
  const copied = [];
  Object.defineProperty(globalThis, 'navigator', {
    value: { clipboard: { writeText: async (t) => { copied.push(t); } } }, configurable: true,
  });
  dispatchControl({ t: 'guests', guests: { list: [], invitations: [{ user: 'bob', code: 'c-1-d', expires: later(600_000) }] } });
  const copy = () => find((el) => 'copy' in el.dataset);
  copy().focus();
  copy().click();
  assert.equal(document.activeElement, document.body, 'the keyboard goes back to the desktop');
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(copied, ['robin gui --join c-1-d bob@box.lan']);
  assert.equal(copy().textContent, 'Copied');
  runPending(1500);
  assert.equal(copy().textContent, 'Copy join command');

  const selected = [];
  globalThis.getSelection = () => ({ selectAllChildren: (el) => selected.push(el.textContent) });
  Object.defineProperty(globalThis, 'navigator', {
    value: { clipboard: { writeText: async () => { throw new Error('denied'); } } }, configurable: true,
  });
  copy().click();
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(selected, ['robin gui --join c-1-d bob@box.lan'], 'the command is selected to copy by hand');
  assert.equal(find(byClass('share-error')).textContent, 'Copy failed: the command is selected, press Ctrl+C');
  Object.defineProperty(globalThis, 'navigator', { value: {}, configurable: true });
  selected.length = 0;
  copy().click();
  assert.equal(selected.length, 1, 'a page with no clipboard selects it too');
  delete globalThis.getSelection;
});

// Last, since a config with a share makes this page a guest's for good.
test('a guest page says whose desktop it is and follows its control', () => {
  sharing.configure({ share: { owner: 'robin' } });
  assert.equal(sharing.guest, true);
  assert.equal(sharing.owner, 'robin');
  const status = find(byClass('share-status'));
  assert.equal(status.textContent, 'View only until robin gives you control');
  assert.equal(status.dataset.control, 'false');
  assert.equal(status.getAttribute('role'), 'status');
  assert.deepEqual(overlay.role, { label: 'Watching robin’s desktop', viewOnly: true }, 'the row says it, not Connected');
  const reveals = overlay.reveals;
  dispatchControl({ t: 'share', share: { owner: 'robin', control: true } });
  assert.equal(find(byClass('share-status')), status, 'one status line, whose text changes');
  assert.equal(status.textContent, 'You have control');
  assert.deepEqual(overlay.role, { label: 'Controlling robin’s desktop', viewOnly: false });
  assert.equal(overlay.reveals, reveals + 1, 'getting control brings the tab out');
  dispatchControl({ t: 'share', share: { owner: 'robin', control: true } });
  assert.equal(overlay.reveals, reveals + 1, 'the same share is no news');
  dispatchControl({ t: 'share', share: { owner: 'robin' } });
  assert.equal(status.dataset.control, 'false');
  assert.equal(overlay.reveals, reveals + 2, 'losing control brings the tab out');
  assert.equal(find((el) => el.tagName === 'FORM'), null, 'a guest invites nobody');
});
