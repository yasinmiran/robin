// Sharp text: which of the agent's lossless tiles may be shown over the
// video, from the manifests that come before each frame and the frames the
// video shows, as internal/protocol/tiles.go defines them. A tile shows
// only over a frame whose manifest, and those of every frame between the
// tile's and it, the page has: a frame on screen that is newer than every
// manifest hides them all until its manifest arrives. Nothing here touches
// the DOM.

export const TILE_SIZE = 64;
export const MAX_TILE_BATCH = 256;
export const TILE_ATLAS_COLUMNS = 16;
export const TILE_BATCH_LIMIT = 4 * 1048576;
// MAX_TILE_GRID bounds each side of a grid: the largest canvas side a
// browser draws.
export const MAX_TILE_GRID = 16384;
// MAX_WAITING_BATCHES bounds the batches that wait for their frame, each
// holding a decoded atlas: the agent keeps two under way, but a hidden tab
// reports no frames while they keep coming.
export const MAX_WAITING_BATCHES = 8;
// TILE_HORIZON is how far, in 90 kHz RTP ticks, a batch's frame may come
// after the newest manifest: two seconds. A batch is cut from a frame
// already written, whose manifest went first.
export const TILE_HORIZON = 180000;

const HEAD = 10; // RTP, generation and count, before the index
// A PNG starts with its signature, then the IHDR chunk: its type at 12 and
// the picture's width and height, big-endian, at 16 and 20.
const PNG_SIGNATURE = [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a];
const IHDR = [0x49, 0x48, 0x44, 0x52];
const PNG_HEAD = 24;

const isUint32 = (v) => Number.isInteger(v) && v >= 0 && v <= 0xffffffff;
const isSide = (v) => Number.isInteger(v) && v >= 1 && v <= MAX_TILE_GRID;

// after reports whether RTP timestamp a comes later than b, across the
// 32-bit wrap.
export function after(a, b) {
  return ((a - b) | 0) > 0;
}

// atlasSize is the size in pixels of the atlas of n tiles.
export function atlasSize(n) {
  return { width: Math.min(n, TILE_ATLAS_COLUMNS) * TILE_SIZE, height: Math.ceil(n / TILE_ATLAS_COLUMNS) * TILE_SIZE };
}

// parseTileBatch reads a tile transfer's payload: the RTP timestamp of the
// frame its tiles show, their grid's generation, their indexes in atlas
// order, and the PNG atlas, which must declare the atlas's size before
// anything decodes it.
export function parseTileBatch(bytes) {
  if (!(bytes instanceof Uint8Array) || bytes.length < HEAD) throw new Error('malformed tile batch');
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const n = view.getUint16(8);
  if (n === 0 || n > MAX_TILE_BATCH || bytes.length < HEAD + 4 * n) throw new Error(`malformed tile batch of ${n} tiles`);
  const tiles = [];
  for (let k = 0; k < n; k++) tiles.push(view.getUint32(HEAD + 4 * k));
  const at = HEAD + 4 * n;
  const png = bytes.subarray(at);
  const { width, height } = atlasSize(n);
  const starts = (sig, from) => sig.every((b, i) => png[from + i] === b);
  if (png.length < PNG_HEAD || !starts(PNG_SIGNATURE, 0) || !starts(IHDR, 12) || view.getUint32(at + 16) !== width || view.getUint32(at + 20) !== height) {
    throw new Error(`malformed tile batch: the PNG is not a ${width}x${height} atlas of ${n} tiles`);
  }
  return { rtp: view.getUint32(0), gen: view.getUint32(4), tiles, png };
}

// checkGone throws unless gone is half-open ranges of a grid of total
// tiles: every number an integer greater than the one before, the first at
// least 0 and the last at most total.
function checkGone(gone, total) {
  if (!Array.isArray(gone) || gone.length % 2 !== 0) throw new Error('malformed tile manifest');
  let prev = -1;
  for (const i of gone) {
    if (!Number.isInteger(i) || i <= prev) throw new Error('malformed tile manifest');
    prev = i;
  }
  if (prev > total) throw new Error('malformed tile manifest');
}

// TileBook keeps the page's account of tiles: the grid, which tiles are on
// the canvas, which wait for the video to reach their frame, and whether
// the canvas may be seen. Its methods return what the canvas must do.
export class TileBook {
  gen = null;
  width = 0;
  height = 0;
  #cols = 0;
  #latest = null; // RTP of the newest manifest
  #onScreen = null; // RTP of the frame the video shows
  #gone = new Map(); // tile -> RTP of the latest frame that changed it
  #drawn = new Map(); // tile -> RTP of the batch it was drawn from
  #waiting = []; // batches whose frame the video has not shown yet

  // visible says the canvas may be seen: the video shows a frame whose
  // manifest has arrived.
  get visible() {
    return this.#onScreen !== null && this.#latest !== null && !after(this.#onScreen, this.#latest);
  }

  // drawn is how many tiles are on the canvas.
  get drawn() {
    return this.#drawn.size;
  }

