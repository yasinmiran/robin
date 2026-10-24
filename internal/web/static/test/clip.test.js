import test from 'node:test';
import assert from 'node:assert/strict';
import {
  CLIP_LIMIT, CONTROL_TEXT, HTML, MAX_CLIP, PNG, TEXT, clipSize, clipTypes, decodeClip, encodeClip, fitsControl, splitPaste,
} from '../clip.js';

const utf8 = new TextEncoder();

function part(mime, data) {
  const m = utf8.encode(mime);
  const out = new Uint8Array(1 + m.length + 4 + data.length);
  out[0] = m.length;
  out.set(m, 1);
  new DataView(out.buffer).setUint32(1 + m.length, data.length);
  out.set(data, 5 + m.length);
  return out;
}

function concat(...arrays) {
  const out = new Uint8Array(arrays.reduce((n, a) => n + a.length, 0));
  let at = 0;
  for (const a of arrays) {
    out.set(a, at);
    at += a.length;
  }
  return out;
}

test('a clip survives the round trip in every mix of formats', () => {
  const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0, 255]);
  for (const clip of [{}, { text: 'héllo ✓' }, { html: '<b>hi</b>' }, { png }, { text: 'a', html: '<i>a</i>', png }]) {
    const back = decodeClip(encodeClip(clip));
    assert.equal(back.text, clip.text);
    assert.equal(back.html, clip.html);
    assert.deepEqual(back.png, clip.png);
  }
});

test('the layout is the agent’s: type length, type, big-endian data length, data, in text, HTML, PNG order', () => {
  const got = encodeClip({ png: new Uint8Array([9, 8]), text: 'a' });
  assert.deepEqual(got, concat(part(TEXT, utf8.encode('a')), part(PNG, new Uint8Array([9, 8]))));
  // TestClipWireShape checks the same layout on the agent.
  assert.deepEqual([...got.subarray(0, 16)], [10, ...utf8.encode('text/plain'), 0, 0, 0, 1, 97]);
});

test('decodeClip skips formats it does not know and refuses junk', () => {
  const clip = decodeClip(concat(part('image/jpeg', new Uint8Array([1, 2])), part(HTML, utf8.encode('<p>'))));
  assert.deepEqual(clip, { html: '<p>' });
  const text = part(TEXT, utf8.encode('hi'));
  for (const [name, bytes, why] of [
    ['short header', new Uint8Array([4, 116, 101]), /part header/],
    ['data past the end', text.subarray(0, text.length - 1), /past the end/],
    ['twice', concat(text, text), /twice/],
    ['text not utf-8', part(TEXT, new Uint8Array([0xff, 0xfe])), /not UTF-8/],
    ['html not utf-8', part(HTML, new Uint8Array([0xc3])), /not UTF-8/],
    ['huge length', new Uint8Array([1, 120, 255, 255, 255, 255]), /past the end/],
  ]) {
    assert.throws(() => decodeClip(bytes), why, name);
  }
});

test('the size bound holds both ways', () => {
  const big = { text: 'x'.repeat(MAX_CLIP), html: 'y' };
  assert.throws(() => encodeClip(big), /limit/);
  const atCap = encodeClip({ text: 'x'.repeat(MAX_CLIP - 1), png: new Uint8Array(1) });
  assert.ok(atCap.length <= CLIP_LIMIT);
  const over = concat(part(TEXT, new Uint8Array(MAX_CLIP)), part(PNG, new Uint8Array(1)));
  assert.throws(() => decodeClip(over), /more than/);
});

test('clipSize counts UTF-8 bytes, and fitsControl takes short plain text only', () => {
  assert.equal(clipSize({ text: 'é', html: 'ab', png: new Uint8Array(3) }), 7);
  assert.equal(clipSize({}), 0);
  assert.equal(fitsControl({ text: 'hi' }), true);
  assert.equal(fitsControl({ text: '' }), true);
  assert.equal(fitsControl({ text: 'x'.repeat(CONTROL_TEXT) }), true);
  assert.equal(fitsControl({ text: 'x'.repeat(CONTROL_TEXT + 1) }), false);
  assert.equal(fitsControl({ text: 'é'.repeat(CONTROL_TEXT / 2 + 1) }), false);
  assert.equal(fitsControl({ text: 'hi', html: '<b>hi</b>' }), false);
  assert.equal(fitsControl({ png: new Uint8Array(1) }), false);
  assert.deepEqual(clipTypes({ png: new Uint8Array(1), text: 'a' }), [TEXT, PNG]);
  assert.deepEqual(clipTypes({ png: new Uint8Array() }), []);
});

test('splitPaste: one PNG is the clipboard, other files are uploads', () => {
  const png = { name: 'image.png', type: PNG };
  const pdf = { name: 'report.pdf', type: 'application/pdf' };
  assert.deepEqual(splitPaste({ text: 'hi' }), { clip: { text: 'hi' }, uploads: [] });
  assert.deepEqual(splitPaste({ html: '<b>hi</b>', text: 'hi' }), { clip: { text: 'hi', html: '<b>hi</b>' }, uploads: [] });
  assert.deepEqual(splitPaste({ html: '<img>', files: [png] }), { clip: { html: '<img>', image: png }, uploads: [] });
  // Pasted files come with their names as text, which is not wanted.
  assert.deepEqual(splitPaste({ text: 'report.pdf', files: [pdf] }), { clip: null, uploads: [pdf] });
  assert.deepEqual(splitPaste({ files: [png, png] }), { clip: null, uploads: [png, png] });
  assert.deepEqual(splitPaste({ files: [png, pdf] }), { clip: null, uploads: [png, pdf] });
  assert.deepEqual(splitPaste({}), { clip: null, uploads: [] });
});
