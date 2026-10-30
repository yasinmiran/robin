// What the sharing parts of the status tab show, worked out from what the
// agent reports, apart from the DOM so it can be tested: the owner's
// invitations and guests, and a guest's word on whose desktop it watches.

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
  return `robin gui --join ${invitation.code} ${invitation.user}@${host || 'host'}`;
}

// until says, as the owner's clock reads, when an invitation expires.
export function until(expires) {
  const d = new Date(expires);
  return `until ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
}

// guestActions are the owner's buttons for guest g.
export function guestActions(g) {
  const control = g.control
    ? { action: 'revoke', label: 'Take back control' }
    : { action: 'grant', label: 'Give control' };
  return [control, { action: 'remove', label: 'Remove' }];
}

// guestStatus says what guest g may do.
export function guestStatus(g) {
  return g.control ? 'Has control' : 'Watching';
}

// shareStatus is a guest page's banner: whose desktop it shows, and
// whether the guest has control. Null on the owner's page.
export function shareStatus(share) {
  if (!share) return null;
  const owner = share.owner || 'the owner';
  return {
    title: `${owner}’s desktop`,
    control: Boolean(share.control),
    text: share.control ? 'You have control' : `View only until ${owner} gives you control`,
  };
}

// sentence makes a reason the CLI gives, such as "the owner removed you",
// read as a sentence of a notice.
export function sentence(text) {
  const t = String(text ?? '').trim();
  if (!t) return '';
  return t[0].toUpperCase() + t.slice(1) + (/[.!?]$/.test(t) ? '' : '.');
}
