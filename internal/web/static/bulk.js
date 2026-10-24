// Bulk transfers between the page and the agent, one data channel each,
// in the frames bulkframe.js defines. Features register the kinds they
// send with registerKind and the kinds they take with onBulk.

import { BULK_CHANNEL, CHUNK_SIZE, Sum, checkHeader, parseFrame } from './bulkframe.js';

// A sender keeps between LOW_WATER and HIGH_WATER bytes queued, as the
// agent does.
const HIGH_WATER = 1024 * 1024;
const LOW_WATER = 256 * 1024;
const MAX_REASON = 1024;

const receivers = new Map();

// BulkRefused is the far side turning a transfer down; reason is in its
// own words.
export class BulkRefused extends Error {
  constructor(reason) {
    super(`bulk transfer refused: ${reason}`);
    this.name = 'BulkRefused';
    this.reason = reason;
  }
}

// onBulk registers accept for transfers of kind from the agent. accept is
// called with the header and returns a sink, or throws to refuse; its
// message reaches the agent. A sink has write(chunk), commit() and
// abort(err), each of which may return a promise: writes come in order,
// then commit once every byte arrived intact, its throw refusing the
// transfer after all, or abort with the reason it failed. Frames wait in
// memory while a sink is slow, since a page cannot stop a data channel
// reading.
export function onBulk(kind, accept) {
  if (receivers.has(kind)) throw new Error(`bulk: kind ${JSON.stringify(kind)} taken twice`);
  receivers.set(kind, accept);
}

// sendBulk sends blob, a Blob or File, as one transfer described by header
// on a channel of its own from pc, and resolves once the agent kept it.
// Aborting signal cancels the transfer; progress, when given, hears how
// many bytes have gone out after each chunk.
export async function sendBulk(pc, header, blob, { signal, progress } = {}) {
  const h = { ...header, t: 'header', size: blob.size };
  checkHeader(h);
  signal?.throwIfAborted();
  const channel = pc.createDataChannel(BULK_CHANNEL, { ordered: true });
  channel.binaryType = 'arraybuffer';
  channel.bufferedAmountLowThreshold = LOW_WATER;
  const out = new Outbound(channel, signal);
  try {
    await out.until('open');
    channel.send(JSON.stringify(h));
    const sum = new Sum(h.size);
    for (let at = 0; at < blob.size; at += CHUNK_SIZE) {
      const chunk = new Uint8Array(await blob.slice(at, at + CHUNK_SIZE).arrayBuffer());
      sum.add(chunk);
      while (channel.bufferedAmount > HIGH_WATER) await out.until('bufferedamountlow');
      out.throwIfStopped();
      channel.send(chunk);
      progress?.(at + chunk.length);
    }
    channel.send(JSON.stringify(sum.end()));
    await out.finished();
  } finally {
    channel.close();
  }
}

const DONE = Symbol('done');

// Outbound follows what the agent and the page say about a transfer being
// sent: its verdict, a close, or an abort.
class Outbound {
  #channel;
  #reason = null; // DONE for the agent's done, otherwise why it failed
  #stopped;
  #stop;

  constructor(channel, signal) {
    this.#channel = channel;
    this.#stopped = new Promise((resolve) => { this.#stop = resolve; });
    channel.addEventListener('message', (e) => {
      let v = null;
      try {
        v = parseFrame(e.data);
      } catch {
        // Junk is a refusal like any other.
      }
      if (v?.t === 'done') this.#end(DONE);
      else this.#end(new BulkRefused(v?.t === 'error' ? v.error : 'malformed verdict'));
    });
    channel.addEventListener('close', () => this.#end(new Error('bulk transfer cancelled')));
    signal?.addEventListener('abort', () => this.#end(signal.reason));
  }

  #end(reason) {
    if (this.#reason !== null) return;
    this.#reason = reason;
    this.#stop();
  }

  // until waits for the channel's next event of type, and throws if the
  // transfer stops first.
  async until(type) {
    if (this.#reason === null) {
      await Promise.race([new Promise((resolve) => this.#channel.addEventListener(type, resolve, { once: true })), this.#stopped]);
    }
    this.throwIfStopped();
  }

  // throwIfStopped throws why the transfer cannot go on. Before the end
  // frame, a done is a refusal too.
  throwIfStopped() {
    if (this.#reason === DONE) throw new BulkRefused('receiver finished before the end');
    if (this.#reason !== null) throw this.#reason;
  }

  // finished waits for the verdict on a transfer that was sent in full.
  async finished() {
    await this.#stopped;
    if (this.#reason !== DONE) throw this.#reason;
  }
}

// acceptBulk takes a channel the agent opened for a transfer.
export function acceptBulk(channel) {
  channel.binaryType = 'arraybuffer';
  const inbound = new Inbound(channel, (kind) => receivers.get(kind));
  // Sinks may be slow; frames are handled one at a time, in order.
  let queue = Promise.resolve();
  channel.addEventListener('message', (e) => { queue = queue.then(() => inbound.message(e.data)); });
  channel.addEventListener('close', () => { queue = queue.then(() => inbound.closed()); });
}

// Inbound is the receiving end of one transfer.
export class Inbound {
  #channel;
  #lookup;
  #sink = null;
  #sum = null;
  #over = false; // a verdict went out or the channel closed

  constructor(channel, lookup) {
    this.#channel = channel;
    this.#lookup = lookup;
  }

  async message(data) {
    if (this.#over) return;
    try {
      if (this.#sink === null) {
        await this.#open(data);
        return;
      }
      if (typeof data !== 'string') {
        const chunk = new Uint8Array(data);
        this.#sum.add(chunk);
        await this.#sink.write(chunk);
        return;
      }
      this.#sum.check(parseFrame(data));
    } catch (err) {
      await this.#fail(err);
      return;
    }
    try {
      await this.#sink.commit();
    } catch (err) {
      this.#reply({ t: 'error', error: reasonOf(err) });
      return;
    }
    this.#reply({ t: 'done' });
  }

  async closed() {
    if (this.#over) return;
    this.#over = true;
    await this.#sink?.abort(new Error('bulk transfer cancelled'));
  }

  async #open(data) {
    if (typeof data !== 'string') throw new Error('malformed bulk frame: data before the header');
    const h = parseFrame(data);
    checkHeader(h);
    const accept = this.#lookup(h.kind);
    if (!accept) throw new Error(`unknown bulk kind ${JSON.stringify(h.kind)}: this page takes none`);
    this.#sink = await accept(h);
    this.#sum = new Sum(h.size ?? 0);
  }

  async #fail(err) {
    // The verdict goes first: a sink's abort may take a while.
    this.#reply({ t: 'error', error: reasonOf(err) });
    try {
      await this.#sink?.abort(err);
    } catch (abortErr) {
      console.warn('robin: bulk abort', abortErr);
    }
  }

  #reply(verdict) {
    this.#over = true;
    try {
      this.#channel.send(JSON.stringify(verdict));
    } catch {
      // Already closing: the sender learns from the close.
    }
    this.#channel.close();
  }
}

function reasonOf(err) {
  return String(err?.message ?? err).slice(0, MAX_REASON);
}
