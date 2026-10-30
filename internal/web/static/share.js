// Sharing in the status tab. On the owner's page: invite an account on the
// host by name, show the command it joins with, and list the guests, each
// with control to give or take back and a button that removes it; the
// tab's row says who watches. On a guest's page: the row says whose
// desktop this is and whether the guest has control, and a line under it
// says how to get control. Both show what the agent reports, not what was
// clicked.

import { onControl } from './control.js';
import {
  guestActions, guestRequest, guestStatus, guestsChanged, guestsSummary, holderOf, inviteRequest, joinCommand, nextExpiry,
  openInvitations, sentence, shareStatus, until,
} from './sharestate.js';

const BAD_NAME = 'Enter the name of an account on the host';
const COPY = 'Copy join command';
const COPY_FAILED = 'Copy failed: the command is selected, press Ctrl+C';
// CONFIRM_MS is how long Remove waits for the click that confirms it, and
// COPIED_MS how long Copy says it copied.
const CONFIRM_MS = 3000;
const COPIED_MS = 1500;

export class Sharing {
  #overlay;
  #send;
  #onShare;
  #host = '';
  #section;
  #guests = null; // protocol.Guests, on the owner's page
  #share = null; // protocol.Share, on a guest's page
  #error = ''; // as the agent or the page put it
  // errorAt is the guest whose change the error is about, or null when it
  // is about an invite; pending is what the owner asked last, the same way.
  #errorAt = null;
  #pending = null;
  #expiry = 0;
  #later;
  #confirming = null; // the Remove waiting for its second click: { id, cancel }
  // The owner's parts that outlive a render, so a name being typed keeps
  // its focus when the guests change, and the guest's status line, which
  // a screen reader announces only if it was there before its text.
  #owner = null;
  #status = null;

