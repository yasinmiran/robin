import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash, randomBytes } from 'node:crypto';
import { Sha256 } from '../sha256.js';

const oracle = (bytes) => createHash('sha256').update(bytes).digest('hex');
const text = (s) => new TextEncoder().encode(s);

test('FIPS 180-4 examples', () => {
  assert.equal(new Sha256().hex(), 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855');
  assert.equal(new Sha256().update(text('abc')).hex(), 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad');
  assert.equal(
    new Sha256().update(text('abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq')).hex(),
    '248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1',
  );
});

test('every length across the padding boundaries', () => {
  const bytes = new Uint8Array(randomBytes(200));
  for (let n = 0; n <= 200; n++) {
    assert.equal(new Sha256().update(bytes.subarray(0, n)).hex(), oracle(bytes.subarray(0, n)), `length ${n}`);
  }
});

test('pieces of any size hash as the whole', () => {
  const bytes = new Uint8Array(randomBytes(3 * 1024 * 1024 + 77));
  const want = oracle(bytes);
  assert.equal(new Sha256().update(bytes).hex(), want);
  for (let round = 0; round < 5; round++) {
    const h = new Sha256();
    for (let at = 0; at < bytes.length;) {
      const n = Math.min(bytes.length - at, 1 + Math.floor(Math.random() * (round < 2 ? 100 : 70000)));
      h.update(bytes.subarray(at, at + n));
      at += n;
    }
    assert.equal(h.hex(), want, `round ${round}`);
  }
});

test('digest is 32 bytes and update chains', () => {
  const sum = new Sha256().update(text('a')).update(text('bc')).digest();
  assert.equal(sum.length, 32);
  assert.equal(Buffer.from(sum).toString('hex'), oracle(text('abc')));
});
