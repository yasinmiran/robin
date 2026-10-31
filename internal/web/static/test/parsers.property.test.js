// Property tests of the page's parsers of what the agent sends: random
// and mutated input, and the invariants each parser promises. The agent's
// Go side has the same properties as fuzz targets.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { BULK_WINDOW, CHUNK_SIZE, Grant, Sum, checkHeader, kindLimit, parseFrame, registerKind } from '../bulkframe.js';
import { MAX_CLIP, decodeClip, encodeClip } from '../clip.js';
import { dispatchControl, onControl } from '../control.js';
import { MAX_TILE_BATCH, MAX_TILE_GRID, TILE_SIZE, TileBook, atlasSize, parseTileBatch } from '../tiles.js';
import { forAll, refusedOr } from './property.js';
import { marshal, pngHead } from './tilebatch.js';

registerKind('property', 4096);

const utf8 = new TextEncoder();

// frames are bulk frames as the agent writes them, to mutate.
const frames = [
  { t: 'header', kind: 'property', id: '7', name: 'a.txt', mime: 'text/plain', size: 3 },
  { t: 'header', kind: 'property' },
  { t: 'end', sha256: 'ab' },
  { t: 'done' },
  { t: 'error', error: 'no' },
  { t: 'credit', credit: 1048576 },
];

test('parseFrame takes only JSON objects within a chunk', () => {
  forAll((r) => {
    const text = r.int(3) === 0
      ? JSON.stringify(r.value())
      : new TextDecoder().decode(r.mutate(utf8.encode(JSON.stringify(r.pick(frames)))));
    const got = refusedOr(() => parseFrame(text));
    if (got.refused) return;
    assert.ok(text.length <= CHUNK_SIZE, 'took a frame past a chunk');
    assert.ok(got.value !== null && typeof got.value === 'object', `took ${text}`);
  });
  for (const junk of [undefined, 7, null, {}, new Uint8Array(3)]) assert.throws(() => parseFrame(junk), /malformed/);
});

test('checkHeader passes only a header of a registered kind within its limit', () => {
  forAll((r) => {
    const h = r.bool() ? { ...r.pick(frames) } : r.value();
    if (h && typeof h === 'object' && r.bool()) {
      h.t = r.pick(['header', 'end', r.string()]);
      h.kind = r.pick(['property', 'nobody', '__proto__', r.string()]);
      h.size = r.pick([r.edge(), r.int(5000), String(r.int(10)), undefined]);
    }
    if (refusedOr(() => checkHeader(h)).refused) return;
    const size = h.size ?? 0;
    assert.equal(h.t, 'header');
    assert.ok(Number.isSafeInteger(size) && size >= 0 && size <= kindLimit(h.kind), `passed ${JSON.stringify(h)}`);
  });
});

test('Sum checks exactly the declared bytes, as SHA-256 hashes them', () => {
  forAll((r) => {
    // A receiver makes a Sum only of a size checkHeader passed.
    const size = r.int(3) === 0 ? r.pick([0, 1, 64, 255, 256]) : r.int(300);
    const payload = r.bytes(r.int(400));
    const sum = new Sum(size);
    let passed = 0;
    for (let at = 0; at < payload.length;) {
      const n = r.int(80);
      const chunk = payload.subarray(at, at + n);
      if (refusedOr(() => sum.add(chunk)).refused) {
        assert.ok(!(passed + chunk.length <= size), `refused ${chunk.length} after ${passed} of ${size}`);
        break;
      }
      passed += chunk.length;
      at += n;
    }
    assert.ok(!(passed > size), `counted ${passed} of ${size}`);
    const honest = r.int(4) !== 0;
    const sha256 = honest ? createHash('sha256').update(payload.subarray(0, passed)).digest('hex') : '0'.repeat(64);
    const ok = !refusedOr(() => sum.check({ t: 'end', sha256 })).refused;
    assert.equal(ok, honest && !(passed < size), `check of ${passed} of ${size}, honest ${honest}`);
  });
});

test('Grant credits only grow, and data past the credit is refused', () => {
  forAll((r) => {
    const g = new Grant();
    let taken = 0;
    let last = 0;
    let arrived = 0;
    let granted = BULK_WINDOW;
    for (let i = r.int(40); i > 0; i--) {
      const n = r.int(4) * 64 * 1024 + r.int(100);
      if (r.bool()) {
        arrived += n;
        const refused = refusedOr(() => g.arrive(n)).refused;
        assert.equal(Boolean(refused), arrived > granted, `${arrived} arrived of ${granted}`);
        if (refused) return;
      } else {
        taken += n;
        const c = g.take(n);
        if (c === null) continue;
        assert.ok(c.t === 'credit' && c.credit === taken + BULK_WINDOW && c.credit > last, JSON.stringify(c));
        last = c.credit;
        granted = c.credit;
      }
    }
  });
});

// clips are clipboard contents as the agent sends them, to mutate.
const clips = [{ text: 'hello' }, { text: 'a', html: '<b>a</b>', png: new Uint8Array([0x89, 0x50, 0x4e, 0x47]) }, { html: '<i>x</i>' }, { png: new Uint8Array([1, 2, 3]) }];