  // send delivers a control message to the agent. onShare(share) hears of
  // a guest page's share each time it changes. later(fn, ms) runs fn after
  // ms and returns what cancels it.
  constructor(overlay, send, { onShare = () => {}, later = runLater } = {}) {
    this.#overlay = overlay;
    this.#send = send;
    this.#onShare = onShare;
    this.#later = later;
    // The section has its place in the tab from the start, empty, so the
    // tab's parts do not move with the order messages come in.
    this.#section = this.#build();
    onControl('guests', (c) => {
      const before = this.#guests;
      this.#guests = c.guests ?? null;
      this.#error = this.#guests?.error ?? '';
      this.#errorAt = this.#error ? this.#pending : null;
      this.#overlay.setGuests(guestsSummary(this.#guests?.list));
      // Someone joining, going or getting control is news the owner sees,
      // whether or not the tab was out.
      if (guestsChanged(before, this.#guests)) this.#overlay.reveal();
      this.#render();
    });
    onControl('share', (c) => this.#setShare(c.share ?? null));
  }

  // configure takes the CLI's config: the host as the owner named it, for
  // the join command, and on a guest's page the share it starts with.
  configure(config) {
    this.#host = config?.host ?? '';
    if (config?.share) this.#setShare(config.share);
  }

  // guest says whether this is a guest's page.
  get guest() {
    return this.#share !== null;
  }

  // owner is the name of the desktop's owner, on a guest's page.
  get owner() {
    return this.#share?.owner ?? '';
  }

  // state is what the agent last reported, for tests.
  get state() {
    return { guests: this.#guests, share: this.#share, error: this.#error };
  }

  // invite asks the agent to invite the account user, and says whether it
  // was asked.
  invite(user) {
    const request = inviteRequest(user);
    this.#error = request ? '' : BAD_NAME;
    this.#errorAt = null;
    this.#pending = null;
    if (request) this.#send(request);
    this.#render();
    return Boolean(request);
  }

  // change asks the agent to grant, revoke or remove guest id.
  change(id, action) {
    this.#pending = id;
    this.#send(guestRequest(id, action));
  }

  #setShare(share) {
    const was = this.#share;
    this.#share = share;
    this.#render();
    this.#onShare(share);
    // Getting or losing control is news the guest must see before they
    // reach for the desktop.
    if (was && Boolean(was.control) !== Boolean(share?.control)) this.#overlay.reveal();
  }

  #render() {
    if (!this.#guests && !this.#share) return;
    if (this.#share) this.#renderGuest();
    else this.#renderOwner();
  }

  #build() {
    const section = this.#overlay.addSection('share');
    section.dataset.keys = 'local';
    // The tab stays out while the owner types a name, and may tuck once
    // the focus leaves.
    section.addEventListener('focusin', () => section.setAttribute('data-pinned', ''));
    section.addEventListener('focusout', () => {
      section.removeAttribute('data-pinned');
      this.#overlay.reveal();
    });
    return section;
  }

  #renderGuest() {
    const status = shareStatus(this.#share);
    if (!this.#status) {
      this.#status = el('p', 'share-status');
      this.#status.setAttribute('role', 'status');
      this.#section.replaceChildren(this.#status);
    }
    this.#status.textContent = status.text;
    this.#status.dataset.control = String(status.control);
    this.#overlay.setRole(status.role, !status.control);
  }

  #renderOwner() {
    this.#owner ??= this.#buildOwner();
    const { input, error, lists } = this.#owner;
    const formError = this.#errorAt === null ? sentence(this.#error) : '';
    error.textContent = formError;
    error.hidden = !formError;
    if (formError) {
      input.setAttribute('aria-invalid', 'true');
      input.setAttribute('aria-describedby', error.id);
    } else {
      input.removeAttribute('aria-invalid');
      input.removeAttribute('aria-describedby');
    }
    const parts = [];
    const invitations = openInvitations(this.#guests);
    if (invitations.length) {
      const list = el('ul', 'share-invitations');
      for (const inv of invitations) list.append(this.#invitation(inv));
      parts.push(list);
    }
    const guests = this.#guests?.list ?? [];
    if (guests.length) {
      const list = el('ul', 'share-guests');
      const holder = holderOf(guests);
      for (const g of guests) list.append(this.#guest(g, holder));
      parts.push(list);
    }
    lists.replaceChildren(...parts);
    this.#scheduleExpiry();
  }

  #buildOwner() {
    const form = document.createElement('form');
    form.className = 'share-invite';
    const label = el('label', 'share-label', 'Share');
    label.htmlFor = 'share-user';
    const input = document.createElement('input');
    input.type = 'text';
    input.id = 'share-user';
    input.placeholder = 'Account on the host';
    input.autocomplete = 'off';
    input.spellcheck = false;
    input.addEventListener('input', () => {
      input.removeAttribute('aria-invalid');
      input.removeAttribute('aria-describedby');
    });
    const invite = document.createElement('button');
    invite.type = 'submit';
    invite.id = 'share-invite';
    invite.textContent = 'Invite';
    form.append(label, input, invite);
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      if (this.invite(input.value)) input.value = '';
    });
    const error = el('p', 'share-error');
    error.id = 'share-error';
    error.setAttribute('role', 'alert');
    const lists = el('div', 'share-lists');
    this.#section.replaceChildren(form, error, lists);
    return { input, error, lists };
  }

  // invitation is one open invitation: its user, a button that copies the
  // command it runs, when it expires, and the command itself as text.
  #invitation(inv) {
    const li = el('li', 'share-invitation');
    li.dataset.user = inv.user;
    const command = el('code', 'share-command', joinCommand(inv, this.#host));
    command.title = command.textContent;
    const copy = document.createElement('button');
    copy.type = 'button';
    copy.dataset.copy = '';
    copy.textContent = COPY;
    copy.title = `Copy the command ${inv.user} runs to join`;
    copy.addEventListener('click', () => {
      copy.blur();
      this.#copy(copy, command);
    });
    li.append(named(inv.user), copy, el('span', 'share-until', until(inv.expires)), command);
    return li;
  }

  // copy puts the command on the clipboard and says so; where the browser
  // will not, it selects the command for the owner to copy by hand.
  #copy(button, command) {
    const copied = () => {
      button.textContent = 'Copied';
      this.#later(() => { button.textContent = COPY; }, COPIED_MS);
    };
    const failed = () => {
      globalThis.getSelection?.()?.selectAllChildren(command);
      const { error } = this.#owner;
      error.textContent = COPY_FAILED;
      error.hidden = false;
    };
    const write = navigator.clipboard?.writeText(command.textContent);
    if (!write) {
      failed();
      return Promise.resolve();
    }
    return write.then(copied, failed);
  }

  // guest is guest g's row; holder is the guest with control, if another.
  #guest(g, holder) {
    const li = el('li', 'share-guest');
    li.dataset.id = g.id;
    li.dataset.control = String(Boolean(g.control));
    li.append(named(g.user), el('span', 'share-state', guestStatus(g)));
    for (const { action, label } of guestActions(g, holder)) {
      const b = document.createElement('button');
      b.type = 'button';
      b.dataset.action = action;
      b.textContent = label;
      if (action === 'remove' && this.#confirming?.id === g.id) {
        b.textContent = `Remove ${g.user}?`;
        b.dataset.confirm = '';
      }
      b.addEventListener('click', () => {
        b.blur();
        if (action === 'remove') this.#remove(g.id);
        else this.change(g.id, action);
      });
      li.append(b);
    }
    if (this.#error && this.#errorAt === g.id) li.append(el('p', 'share-error', sentence(this.#error)));
    return li;
  }

  // remove asks for a second click within CONFIRM_MS, since a guest
  // removed needs a new invitation to come back, then asks the agent.
  #remove(id) {
    const confirmed = this.#confirming?.id === id;
    this.#confirming?.cancel();
    this.#confirming = null;
    if (confirmed) {
      this.change(id, 'remove');
      return;
    }
    const cancel = this.#later(() => {
      this.#confirming = null;
      this.#render();
    }, CONFIRM_MS);
    this.#confirming = { id, cancel };
    this.#render();
  }

  // scheduleExpiry draws the list again once the next invitation expires.
  #scheduleExpiry() {
    clearTimeout(this.#expiry);
    const left = nextExpiry(this.#guests);
    if (left !== null) this.#expiry = setTimeout(() => this.#render(), left + 50);
  }
}

function runLater(fn, ms) {
  const timer = setTimeout(fn, ms);
  return () => clearTimeout(timer);
}

// named is an account's name, cut short where it is long, in full on hover.
function named(user) {
  const name = el('span', 'share-user', user);
  name.title = user;
  return name;
}

// el is an element of tag with class name and, given, its text: names come
// from the host and the owner, so they go in as text, never as markup.
function el(tag, name, text) {
  const e = document.createElement(tag);
  e.className = name;
  if (text !== undefined) e.textContent = text;
  return e;
}
