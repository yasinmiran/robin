// What the sharing parts of the status tab show, worked out from what the
// agent reports, apart from the DOM so it can be tested: the owner's
// invitations and guests, and a guest's word on whose desktop it watches.
// Names and reasons come from the host, so what is made of them shows the
// marks that reorder text (hosttext.js).

import { shown } from './hosttext.js';

// USER_NAME is what the agent takes for an account on the host.
export const USER_NAME = /^[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}$/;

// inviteRequest is the control message that invites the account named in
// text, or null for text that names no account.
export function inviteRequest(text) {
  const user = String(text ?? '').trim();
  if (!USER_NAME.test(user)) return null;
  return { t: 'invite', invite: { user } };
}

// guestRequest is the control message that makes the owner's change to a
// guest: grant, revoke or remove.
export function guestRequest(id, action) {
  return { t: 'guest', guest: { id, action } };
}

// openInvitations are the invitations that can still be used at now.
export function openInvitations(guests, now = Date.now()) {
  return (guests?.invitations ?? []).filter((i) => i.code && Date.parse(i.expires) > now);
}

// nextExpiry is how long until the first open invitation expires, or null.
export function nextExpiry(guests, now = Date.now()) {
  const left = openInvitations(guests, now).map((i) => Date.parse(i.expires) - now);
  return left.length ? Math.min(...left) : null;
}

// joinCommand is what the invited user runs on their own machine.
export function joinCommand(invitation, host) {
  return `robin gui --join ${shown(invitation.code)} ${shown(invitation.user)}@${host || 'host'}`;
}

// until says, as the owner's clock and locale read it, when an invitation
// expires.
export function until(expires) {
  return `until ${new Date(expires).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })}`;
}

// guestActions are the owner's buttons for guest g, while holder, a name
// or empty, is the guest with control: giving control to another takes it
// from them, and the button says so.
export function guestActions(g, holder = '') {
  let control = { action: 'grant', label: 'Give control' };
  if (g.control) control = { action: 'revoke', label: 'Take back control' };
  else if (holder) control = { action: 'grant', label: `Give control (takes it from ${shown(holder)})` };
  return [control, { action: 'remove', label: 'Remove' }];
}

// holderOf is the name of the guest with control in list, or ''.
export function holderOf(list) {
  return (list ?? []).find((g) => g.control)?.user ?? '';
}

// guestsSummary says who watches the desktop, for the row of the status
// tab, or '' when nobody does. Past three names it counts them.
export function guestsSummary(list) {
  const guests = list ?? [];
  if (!guests.length) return '';
  const holder = shown(holderOf(guests));
  const watching = guests.filter((g) => !g.control).map((g) => shown(g.user));
  const names = watching.length > 3 ? `${watching.length} guests` : watching.join(', ');
  if (!holder) return `${names} watching`;
  return watching.length ? `${holder} has control, ${names} watching` : `${holder} has control`;
}

// guestsChanged says whether the guests, or which of them has control,
// differ between two reports: news the owner must see.
export function guestsChanged(before, after) {
  const key = (gs) => (gs?.list ?? []).map((g) => `${g.id}:${Boolean(g.control)}`).join(' ');
  return key(before) !== key(after);
}

// guestStatus says what guest g may do.
export function guestStatus(g) {
  return g.control ? 'Has control' : 'Watching';
}

// shareStatus is what a guest's page says of its share: whose desktop it
// shows, whether the guest has control, the line under the tab's row, and
// the word that takes Connected's place. Null on the owner's page.
export function shareStatus(share) {
  if (!share) return null;
  const owner = share.owner ? shown(share.owner) : 'the owner';
  const control = Boolean(share.control);
  return {
    title: `${owner}’s desktop`,
    control,
    text: control ? 'You have control' : `View only until ${owner} gives you control`,
    role: `${control ? 'Controlling' : 'Watching'} ${owner}’s desktop`,
  };
}

// WHY_GUEST_LEFT is the reason a guest's session ends with when the guest
// left it, as the agent and the CLI send it (protocol.WhyGuestLeft).
export const WHY_GUEST_LEFT = 'you left the session';

// leftNotice is the title and the body of the notice a guest's page ends
// with: the guest left, or was let go for detail, a reason the CLI gave.
export function leftNotice(detail, owner) {
  const who = owner ? shown(owner) : 'the owner';
  const ask = `Ask ${who} for a new invitation to watch again.`;
  if (!detail || detail === WHY_GUEST_LEFT) return [`You left ${who}’s desktop`, ask];
  return [`Removed from ${who}’s desktop`, `${sentence(detail)} ${ask}`];
}

// sentence makes a reason the CLI gives, such as "the owner removed you",
// read as a sentence of a notice.
export function sentence(text) {
  const t = shown(String(text ?? '').trim());
  if (!t) return '';
  return t[0].toUpperCase() + t.slice(1) + (/[.!?]$/.test(t) ? '' : '.');
}
