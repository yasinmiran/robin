import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash, randomBytes } from 'node:crypto';
import { BulkRefused, Inbound, acceptBulk, onBulk, sendBulk } from '../bulk.js';
import { CHUNK_SIZE, registerKind } from '../bulkframe.js';
import { FakeChannel, FakePeer } from './fakes.js';

registerKind('test', 16 * 1024 * 1024);

const later = () => new Promise((resolve) => setTimeout(resolve, 0));
const sha = (bytes) => createHash('sha256').update(bytes).digest('hex');

// memSink keeps a transfer and says how it ended.
function memSink() {
  const parts = [];
  let settle;
  const sink = {
    ended: new Promise((resolve) => { settle = resolve; }),
    write: async (chunk) => { parts.push(chunk.slice()); },
    commit: async () => settle({ ok: true }),
    abort: async (err) => settle({ ok: false, err }),
    bytes: () => Buffer.concat(parts),
  };
  return sink;
}

// The far end answers with the page's own receiver.
const sinks = [];
onBulk('test', (h) => {
  const sink = memSink();
  sink.header = h;
  sinks.push(sink);
  if (h.id === 'refuse') throw new Error('not today');
  return sink;
});

test('a transfer arrives whole on the far side, flow control and all', async () => {
  let farChannel;
  const pc = new FakePeer((ch) => {
    farChannel = ch;
    ch.peer.paused = true; // the far side reads nothing at first
    acceptBulk(ch);
  });
  const bytes = new Uint8Array(randomBytes(3 * 1024 * 1024 + 3));
  let peak = 0;
  const sending = sendBulk(pc, { kind: 'test', id: 'up', name: 'n.bin', mime: 'application/octet-stream' }, new Blob([bytes]));
  // Let the sender fill its queue, then let it drain.
  for (let i = 0; i < 200; i++) {
    await later();
    peak = Math.max(peak, pc.channels[0]?.bufferedAmount ?? 0);
  }
  // One chunk past the high-water mark, and the header.
  assert.ok(peak > 1024 * 1024 && peak <= 1024 * 1024 + CHUNK_SIZE + 1024, `queue peaked at ${peak} bytes`);
  farChannel.peer.resume();
  await sending;
  const sink = sinks.at(-1);
  assert.deepEqual(await sink.ended, { ok: true });
  assert.deepEqual(sink.header, { kind: 'test', id: 'up', name: 'n.bin', mime: 'application/octet-stream', t: 'header', size: bytes.length });
  assert.equal(sha(sink.bytes()), sha(bytes));
  assert.equal(farChannel.binaryType, 'arraybuffer');
  assert.notEqual(pc.channels[0].readyState, 'open');
});

test('progress hears the bytes sent after each chunk', async () => {
  const pc = new FakePeer(acceptBulk);
  const heard = [];
  await sendBulk(pc, { kind: 'test' }, new Blob([new Uint8Array(2 * CHUNK_SIZE + 5)]), { progress: (n) => heard.push(n) });
  assert.deepEqual(heard, [CHUNK_SIZE, 2 * CHUNK_SIZE, 2 * CHUNK_SIZE + 5]);
});

test('an empty transfer is a header and an end', async () => {
  const pc = new FakePeer(acceptBulk);
  await sendBulk(pc, { kind: 'test' }, new Blob([]));
  assert.deepEqual(await sinks.at(-1).ended, { ok: true });
  assert.equal(sinks.at(-1).bytes().length, 0);
});

test('sendBulk refuses what the agent could not take before opening a channel', async () => {
  const pc = new FakePeer(acceptBulk);
  await assert.rejects(sendBulk(pc, { kind: 'nobody' }, new Blob(['x'])), /unknown bulk kind/);
  await assert.rejects(sendBulk(pc, { kind: 'test' }, { size: 17 * 1024 * 1024 }), /too large/);
  const gone = new AbortController();
  gone.abort();
  await assert.rejects(sendBulk(pc, { kind: 'test' }, new Blob(['x']), { signal: gone.signal }), { name: 'AbortError' });
  assert.equal(pc.channels.length, 0);
});

test('the far side refusing rejects with its reason', async () => {
  const pc = new FakePeer(acceptBulk);
  await assert.rejects(sendBulk(pc, { kind: 'test', id: 'refuse' }, new Blob(['abc'])), (err) => {
    assert.ok(err instanceof BulkRefused);
    assert.equal(err.reason, 'not today');
    assert.match(err.message, /refused: not today/);
    return true;
  });
});

// answering opens transfers to a far end that answers each frame with reply.
function answering(reply) {
  return new FakePeer((ch) => ch.addEventListener('message', (e) => reply(ch, e.data)));
}

test('sendBulk reads the verdict', async () => {
  const cases = [
    ['done before the end', (ch) => ch.send('{"t":"done"}'), /receiver finished before the end/],
    ['junk', (ch) => ch.send('junk'), /refused: malformed verdict/],
    ['binary', (ch) => ch.send(new Uint8Array(1)), /refused: malformed verdict/],
    ['an error', (ch) => ch.send('{"t":"error","error":"disk full"}'), /refused: disk full/],
    ['a close', (ch) => ch.close(), /cancelled/],
  ];
  for (const [name, reply, want] of cases) {
    let replied = false;
    const pc = answering((ch) => {
      if (!replied) reply(ch);
      replied = true;
    });
    // Big enough that the reply lands before the last chunk goes.
    await assert.rejects(sendBulk(pc, { kind: 'test' }, new Blob([new Uint8Array(40 * CHUNK_SIZE)])), want, name);
  }
  // A verdict after the end decides the outcome.
  const ok = answering((ch, data) => { if (typeof data === 'string' && data.includes('"end"')) ch.send('{"t":"done"}'); });
  await sendBulk(ok, { kind: 'test' }, new Blob(['abc']));
});

