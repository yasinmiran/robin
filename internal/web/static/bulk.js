// Bulk transfers between the page and the agent, one data channel each,
// in the frames bulkframe.js defines. Features register the kinds they
// send with registerKind and the kinds they take with onBulk.

import { BULK_CHANNEL, CHUNK_SIZE, Sum, checkHeader, parseFrame } from './bulkframe.js';

// A sender keeps between LOW_WATER and HIGH_WATER bytes queued, as the
// agent does.
const HIGH_WATER = 1024 * 1024;
const LOW_WATER = 256 * 1024;
const MAX_REASON = 1024;

const receivers = new Map(); // kind -> { accept, inbound, supersede }
// underWay holds, by kind, the transfers from the agent that hold a place
// under the kind's inbound limit, oldest first.
const underWay = new Map();

// BulkRefused is the far side turning a transfer down; reason is in its
// own words.
export class BulkRefused extends Error {
  constructor(reason) {
    super(`bulk transfer refused: ${reason}`);
    this.name = 'BulkRefused';
    this.reason = reason;
  }
}

// onBulk registers accept for transfers of kind from the agent, of which
// the agent may have inbound open to the page at once: a positive integer,
// or Infinity for a kind something else bounds. A header past that is
// refused before accept sees it or, when supersede is set, ends the
// oldest transfer of the kind still under way to take its place. accept
// is called with the header and returns a sink, or throws to refuse; its
// message reaches the agent. A sink has write(chunk), commit() and
// abort(err), each of which may return a promise: writes come in order,
// then commit once every byte arrived intact, its throw refusing the
// transfer after all, or abort with the reason it failed. Frames wait in
// memory while a sink is slow, since a page cannot stop a data channel
// reading.
export function onBulk(kind, accept, { inbound, supersede = false } = {}) {
  if (receivers.has(kind)) throw new Error(`bulk: kind ${JSON.stringify(kind)} taken twice`);
  if (!(Number.isSafeInteger(inbound) && inbound > 0) && inbound !== Infinity) {
    throw new Error(`bulk: kind ${JSON.stringify(kind)} needs how many may be inbound at once, not ${inbound}`);
  }
  receivers.set(kind, { accept, inbound, supersede });
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
  new Inbound(channel, (kind) => receivers.get(kind));
}

// Inbound is the receiving end of one transfer.
export class Inbound {
  #channel;
  #lookup;
  #steps = Promise.resolve();
  #sink = null;
  #sum = null;
  #over = false; // a verdict went out or the channel closed
  #ended = false; // the sink was handed its commit or abort
  #held = null; // the places of the kind, while this transfer holds one

  constructor(channel, lookup) {
    this.#channel = channel;
    this.#lookup = lookup;
    channel.addEventListener('message', (e) => this.#later(() => this.message(e.data)));
    channel.addEventListener('close', () => this.#later(() => this.closed()));
  }

  // later runs step once the steps before it are done: sinks may be slow,
  // and a transfer's frames are handled one at a time, in order.
  #later(step) {
    this.#steps = this.#steps.then(step);
    return this.#steps;
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
    this.#ended = true;
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
    this.#release();
    await this.#abort(new Error('bulk transfer cancelled'));
  }

  async #open(data) {
    if (typeof data !== 'string') throw new Error('malformed bulk frame: data before the header');
    const h = parseFrame(data);
    checkHeader(h);
    const receiver = this.#lookup(h.kind);
    if (!receiver) throw new Error(`unknown bulk kind ${JSON.stringify(h.kind)}: this page takes none`);
    const pushedOut = this.#hold(h.kind, receiver);
    if (pushedOut) {
      // The transfer it ended hears first, so at most inbound sinks of the
      // kind are open at once.
      await pushedOut;
      if (this.#over) return; // pushed out in turn meanwhile
    }
    this.#sink = await receiver.accept(h);
    this.#sum = new Sum(h.size ?? 0);
  }

  async #fail(err) {
    // The verdict goes first: a sink's abort may take a while.
    this.#reply({ t: 'error', error: reasonOf(err) });
    await this.#abort(err);
  }

  // abort hands the sink err, unless it has none yet or already ended.
  async #abort(err) {
    if (this.#sink === null || this.#ended) return;
    this.#ended = true;
    try {
      await this.#sink.abort(err);
    } catch (abortErr) {
      console.warn('robin: bulk abort', abortErr);
    }
  }

  // hold takes one of the kind's places before accept runs, so two headers
  // cannot both take the last one while it does. A kind that supersedes
  // takes the oldest's place when none is free, and hold returns a promise
  // that settles once that transfer's sink has heard.
  #hold(kind, { inbound, supersede }) {
    let held = underWay.get(kind);
    if (!held) underWay.set(kind, (held = new Set()));
    let pushedOut = null;
    if (held.size >= inbound) {
      if (!supersede) throw new Error(`too many ${kind} transfers under way: the page takes ${inbound} at once`);
      pushedOut = held.values().next().value.#supersede(kind);
    }
    held.add(this);
    this.#held = held;
    return pushedOut;
  }

  // supersede ends this transfer for a newer one of kind. The verdict and
  // the place go at once; the sink hears in turn with the frames, so its
  // abort never overlaps a write or follows a commit.
  #supersede(kind) {
    const err = new Error(`superseded by a newer ${kind} transfer`);
    this.#reply({ t: 'error', error: err.message });
    return this.#later(() => this.#abort(err));
  }

  #release() {
    this.#held?.delete(this);
    this.#held = null;
  }

  #reply(verdict) {
    if (this.#over) return;
    this.#over = true;
    this.#release();
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
