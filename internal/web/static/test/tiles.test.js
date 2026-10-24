import { test } from 'node:test';
import assert from 'node:assert/strict';
import { MAX_TILE_BATCH, TileBook, after, parseTileBatch } from '../tiles.js';

// grid is the manifest that starts a 130x70 desktop: 3x2 tiles, the last
// column and row cut short.
const grid = (rtp, gen = 1) => ({ rtp, gen, w: 130, h: 70 });

// atlas stands in for a decoded ImageBitmap and says whether it was closed.
const atlas = (name) => ({ name, closed: false, close() { this.closed = true; } });

const batch = (rtp, tiles, gen = 1) => ({ rtp, gen, tiles, image: atlas(`b${rtp}`) });

const drawnTiles = (book) => book.entries().map(([i]) => i).sort((a, b) => a - b);

test('after compares RTP timestamps across the wrap', () => {
  assert.equal(after(2, 1), true);
  assert.equal(after(1, 2), false);
  assert.equal(after(5, 5), false);
  assert.equal(after(3, 0xfffffff0), true);
  assert.equal(after(0xfffffff0, 3), false);
});

test('parseTileBatch reads what the agent marshals', () => {
  const b = new Uint8Array([0xde, 0xad, 0xbe, 0xef, 0, 0, 0, 7, 0, 2, 0, 0, 0, 1, 0, 0x10, 0, 0, 0x89, 0x50]);
  const p = parseTileBatch(b);
  assert.equal(p.rtp, 0xdeadbeef);
  assert.equal(p.gen, 7);
  assert.deepEqual(p.tiles, [1, 0x100000]);
  assert.deepEqual([...p.png], [0x89, 0x50]);
  assert.throws(() => parseTileBatch(new Uint8Array(4)), /malformed/);
  assert.throws(() => parseTileBatch(new Uint8Array(10)), /0 tiles/);
  const many = new Uint8Array(10 + 4 * (MAX_TILE_BATCH + 1));
  new DataView(many.buffer).setUint16(8, MAX_TILE_BATCH + 1);
  assert.throws(() => parseTileBatch(many), /malformed/);
  assert.throws(() => parseTileBatch(b.subarray(0, 15)), /2 tiles/);
  assert.throws(() => parseTileBatch([1, 2, 3]), /malformed/);
});

test('a batch is drawn once the video shows its frame, in the atlas order', () => {
  const book = new TileBook();
  assert.deepEqual(book.manifest(grid(100)), { reset: true, clear: [] });
  assert.deepEqual([book.width, book.height, book.gen], [130, 70, 1]);
  const b = batch(100, [5, 0, 2]);
  assert.deepEqual(book.batch(b), { draws: [], spent: [] }, 'nothing on screen yet');
  const { draws, spent } = book.frame(100);
  assert.deepEqual(draws.map(({ image, ...d }) => d), [
    { sx: 0, sy: 0, x: 128, y: 64, w: 2, h: 6 },
    { sx: 64, sy: 0, x: 0, y: 0, w: 64, h: 64 },
    { sx: 128, sy: 0, x: 128, y: 0, w: 2, h: 64 },
  ]);
  assert.equal(draws[0].image, b.image);
  assert.deepEqual(spent, [b.image]);
  assert.equal(book.drawn, 3);
  assert.equal(book.has(5), true);
  assert.equal(book.visible, true);
  assert.deepEqual(book.frame(101), { draws: [], spent: [] }, 'drawn once');
});

test('a batch newer than the frame on screen waits for it', () => {
  const book = new TileBook();
  book.manifest(grid(100));
  book.frame(100);
  book.manifest({ ...grid(200) });
  assert.deepEqual(book.batch(batch(200, [1])).draws, []);
  assert.deepEqual(book.frame(150).draws, []);
  assert.equal(book.frame(200).draws.length, 1);
});

