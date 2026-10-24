// In-memory stand-ins for RTCPeerConnection and RTCDataChannel, enough for
// the bulk layer: ordered delivery, bufferedAmount with its low event, and
// a close that lands after the frames sent before it.

import assert from 'node:assert/strict';
import { acceptBulk } from '../bulk.js';

const later = () => new Promise((resolve) => setTimeout(resolve, 0));

// until waits a while for cond to hold, and fails with what if it never
// does.
export async function until(what, cond) {
  for (let i = 0; i < 100 && !cond(); i++) await later();
  assert.ok(cond(), what);
}

export class FakeChannel extends EventTarget {
  readyState = 'connecting';
  bufferedAmount = 0;
  bufferedAmountLowThreshold = 0;
  binaryType = 'blob';
  peer = null;
  // While paused, frames to the peer wait in the queue.
  paused = false;
  // Every frame this end sent, as delivered.
  sent = [];
  #queue = [];
  #pumping = false;
  #closing = false;

  constructor(label) {
    super();
    this.label = label;
  }

  send(data) {
    if (this.readyState !== 'open') throw new Error('InvalidStateError: channel not open');
    let frame = data;
    if (data instanceof ArrayBuffer) frame = data.slice(0);
    else if (typeof data !== 'string') frame = data.slice().buffer;
    this.sent.push(frame);
    this.bufferedAmount += typeof frame === 'string' ? frame.length : frame.byteLength;
    this.#queue.push(frame);
    this.#pump();
  }

  close() {
    if (this.#closing || this.readyState === 'closed') return;
    this.#closing = true;
    this.readyState = 'closing';
    this.#pump();
  }

  resume() {
    this.paused = false;
    this.#pump();
  }

  async #pump() {
    if (this.#pumping) return;
    this.#pumping = true;
    try {
      while (!this.paused && this.#queue.length) {
        await later();
        if (this.paused) break;
        const frame = this.#queue.shift();
        const before = this.bufferedAmount;
        this.bufferedAmount -= typeof frame === 'string' ? frame.length : frame.byteLength;
        if (this.peer.readyState === 'open' || this.peer.readyState === 'closing') {
          this.peer.dispatchEvent(Object.assign(new Event('message'), { data: frame }));
        }
        if (before > this.bufferedAmountLowThreshold && this.bufferedAmount <= this.bufferedAmountLowThreshold) {
          this.dispatchEvent(new Event('bufferedamountlow'));
        }
      }
      if (this.#closing && !this.#queue.length) {
        await later();
        this.#shut();
        this.peer.#shut();
      }
    } finally {
      this.#pumping = false;
    }
  }

  #shut() {
    if (this.readyState === 'closed') return;
    this.readyState = 'closed';
    this.dispatchEvent(new Event('close'));
  }

  open() {
    this.readyState = 'open';
    this.dispatchEvent(new Event('open'));
  }
}

// FakePeer hands the far end of each channel it creates to onRemote, as
// the agent's side would see it, then opens both.
export class FakePeer {
  channels = [];

  constructor(onRemote) {
    this.onRemote = onRemote;
  }

  createDataChannel(label) {
    const local = new FakeChannel(label);
    const remote = new FakeChannel(label);
    local.peer = remote;
    remote.peer = local;
    this.channels.push(local);
    setTimeout(() => {
      remote.readyState = 'open';
      this.onRemote(remote);
      local.open();
    }, 0);
    return local;
  }
}

// fromAgent opens a channel to the page as the agent would for a transfer
// and sends header on it, leaving the rest of the frames to the caller.
// verdict resolves with the page's answer.
export function fromAgent(header) {
  const page = new FakeChannel('bulk');
  const agent = new FakeChannel('bulk');
  page.peer = agent;
  agent.peer = page;
  page.readyState = agent.readyState = 'open';
  const verdict = new Promise((resolve) => agent.addEventListener('message', (e) => resolve(JSON.parse(e.data)), { once: true }));
  acceptBulk(page);
  agent.send(JSON.stringify({ t: 'header', ...header }));
  return { page, agent, verdict };
}
