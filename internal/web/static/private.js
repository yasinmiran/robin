// The private session's switches in the status tab: black out the screen
// at the host, quiet its speakers. They appear once the agent says the
// attached desktop can have them, and show what the agent reports, not
// what was clicked.

import { onControl } from './control.js';
import { shown } from './hosttext.js';
import { next, request, requests, settle, view } from './privatestate.js';
import { switchButton } from './switches.js';

export class PrivateSwitches {
  #overlay;
  #send;
  #section = null;
  #state = null;
  #pending = {};

  // send delivers a control message to the agent.
  constructor(overlay, send) {
    this.#overlay = overlay;
    this.#send = send;
    onControl('private', (c) => this.#apply(c.private));
  }

  // state is the private session as the agent last reported it.
  get state() {
    return this.#state;
  }

  // reopened sends the agent the switches still waiting on it, once a
  // control channel opens: a click while the page reconnected stays busy
  // until the new peer connection carries it.
  reopened() {
    for (const r of requests(this.#pending)) this.#send(r);
  }

  #apply(state) {
    this.#state = state ?? null;
    settle(this.#pending, this.#state);
    this.#render();
  }

  #toggle(key) {
    const on = next(key, this.#state, this.#pending);
    this.#pending[key] = on;
    this.#send(request(key, on));
    this.#render();
  }

  #render() {
    const items = view(this.#state, this.#pending);
    if (!items.length && !this.#section) return;
    this.#section ??= this.#overlay.addSection('private');
    const row = document.createElement('div');
    row.className = 'switches';
    const errors = [];
    for (const item of items) {
      row.append(switchButton(item, (key) => this.#toggle(key)));
      if (item.error) {
        const p = document.createElement('p');
        p.className = 'switch-error';
        p.setAttribute('role', 'alert');
        p.textContent = `${item.label}: ${shown(item.error)}`;
        errors.push(p);
      }
    }
    this.#section.replaceChildren(row, ...errors);
  }
}