  // has reports whether tile i is on the canvas.
  has(i) {
    return this.#drawn.has(i);
  }

  // entries lists the tiles on the canvas, each with the RTP timestamp of
  // the batch it came from.
  entries() {
    return [...this.#drawn];
  }

  // rect is tile i's place in the desktop.
  rect(i) {
    const x = (i % this.#cols) * TILE_SIZE;
    const y = Math.floor(i / this.#cols) * TILE_SIZE;
    return { x, y, w: Math.min(TILE_SIZE, this.width - x), h: Math.min(TILE_SIZE, this.height - y) };
  }

  // manifest takes one frame's manifest. It returns { reset, clear }: reset
  // when the grid is new and the whole canvas goes, at width by height;
  // otherwise clear lists the tiles to wipe now. A tile drawn from that
  // frame or a later one stays: the manifest came late, behind the batch
  // that already shows the change. Batches that wait are dropped when their
  // grid goes. A manifest outside the bounds protocol.Frame states throws,
  // and changes nothing.
  manifest(frame) {
    const { rtp, gen, w, h } = frame ?? {};
    if (!isUint32(rtp) || !isUint32(gen) || !isSide(w) || !isSide(h)) throw new Error('malformed tile manifest');
    const gone = frame.gone ?? [];
    checkGone(gone, Math.ceil(w / TILE_SIZE) * Math.ceil(h / TILE_SIZE));
    this.#latest = rtp;
    if (gen !== this.gen || w !== this.width || h !== this.height) {
      this.gen = gen;
      this.width = w;
      this.height = h;
      this.#cols = Math.ceil(w / TILE_SIZE);
      this.#gone.clear();
      this.#drawn.clear();
      this.#release(this.#waiting);
      this.#waiting = [];
      return { reset: true, clear: [] };
    }
    const clear = [];
    for (let k = 0; k < gone.length; k += 2) {
      for (let i = gone[k]; i < gone[k + 1]; i++) {
        this.#gone.set(i, rtp);
        const drawn = this.#drawn.get(i);
        if (drawn === undefined || !after(rtp, drawn)) continue;
        this.#drawn.delete(i);
        clear.push(i);
      }
    }
    return { reset: false, clear };
  }

  // batch takes a parsed tile batch with its decoded atlas in batch.image.
  // It returns what is due now, as frame does; a batch of another grid
  // comes straight back as spent, and so do the oldest that wait once more
  // than MAX_WAITING_BATCHES do. One that names a tile outside the grid, or
  // a tile twice, or a frame more than TILE_HORIZON past the newest
  // manifest, throws, and changes nothing.
  batch(b) {
    if (b.gen !== this.gen) return { draws: [], spent: [b.image] };
    const total = this.#cols * Math.ceil(this.height / TILE_SIZE);
    if (!b.tiles.every((i) => Number.isInteger(i) && i >= 0 && i < total) || new Set(b.tiles).size !== b.tiles.length) {
      throw new Error('malformed tile batch: a tile outside the grid, or twice');
    }
    if (after(b.rtp, this.#latest + TILE_HORIZON)) throw new Error('malformed tile batch: a frame far past every manifest');
    this.#waiting.push(b);
    const due = this.#due();
    while (this.#waiting.length > MAX_WAITING_BATCHES) due.spent.push(this.#waiting.shift().image);
    return due;
  }

  // frame takes the RTP timestamp of the frame the video now shows. It
  // returns { draws, spent }: draws are { image, sx, sy, x, y, w, h }, the
  // atlas square to copy and where it goes, and spent the atlases to close
  // once they are drawn.
  frame(rtp) {
    this.#onScreen = rtp;
    return this.#due();
  }

  // reset forgets everything, for a new peer or when tiles go off.
  reset() {
    this.gen = null;
    this.width = this.height = this.#cols = 0;
    this.#latest = this.#onScreen = null;
    this.#gone.clear();
    this.#drawn.clear();
    this.#release(this.#waiting);
    this.#waiting = [];
  }

  #due() {
    const draws = [];
    const spent = [];
    if (this.#onScreen === null) return { draws, spent };
    const later = [];
    for (const b of this.#waiting) {
      if (after(b.rtp, this.#onScreen)) {
        later.push(b);
        continue;
      }
      b.tiles.forEach((i, k) => {
        const gone = this.#gone.get(i);
        if (gone !== undefined && after(gone, b.rtp)) return;
        const drawn = this.#drawn.get(i);
        if (drawn !== undefined && after(drawn, b.rtp)) return;
        this.#drawn.set(i, b.rtp);
        const r = this.rect(i);
        draws.push({ image: b.image, sx: (k % TILE_ATLAS_COLUMNS) * TILE_SIZE, sy: Math.floor(k / TILE_ATLAS_COLUMNS) * TILE_SIZE, ...r });
      });
      spent.push(b.image);
    }
    this.#waiting = later;
    return { draws, spent };
  }

  #release(batches) {
    for (const b of batches) b.image?.close?.();
  }
}
