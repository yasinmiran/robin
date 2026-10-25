// Bulk transfers between the page and the agent, one data channel each,
// in the frames bulkframe.js defines. Features register the kinds they
// send with registerKind and the kinds they take with onBulk.

import { BULK_CHANNEL, BULK_WINDOW, CHUNK_SIZE, Grant, Sum, checkHeader, parseFrame } from './bulkframe.js';

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
// transfer after all, or abort with the reason it failed. The agent sends
// no more than the credit the page grants as its sink takes chunks, so a
// slow sink holds at most a window of a transfer in memory.
export function onBulk(kind, accept, { inbound, supersede = false } = {}) {
  if (receivers.has(kind)) throw new Error(`bulk: kind ${JSON.stringify(kind)} taken twice`);
  if (!(Number.isSafeInteger(inbound) && inbound > 0) && inbound !== Infinity) {
    throw new Error(`bulk: kind ${JSON.stringify(kind)} needs how many may be inbound at once, not ${inbound}`);
  }
  receivers.set(kind, { accept, inbound, supersede });
}

// sendBulk sends blob, a Blob or File, as one transfer described by header
// on a channel of its own from pc, and resolves once the agent kept it.
// It sends no further ahead of the agent's sink than its credit, which
// keeps the channel's queue to a window. Aborting signal cancels the
// transfer; progress, when given, hears how many bytes have gone out
// after each chunk.
export async function sendBulk(pc, header, blob, { signal, progress } = {}) {
  const h = { ...header, t: 'header', size: blob.size };
  checkHeader(h);
  signal?.throwIfAborted();
  const channel = pc.createDataChannel(BULK_CHANNEL, { ordered: true });
  channel.binaryType = 'arraybuffer';
  const out = new Outbound(channel, signal);
  try {
    await out.until('open');
    channel.send(JSON.stringify(h));
    const sum = new Sum(h.size);
    for (let at = 0; at < blob.size; at += CHUNK_SIZE) {
      const chunk = new Uint8Array(await blob.slice(at, at + CHUNK_SIZE).arrayBuffer());
      sum.add(chunk);
      await out.credit(at + chunk.length);
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
// sent: its credit, its verdict, a close, or an abort.
class Outbound {
  #channel;
  #reason = null; // DONE for the agent's done, otherwise why it failed
  #stopped;
  #stop;
  #credit = BULK_WINDOW;
  #more = null; // ends the wait for credit

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
      if (v?.t === 'credit' && Number.isSafeInteger(v.credit)) {
        this.#credit = Math.max(this.#credit, v.credit);
        this.#more?.();
      } else if (v?.t === 'done') this.#end(DONE);
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

  // credit waits until the agent lets the sender have sent upTo bytes, and
  // throws if the transfer stops first.
  async credit(upTo) {
    while (this.#reason === null && upTo > this.#credit) {
      await Promise.race([new Promise((resolve) => { this.#more = resolve; }), this.#stopped]);
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
  channel.addEventListener('message', (e) => inbound.receive(e.data));
  channel.addEventListener('close', () => inbound.closed());
}

// Inbound is the receiving end of one transfer. Sinks may be slow, so it
// handles frames one at a time, in order.
export class Inbound {
  #channel;
  #lookup;
  #sink = null;
  #sum = null;
  #grant = new Grant();
  #queue = Promise.resolve();
  #over = false; // a verdict went out or the channel closed
  #ended = false; // the sink was committed or aborted
  #held = null; // the places of the kind, while this transfer holds one

  constructor(channel, lookup) {
    this.#channel = channel;
    this.#lookup = lookup;
  }

  // receive takes the next frame as it arrives, and returns the promise of
  // its handling. Payload counts against the credit at once: a sender past
  // its credit fails the transfer there and then, however long the sink
  // takes over the frames before, and nothing more from it is kept.
  receive(data) {
    if (this.#over || this.#ended) return this.#queue;
    if (typeof data !== 'string') {
      try {
        this.#grant.arrive(data.byteLength);
      } catch (err) {
        this.#reply({ t: 'error', error: reasonOf(err) });
        return this.#then(() => this.#abort(err));
      }
    }
    return this.#then(() => this.#message(data));
  }

  // closed takes the channel's close, once the frames before it are
  // handled.
  closed() {
    return this.#then(async () => {
      if (this.#over) return;
      this.#over = true;
      this.#release();
      await this.#abort(new Error('bulk transfer cancelled'));
    });
  }

  #then(step) {
    this.#queue = this.#queue.then(step);
    return this.#queue;
  }

  async #message(data) {
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
        const credit = this.#grant.take(chunk.length);
        if (credit && !this.#over) this.#send(credit);
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
    return this.#then(() => this.#abort(err));
  }

  #release() {
    this.#held?.delete(this);
    this.#held = null;
  }

  // reply sends the verdict and closes the channel, which ends the
  // transfer and frees its place. Only the first verdict counts.
  #reply(verdict) {
    if (this.#over) return;
    this.#over = true;
    this.#release();
    this.#send(verdict);
    this.#channel.close();
  }

  #send(frame) {
    try {
      this.#channel.send(JSON.stringify(frame));
    } catch {
      // Already closing: the sender learns from the close.
    }
  }
}

function reasonOf(err) {
  return String(err?.message ?? err).slice(0, MAX_REASON);
}