test('the atlas wraps every sixteen tiles', () => {
  const book = new TileBook();
  book.manifest({ rtp: 1, gen: 1, w: 64 * 20, h: 64 });
  book.frame(1);
  const { draws } = book.batch(batch(1, Array.from({ length: 18 }, (_, i) => i)));
  assert.deepEqual([draws[16].sx, draws[16].sy, draws[16].x], [0, 64, 16 * 64]);
  assert.deepEqual([draws[17].sx, draws[17].sy], [64, 64]);
});

test('a manifest clears the tiles its frame changes, at once', () => {
  const book = new TileBook();
  book.manifest(grid(10));
  book.frame(10);
  book.batch(batch(10, [0, 1, 2, 3, 4, 5]));
  assert.deepEqual(book.manifest({ ...grid(20), gone: [1, 3, 5, 6] }), { reset: false, clear: [1, 2, 5] });
  assert.deepEqual(drawnTiles(book), [0, 3, 4]);
  assert.deepEqual(book.manifest({ ...grid(30), gone: [1, 2] }).clear, [], 'only what is drawn is cleared');
});

test('a batch older than a change to its tile draws nothing there', () => {
  const book = new TileBook();
  book.manifest(grid(10));
  book.frame(10);
  // The change's manifest beat the batch cut before it.
  book.manifest({ ...grid(20), gone: [1, 2] });
  book.frame(20);
  const { draws, spent } = book.batch(batch(10, [0, 1]));
  assert.deepEqual(draws.map((d) => d.x), [0]);
  assert.equal(spent.length, 1, 'the atlas is spent all the same');
  // A batch cut after the change, of the frame that made it, is current.
  assert.equal(book.batch(batch(20, [1])).draws.length, 1);
  // An older batch never paints over a newer one.
  assert.deepEqual(book.batch(batch(15, [1])).draws, []);
});

test('the canvas hides while the frame on screen is newer than every manifest', () => {
  const book = new TileBook();
  assert.equal(book.visible, false, 'nothing known');
  book.manifest(grid(10));
  assert.equal(book.visible, false, 'no frame shown yet');
  book.frame(10);
  book.batch(batch(10, [0]));
  assert.equal(book.visible, true);
  // Frame 20's manifest is held up behind a lost packet; the frame is not.
  book.frame(20);
  assert.equal(book.visible, false);
  book.manifest({ ...grid(20), gone: [0, 1] });
  assert.equal(book.visible, true);
  assert.equal(book.drawn, 0, 'the tile it names went before the canvas showed again');
  // Across the 32-bit wrap.
  book.manifest(grid(0xfffffffe));
  book.frame(0xfffffffe);
  assert.equal(book.visible, true);
  book.frame(3);
  assert.equal(book.visible, false);
  book.manifest(grid(3));
  assert.equal(book.visible, true);
});

test('a new grid drops every tile and every batch that waits', () => {
  const book = new TileBook();
  book.manifest(grid(10));
  book.frame(10);
  book.batch(batch(10, [0]));
  const waiting = batch(30, [1]);
  book.batch(waiting);
  assert.deepEqual(book.manifest(grid(20, 2)), { reset: true, clear: [] });
  assert.equal(book.drawn, 0);
  assert.equal(waiting.image.closed, true);
  assert.deepEqual(book.frame(30).draws, []);
  // A batch of the old grid comes back spent.
  const old = batch(30, [1], 1);
  assert.deepEqual(book.batch(old), { draws: [], spent: [old.image] });
  // So does a resize under the same generation.
  book.batch(batch(30, [0], 2));
  assert.equal(book.drawn, 1);
  assert.equal(book.manifest({ rtp: 40, gen: 2, w: 64, h: 64 }).reset, true);
  assert.equal(book.drawn, 0);
});

test('reset forgets everything', () => {
  const book = new TileBook();
  book.manifest(grid(10));
  book.frame(10);
  const waiting = batch(20, [0]);
  book.batch(waiting);
  book.reset();
  assert.equal(book.gen, null);
  assert.equal(book.visible, false);
  assert.equal(waiting.image.closed, true);
  assert.deepEqual(book.frame(20).draws, []);
});