test('decodeClip takes only what round-trips through encodeClip', () => {
  forAll((r) => {
    const bytes = r.int(4) === 0 ? r.bytes(r.int(40)) : r.mutate(encodeClip(r.pick(clips)));
    const got = refusedOr(() => decodeClip(bytes));
    if (got.refused) return;
    const clip = got.value;
    const size = utf8.encode(clip.text ?? '').length + utf8.encode(clip.html ?? '').length + (clip.png?.length ?? 0);
    assert.ok(size <= MAX_CLIP && size <= bytes.length, `decoded ${size} bytes from ${bytes.length}`);
    assert.deepEqual(decodeClip(encodeClip(clip)), clip);
  });
});

test('parseTileBatch takes only a batch that marshals back to its bytes', () => {
  forAll((r) => {
    const n = 1 + r.int(40);
    const { width, height } = atlasSize(n);
    const good = marshal({ rtp: r.u32(), gen: r.u32(), tiles: Array.from({ length: n }, () => r.u32()), png: pngHead(width, height) });
    const bytes = r.int(5) === 0 ? r.bytes(r.int(64)) : r.mutate(good);
    const got = refusedOr(() => parseTileBatch(bytes));
    if (got.refused) return;
    const b = got.value;
    assert.ok(b.tiles.length >= 1 && b.tiles.length <= MAX_TILE_BATCH);
    assert.deepEqual([...marshal(b)], [...bytes]);
  });
});

// book is a TileBook and a record of what it told the canvas.
const atlasOf = (name) => ({ name, close() {} });
const names = (out) => ({ ...out, draws: out.draws?.map((d) => ({ ...d, image: d.image.name })), spent: out.spent?.map((i) => i.name) });

// step makes one input for a TileBook: a manifest, a batch or a frame,
// mostly within the bounds protocol.Frame states and sometimes not.
function step(r, rtp) {
  const w = r.int(6) === 0 ? r.edge() : 1 + r.int(300);
  const h = r.int(6) === 0 ? r.edge() : 1 + r.int(200);
  const total = Math.ceil(w / TILE_SIZE) * Math.ceil(h / TILE_SIZE);
  switch (r.int(3)) {
    case 0: {
      const gone = [];
      for (let i = r.int(4), at = 0; i > 0; i--) {
        const a = at + r.int(4);
        gone.push(a, a + 1 + r.int(4));
        at = a + 5;
      }
      if (r.int(5) === 0 && gone.length) gone[r.int(gone.length)] = r.pick([r.edge(), -1, total + 1, 'x']);
      const gen = r.int(5) === 0 ? r.edge() : 1 + r.int(2);
      return { kind: 'manifest', arg: () => ({ rtp, gen, w, h, gone: [...gone] }) };
    }
    case 1: {
      const tiles = Array.from({ length: 1 + r.int(6) }, () => (r.int(6) === 0 ? r.edge() : r.int(Math.max(total, 1) + 1)));
      const at = rtp - r.int(3) * 3000 + (r.int(8) === 0 ? 200000 : 0);
      const gen = r.int(4) === 0 ? 2 : 1;
      const name = `b${r.u32()}`;
      return { kind: 'batch', arg: () => ({ rtp: at >>> 0, gen, tiles: [...tiles], image: atlasOf(name) }) };
    }
    default: {
      const at = (rtp - r.int(3) * 3000) >>> 0;
      return { kind: 'frame', arg: () => at };
    }
  }
}

test('a TileBook that refuses an input is as if it never had it', () => {
  forAll((r) => {
    // a takes every input; b only those a takes. Refused inputs must not
    // change a, so the two answer every input alike.
    const a = new TileBook();
    const b = new TileBook();
    let rtp = r.u32();
    for (let i = 20 + r.int(60); i > 0; i--) {
      rtp = (rtp + r.int(2) * 3000) >>> 0;
      const s = step(r, rtp);
      const got = refusedOr(() => a[s.kind](s.arg()));
      if (got.refused) continue;
      const want = b[s.kind](s.arg());
      assert.deepEqual(names(got.value), names(want), `${s.kind} answered apart`);
      assert.equal(a.visible, b.visible);
      assert.deepEqual(a.entries(), b.entries());
      assert.ok(a.width <= MAX_TILE_GRID && a.height <= MAX_TILE_GRID);
      for (const d of got.value.draws ?? []) {
        assert.ok(d.w > 0 && d.h > 0 && d.x >= 0 && d.y >= 0 && d.x + d.w <= a.width && d.y + d.h <= a.height, `draw ${JSON.stringify(d)}`);
      }
    }
  });
});

test('dispatchControl hands a message only to the handler of its type', () => {
  const got = [];
  onControl('property-a', (m) => got.push(m));
  forAll((r) => {
    got.length = 0;
    const m = r.bool() ? { ...r.value(), t: r.pick(['property-a', '__proto__', 'constructor', 'toString', r.string()]) } : r.value();
    dispatchControl(m);
    assert.equal(got.length, m?.t === 'property-a' ? 1 : 0, `dispatched ${JSON.stringify(m)}`);
  });
});
