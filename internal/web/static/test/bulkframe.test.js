import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash, randomBytes } from 'node:crypto';
import { CHUNK_SIZE, DEFAULT_INBOUND, Sum, checkHeader, kindInbound, kindLimit, parseFrame, registerKind } from '../bulkframe.js';

registerKind('frame-test', 1000);

test('kinds are registered once with a limit', () => {
  assert.equal(kindLimit('frame-test'), 1000);
  assert.equal(kindLimit('nobody'), undefined);
  assert.throws(() => registerKind('frame-test', 5), /registered twice/);
  for (const [kind, limit] of [['', 1], [7, 1], ['negative', -1], ['fraction', 1.5], ['huge', 2 ** 60]]) {
    assert.throws(() => registerKind(kind, limit), /bad kind/, `${kind} ${limit}`);
  }
  assert.equal(kindLimit('negative'), undefined);
});

test('a kind takes one transfer from the agent at a time unless it names more', () => {
  assert.equal(kindInbound('frame-test'), DEFAULT_INBOUND);
  assert.equal(DEFAULT_INBOUND, 1);
  assert.equal(kindInbound('nobody'), undefined);
  registerKind('frame-pair', 1, { inbound: 2 });
  registerKind('frame-clicked', 1, { inbound: Infinity });
  assert.equal(kindInbound('frame-pair'), 2);
  assert.equal(kindInbound('frame-clicked'), Infinity);
  for (const inbound of [0, -1, 1.5, '2', null, NaN]) {
    assert.throws(() => registerKind(`frame-${inbound}`, 1, { inbound }), /bad kind/, String(inbound));
  }
});

test('parseFrame takes JSON objects within a chunk', () => {
  assert.deepEqual(parseFrame('{"t":"done"}'), { t: 'done' });
  for (const junk of ['done', 'null', '7', '"text"', `{"error":"${'x'.repeat(CHUNK_SIZE)}"}`, new ArrayBuffer(4)]) {
    assert.throws(() => parseFrame(junk), /malformed bulk frame/);
  }
});

test('checkHeader wants a registered kind within its limit', () => {
  checkHeader({ t: 'header', kind: 'frame-test', size: 1000 });
  checkHeader({ t: 'header', kind: 'frame-test' }); // the agent leaves out a size of 0
  const refused = [
    [{ t: 'end', kind: 'frame-test' }, /not a header/],
    [null, /not a header/],
    [{ t: 'header', kind: 'frame-test', size: -1 }, /size -1/],
    [{ t: 'header', kind: 'frame-test', size: '10' }, /size 10/],
    [{ t: 'header', kind: 'frame-test', size: 1001 }, /too large: 1001 bytes of frame-test, the limit is 1000/],
    [{ t: 'header', kind: 'nobody', size: 1 }, /unknown bulk kind "nobody"/],
  ];
  for (const [h, why] of refused) assert.throws(() => checkHeader(h), why, JSON.stringify(h));
});

test('Sum matches SHA-256 of the chunks in order', () => {
  for (const size of [0, 1, 64, CHUNK_SIZE, 3 * CHUNK_SIZE + 5]) {
    const bytes = new Uint8Array(randomBytes(size));
    const sender = new Sum(size);
    const receiver = new Sum(size);
    for (let at = 0; at < size; at += 1000) {
      sender.add(bytes.subarray(at, at + 1000));
      receiver.add(bytes.subarray(at, at + 1000));
    }
    const end = sender.end();
    assert.deepEqual(end, { t: 'end', sha256: createHash('sha256').update(bytes).digest('hex') });
    receiver.check(end);
  }
});

test('Sum refuses what does not add up', () => {
  assert.throws(() => new Sum(10).add(new Uint8Array(CHUNK_SIZE + 1)), /chunk of 65537 bytes/);
  assert.throws(() => new Sum(10).add(new Uint8Array(11)), /past the declared size: 10 bytes declared/);
  const short = new Sum(10);
  short.add(new Uint8Array(4));
  assert.throws(() => short.end(), /ended early: 4 of 10 bytes/);
  assert.throws(() => new Sum(0).check({ t: 'done' }), /not the end/);
  assert.throws(() => new Sum(0).check({ t: 'end', sha256: '00' }), /SHA-256 mismatch/);
  const upper = new Sum(0);
  assert.throws(() => upper.check({ t: 'end', sha256: new Sum(0).end().sha256.toUpperCase() }), /SHA-256 mismatch/);
});
