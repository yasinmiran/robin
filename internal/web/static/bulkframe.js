// The frames of a bulk transfer, as internal/protocol/bulk.go defines them:
// on a data channel of its own the sender writes a header, binary chunks
// of at most CHUNK_SIZE bytes and an end frame with the payload's SHA-256,
// and the receiver answers with one verdict, done or error, and closes the
// channel. Either side closing first cancels. The receiver grants credit
// as its sink takes chunks: the sender sends BULK_WINDOW bytes before the
// first and never past the latest. Nothing here touches the DOM.

import { Sha256 } from './sha256.js';

export const BULK_CHANNEL = 'bulk';
export const CHUNK_SIZE = 64 * 1024;
export const BULK_WINDOW = 512 * 1024;

const limits = new Map();

// registerKind declares a kind of transfer and the most bytes one may
// carry, as protocol.RegisterBulkKind does on the agent. The page neither
// sends nor accepts a kind nobody registered.
export function registerKind(kind, limit) {
  if (typeof kind !== 'string' || !kind || !Number.isSafeInteger(limit) || limit < 0) {
    throw new Error(`bulk: bad kind ${JSON.stringify(kind)} with limit ${limit}`);
  }
  if (limits.has(kind)) throw new Error(`bulk: kind ${JSON.stringify(kind)} registered twice`);
  limits.set(kind, limit);
}

// kindLimit is the most bytes a transfer of kind may carry, or undefined
// for a kind nobody registered.
export function kindLimit(kind) {
  return limits.get(kind);
}

// parseFrame decodes a text frame.
export function parseFrame(text) {
  if (typeof text !== 'string' || text.length > CHUNK_SIZE) throw new Error('malformed bulk frame');
  let frame;
  try {
    frame = JSON.parse(text);
  } catch {
    throw new Error('malformed bulk frame');
  }
  if (frame === null || typeof frame !== 'object') throw new Error('malformed bulk frame');
  return frame;
}

// checkHeader throws unless h may open a transfer: a header of a registered
// kind whose size is within the kind's limit.
export function checkHeader(h) {
  if (h?.t !== 'header') throw new Error('malformed bulk frame: not a header');
  const size = h.size ?? 0;
  if (!Number.isSafeInteger(size) || size < 0) throw new Error(`malformed bulk frame: size ${size}`);
  const limit = kindLimit(h.kind);
  if (limit === undefined) throw new Error(`unknown bulk kind ${JSON.stringify(h.kind)}`);
  if (size > limit) throw new Error(`bulk transfer too large: ${size} bytes of ${h.kind}, the limit is ${limit}`);
}

// Sum counts and hashes the chunks of one transfer as they pass, on either
// side of the channel.
export class Sum {
  #size;
  #seen = 0;
  #hash = new Sha256();

  constructor(size) {
    this.#size = size;
  }

  // add takes the next chunk, a Uint8Array.
  add(chunk) {
    if (chunk.length > CHUNK_SIZE) throw new Error(`malformed bulk frame: chunk of ${chunk.length} bytes`);
    if (chunk.length > this.#size - this.#seen) throw new Error(`bulk data past the declared size: ${this.#size} bytes declared`);
    this.#seen += chunk.length;
    this.#hash.update(chunk);
  }

  // end returns the frame that closes the transfer, once every declared
  // byte has passed.
  end() {
    if (this.#seen < this.#size) throw new Error(`bulk transfer ended early: ${this.#seen} of ${this.#size} bytes`);
    return { t: 'end', sha256: this.#hash.hex() };
  }

  // check throws unless frame is the end of exactly the bytes that passed.
  check(frame) {
    if (frame?.t !== 'end') throw new Error('malformed bulk frame: not the end');
    if (frame.sha256 !== this.end().sha256) throw new Error('bulk transfer SHA-256 mismatch');
  }
}

// Grant is a receiver's side of the credit: what it let the sender send,
// what arrived and what its sink took.
export class Grant {
  #granted = BULK_WINDOW;
  #arrived = 0;
  #taken = 0;

  // arrive counts n bytes that arrived, and throws once they run past the
  // credit, which a page that cannot stop a channel reading would
  // otherwise hold in memory.
  arrive(n) {
    this.#arrived += n;
    if (this.#arrived > this.#granted) throw new Error(`bulk data past the credit: ${this.#arrived} bytes with ${this.#granted} granted`);
  }

  // take counts n bytes the sink took, and returns the credit frame to
  // send once half a window has been taken since the last, or null.
  take(n) {
    this.#taken += n;
    if (this.#granted - this.#taken > BULK_WINDOW / 2) return null;
    this.#granted = this.#taken + BULK_WINDOW;
    return { t: 'credit', credit: this.#granted };
  }
}
