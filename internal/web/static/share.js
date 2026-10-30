// Sharing in the status tab. On the owner's page: invite an account on the
// host by name, show the command it joins with, and list the guests, each
// with control to give or take back and a button that removes it. On a
// guest's page: whose desktop this is, and whether the guest has control.
// Both show what the agent reports, not what was clicked.

import { onControl } from './control.js';
import {
  guestActions, guestRequest, guestStatus, inviteRequest, joinCommand, nextExpiry, openInvitations, shareStatus, until,
} from './sharestate.js';

const BAD_NAME = 'Enter the name of an account on the host';

export class Sharing {
  #overlay;
  #send;
  #host = '';
  #section = null;
  #guests = null; // protocol.Guests, on the owner's page
  #share = null; // protocol.Share, on a guest's page
  #error = '';
  #expiry = 0;
  // The owner's parts that outlive a render, so a name being typed keeps
  // its focus when the guests change.
  #owner = null;

  // send delivers a control message to the agent.
  constructor(overlay, send) {
    this.#overlay = overlay;
    this.#send = send;
    onControl('guests', (c) => {
      this.#guests = c.guests ?? null;
      this.#error = this.#guests?.error ?? '';
      this.#render();
    });
    onControl('share', (c) => {
      this.#share = c.share ?? null;
      this.#render();
    });
  }

  // configure takes the CLI's config: the host as the owner named it, for
  // the join command, and on a guest's page the share it starts with.
  configure(config) {
    this.#host = config?.host ?? '';
    if (config?.share) {
      this.#share = config.share;
      this.#render();
    }
  }

  // guest says whether this is a guest's page.
  get guest() {
    return this.#share !== null;
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
    if (request) this.#send(request);
    this.#render();
    return Boolean(request);
  }

  // change asks the agent to grant, revoke or remove guest id.
  change(id, action) {
    this.#send(guestRequest(id, action));
  }

  #render() {
    if (!this.#guests && !this.#share) return;
    this.#section ??= this.#build();
    if (this.#share) this.#renderGuest();
    else this.#renderOwner();
  }

  #build() {
    const section = this.#overlay.addSection('share');
    section.dataset.keys = 'local';
    // The tab stays out while the owner types a name.
    section.addEventListener('focusin', () => section.setAttribute('data-pinned', ''));
    section.addEventListener('focusout', () => section.removeAttribute('data-pinned'));
    return section;
  }

  #renderGuest() {
    const status = shareStatus(this.#share);
    const head = el('p', 'share-head', status.title);
    const line = el('p', 'share-status', status.text);
    line.dataset.control = String(status.control);
    line.setAttribute('role', 'status');
    this.#section.replaceChildren(head, line);
  }

  #renderOwner() {
    this.#owner ??= this.#buildOwner();
    const { error, lists } = this.#owner;
    error.textContent = this.#error;
    error.hidden = !this.#error;
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
      for (const g of guests) list.append(this.#guest(g));
      parts.push(list);
    }
    lists.replaceChildren(...parts);
    this.#scheduleExpiry();
  }

  #buildOwner() {
    const head = el('p', 'share-head', 'Share this desktop');
    const form = document.createElement('form');
    form.className = 'share-invite';
    const input = document.createElement('input');
    input.type = 'text';
    input.id = 'share-user';
    input.placeholder = 'User on the host';
    input.autocomplete = 'off';
    input.spellcheck = false;
    input.setAttribute('aria-label', 'Account on the host to invite');
    const invite = document.createElement('button');
    invite.type = 'submit';
    invite.id = 'share-invite';
    invite.textContent = 'Invite';
    form.append(input, invite);
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      if (this.invite(input.value)) input.value = '';
    });
    const error = el('p', 'share-error');
    error.setAttribute('role', 'alert');
    const lists = el('div', 'share-lists');
    this.#section.replaceChildren(head, form, error, lists);
    return { input, error, lists };
  }

  // invitation is one open invitation: the command its user runs, as
  // text, a button that copies it, and when it expires.
  #invitation(inv) {
    const li = el('li', 'share-invitation');
    li.dataset.user = inv.user;
    const command = el('code', 'share-command', joinCommand(inv, this.#host));
    const copy = document.createElement('button');
    copy.type = 'button';
    copy.textContent = 'Copy';
    copy.title = `Copy the command ${inv.user} runs to join`;
    copy.addEventListener('click', () => {
      navigator.clipboard?.writeText(command.textContent).catch(() => {});
    });
    li.append(el('span', 'share-user', inv.user), command, copy, el('span', 'share-until', until(inv.expires)));
    return li;
  }

  #guest(g) {
    const li = el('li', 'share-guest');
    li.dataset.id = g.id;
    li.dataset.control = String(Boolean(g.control));
    li.append(el('span', 'share-user', g.user), el('span', 'share-state', guestStatus(g)));
    for (const { action, label } of guestActions(g)) {
      const b = document.createElement('button');
      b.type = 'button';
      b.dataset.action = action;
      b.textContent = label;
      b.addEventListener('click', () => {
        b.blur();
        this.change(g.id, action);
      });
      li.append(b);
    }
    return li;
  }

  // scheduleExpiry draws the list again once the next invitation expires.
  #scheduleExpiry() {
    clearTimeout(this.#expiry);
    const left = nextExpiry(this.#guests);
    if (left !== null) this.#expiry = setTimeout(() => this.#render(), left + 50);
  }
}

// el is an element of tag with class name and, given, its text: names come
// from the host and the owner, so they go in as text, never as markup.
function el(tag, name, text) {
  const e = document.createElement(tag);
  e.className = name;
  if (text !== undefined) e.textContent = text;
  return e;
}