test('aborting cancels a transfer under way', async () => {
  const pc = new FakePeer((ch) => {
    ch.peer.paused = true;
    acceptBulk(ch);
  });
  const stop = new AbortController();
  const before = sinks.length;
  const sending = sendBulk(pc, { kind: 'test', id: 'abort' }, new Blob([new Uint8Array(4 * 1024 * 1024)]), { signal: stop.signal });
  for (let i = 0; i < 50; i++) await later();
  stop.abort();
  await assert.rejects(sending, { name: 'AbortError' });
  pc.channels[0].resume();
  while (sinks.length === before) await later();
  const result = await sinks.at(-1).ended;
  assert.equal(result.ok, false);
  assert.match(result.err.message, /cancelled/);
});

test('onBulk takes each kind once', () => {
  assert.throws(() => onBulk('test', () => null), /taken twice/);
});

// inbound feeds frames to a receiver and reports its verdict.
function inbound(lookup) {
  const channel = new FakeChannel('bulk');
  channel.readyState = 'open';
  const verdicts = [];
  let closed = false;
  channel.send = (data) => verdicts.push(JSON.parse(data));
  channel.close = () => { closed = true; };
  const rx = new Inbound(channel, lookup);
  return { rx, verdicts, closed: () => closed };
}

const header = (size, extra = {}) => JSON.stringify({ t: 'header', kind: 'test', size, ...extra });

test('Inbound refuses what does not add up', async () => {
  const hundred = new Uint8Array(randomBytes(100));
  const end = (bytes) => JSON.stringify({ t: 'end', sha256: sha(bytes) });
  const cases = [
    ['data before the header', [hundred.buffer], /data before the header/, false],
    ['junk header', ['hello'], /malformed/, false],
    ['nothing takes the kind', [JSON.stringify({ t: 'header', kind: 'test', size: 1 })], /this page takes none/, false, () => undefined],
    ['refused', [header(1, { id: 'no' })], /not here/, false],
    ['too large', [header(16 * 1024 * 1024 + 1)], /too large/, false],
    ['wrong hash', [header(100), hundred.buffer, end(hundred.subarray(1))], /SHA-256 mismatch/, true],
    ['truncated', [header(100), hundred.subarray(0, 50).slice().buffer, end(hundred.subarray(0, 50))], /ended early/, true],
    ['past the size', [header(10), hundred.buffer], /past the declared size/, true],
    ['oversized chunk', [header(16 * 1024 * 1024), new ArrayBuffer(CHUNK_SIZE + 1)], /chunk of/, true],
    ['junk end', [header(0), '{'], /malformed/, true],
  ];
  for (const [name, frames, why, aborts, lookup] of cases) {
    const sink = memSink();
    const { rx, verdicts, closed } = inbound(lookup ?? (() => (h) => {
      if (h.id === 'no') throw new Error('not here');
      return sink;
    }));
    for (const f of frames) await rx.message(f);
    assert.equal(verdicts.length, 1, name);
    assert.equal(verdicts[0].t, 'error', name);
    assert.match(verdicts[0].error, why, name);
    assert.ok(closed(), `${name}: channel left open`);
    if (aborts) assert.match((await sink.ended).err.message, why, name);
    await rx.message('{"t":"end"}'); // nothing after the verdict counts
    assert.equal(verdicts.length, 1, name);
  }
});

test('Inbound keeps a good transfer and reports a sink that fails', async () => {
  const bytes = new Uint8Array(randomBytes(1000));
  const end = JSON.stringify({ t: 'end', sha256: sha(bytes) });

  const good = memSink();
  let r = inbound(() => () => good);
  for (const f of [header(1000), bytes.buffer, end]) await r.rx.message(f);
  assert.deepEqual(r.verdicts, [{ t: 'done' }]);
  assert.equal(sha(good.bytes()), sha(bytes));
  await r.rx.closed(); // the close after the verdict changes nothing
  assert.deepEqual(await good.ended, { ok: true });

  const full = { ...memSink(), write: async () => { throw new Error('disk full'); } };
  r = inbound(() => () => full);
  for (const f of [header(1000), bytes.buffer]) await r.rx.message(f);
  assert.match(r.verdicts[0].error, /disk full/);
  assert.match((await full.ended).err.message, /disk full/);

  let aborted = false;
  const stuck = { ...memSink(), commit: async () => { throw new Error('rename failed'); }, abort: () => { aborted = true; } };
  r = inbound(() => () => stuck);
  for (const f of [header(1000), bytes.buffer, end]) await r.rx.message(f);
  assert.deepEqual(r.verdicts, [{ t: 'error', error: 'rename failed' }]);
  assert.equal(aborted, false, 'a failed commit is not aborted too');

  const warned = [];
  const warn = console.warn;
  console.warn = (...args) => warned.push(args);
  try {
    const clumsy = { ...memSink(), abort: () => { throw new Error('abort broke'); } };
    r = inbound(() => () => clumsy);
    for (const f of [header(10), bytes.buffer]) await r.rx.message(f);
    assert.equal(r.verdicts[0].t, 'error');
    assert.equal(warned.length, 1);
  } finally {
    console.warn = warn;
  }

  r = inbound(() => () => { throw new Error('x'.repeat(5000)); });
  await r.rx.message(header(1));
  assert.equal(r.verdicts[0].error.length, 1024);
});

test('Inbound aborts on a close before the end', async () => {
  const sink = memSink();
  const r = inbound(() => () => sink);
  await r.rx.message(header(100));
  await r.rx.closed();
  assert.match((await sink.ended).err.message, /cancelled/);
  await r.rx.closed();
  const idle = inbound(() => () => sink);
  await idle.rx.closed(); // nothing opened, nothing to abort
});
