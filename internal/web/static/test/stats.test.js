import { test } from 'node:test';
import assert from 'node:assert/strict';
import { StatsSampler } from '../stats.js';

const EMPTY = {
  transport: null,
  local: null,
  remote: null,
  framesDecoded: 0,
  frameWidth: 0,
  frameHeight: 0,
  fps: 0,
  kbps: 0,
};

// fakePC answers getStats with a report built from reports, which, like
// RTCStatsReport, iterates values and looks them up by id.
function fakePC(...reports) {
  let calls = 0;
  return {
    connectionState: 'connected',
    get calls() {
      return calls;
    },
    async getStats() {
      const stats = reports[Math.min(calls++, reports.length - 1)];
      return new Map(stats.map((s) => [s.id, s]));
    },
  };
}

const pair = [
  { id: 'T', type: 'transport', selectedCandidatePairId: 'P' },
  { id: 'P', type: 'candidate-pair', localCandidateId: 'L', remoteCandidateId: 'R' },
  { id: 'L', type: 'local-candidate', address: '10.0.0.2', port: 50000, protocol: 'udp', candidateType: 'host' },
  // Older browsers name the address ip.
  { id: 'R', type: 'remote-candidate', ip: '10.0.0.10', port: 40000, protocol: 'udp', candidateType: 'srflx' },
];

const video = (timestamp, bytesReceived, id = 'V') => ({
  id,
  type: 'inbound-rtp',
  kind: 'video',
  timestamp,
  bytesReceived,
  framesDecoded: 42,
  frameWidth: 1280,
  frameHeight: 720,
  framesPerSecond: 29.6,
});

test('no peer connection, or a closed one, reads as nothing at all', async () => {
  const s = new StatsSampler();
  assert.deepEqual(await s.sample(null), EMPTY);
  const pc = fakePC(pair);
  pc.connectionState = 'closed';
  assert.deepEqual(await s.sample(pc), EMPTY);
  assert.equal(pc.calls, 0, 'getStats on a closed connection');
});

test('the selected candidate pair names the transport and both ends', async () => {
  const out = await new StatsSampler().sample(fakePC(pair));
  assert.equal(out.transport, 'udp');
  assert.deepEqual(out.local, { address: '10.0.0.2', port: 50000, protocol: 'udp', candidateType: 'host' });
  assert.deepEqual(out.remote, { address: '10.0.0.10', port: 40000, protocol: 'udp', candidateType: 'srflx' });
  assert.equal(out.framesDecoded, 0, 'no inbound video yet');
});

test('a pair whose candidates are missing reads as no endpoint', async () => {
  const out = await new StatsSampler().sample(fakePC([pair[0], pair[1]]));
  assert.equal(out.transport, null);
  assert.equal(out.local, null);
  assert.equal(out.remote, null);
});

test('a candidate without fields reads them as null', async () => {
  const bare = [pair[0], pair[1], { id: 'L', type: 'local-candidate' }, { id: 'R', type: 'remote-candidate' }];
  const out = await new StatsSampler().sample(fakePC(bare));
  assert.deepEqual(out.local, { address: null, port: null, protocol: null, candidateType: null });
  assert.equal(out.transport, null);
});

test('inbound video gives frames, size and rounded fps; audio is ignored', async () => {
  const audio = { id: 'A', type: 'inbound-rtp', kind: 'audio', framesDecoded: 999 };
  const out = await new StatsSampler().sample(fakePC([...pair, audio, video(0, 0)]));
  assert.equal(out.framesDecoded, 42);
  assert.equal(out.frameWidth, 1280);
  assert.equal(out.frameHeight, 720);
  assert.equal(out.fps, 30);
});

test('inbound video without counters reads zeros', async () => {
  const out = await new StatsSampler().sample(fakePC([{ id: 'V', type: 'inbound-rtp', kind: 'video' }]));
  assert.equal(out.framesDecoded, 0);
  assert.equal(out.frameWidth, 0);
  assert.equal(out.frameHeight, 0);
  assert.equal(out.fps, 0);
});

test('bitrate is measured between samples at least half a second apart', async () => {
  const pc = fakePC(
    [video(1_000, 0)],
    [video(2_000, 125_000)], // 1 s, 1 Mbit
    [video(2_200, 250_000)], // 0.2 s later: too soon, the figure holds
    [video(3_000, 250_000)], // 1 s and 1 Mbit after the last measure, none after 2.2 s
  );
  const s = new StatsSampler();
  assert.equal((await s.sample(pc)).kbps, 0, 'the first sample has nothing to measure against');
  assert.equal((await s.sample(pc)).kbps, 1000);
  assert.equal((await s.sample(pc)).kbps, 1000);
  assert.equal((await s.sample(pc)).kbps, 1000, 'measured against the sample at 2 s, not 2.2 s');
});

test('a new inbound stream starts its bitrate over', async () => {
  const pc = fakePC([video(1_000, 0)], [video(2_000, 125_000)], [video(3_000, 10, 'V2')], [video(4_000, 62_510, 'V2')]);
  const s = new StatsSampler();
  await s.sample(pc);
  assert.equal((await s.sample(pc)).kbps, 1000);
  assert.equal((await s.sample(pc)).kbps, 1000, 'kept until the new stream has two samples');
  assert.equal((await s.sample(pc)).kbps, 500);
});
