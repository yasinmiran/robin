import test from 'node:test';
import assert from 'node:assert/strict';
import { CAMERA_CONSTRAINTS, DeviceSwitch, MIC_CONSTRAINTS, constrainedBaselineFirst, reason } from '../devices.js';

// FakeTrack is a MediaStreamTrack: it can be stopped, and can end on its own.
class FakeTrack extends EventTarget {
  stopped = false;
  stop() {
    this.stopped = true;
  }
  end() {
    this.dispatchEvent(new Event('ended'));
  }
}

// rig is a switch whose browser answers when the test says so.
function rig({ sendFails = false } = {}) {
  const r = { sent: [], states: [], pending: [] };
  r.sw = new DeviceSwitch({
    acquire: () => new Promise((resolve, reject) => r.pending.push({ resolve, reject })),
    send: async (track) => {
      if (sendFails && track) throw new Error('sender closed');
      r.sent.push(track);
    },
    changed: (state, why) => r.states.push(why ? `${state}: ${why}` : state),
  });
  r.grant = async () => {
    const track = new FakeTrack();
    r.pending.shift().resolve(track);
    await settle();
    return track;
  };
  r.deny = async (name) => {
    r.pending.shift().reject(Object.assign(new Error('denied'), { name }));
    await settle();
  };
  return r;
}

const settle = () => new Promise((resolve) => setTimeout(resolve, 0));

test('on puts the track on the sender; off takes it away and stops it', async () => {
  const r = rig();
  assert.equal(r.sw.state, 'off');
  const on = r.sw.toggle();
  assert.equal(r.sw.state, 'starting');
  const track = await r.grant();
  await on;
  assert.equal(r.sw.state, 'on');
  assert.deepEqual(r.sent, [track]);
  await r.sw.toggle();
  assert.equal(r.sw.state, 'off');
  assert.ok(track.stopped);
  assert.deepEqual(r.sent, [track, null]);
  assert.deepEqual(r.states, ['starting', 'on', 'off']);
});

test('a refusal blocks the switch with the reason, and turning it on asks again', async () => {
  const r = rig();
  const on = r.sw.on();
  await r.deny('NotAllowedError');
  await on;
  assert.equal(r.sw.state, 'blocked');
  assert.equal(r.sw.why, 'The browser was not allowed to use it');
  assert.deepEqual(r.sent, []);
  const again = r.sw.toggle();
  assert.equal(r.sw.state, 'starting');
  assert.equal(r.sw.why, '');
  await r.grant();
  await again;
  assert.equal(r.sw.state, 'on');
  assert.deepEqual(r.states, ['starting', 'blocked: The browser was not allowed to use it', 'starting', 'on']);
});

test('turned off while the browser asks, the track it then gives is stopped, never sent', async () => {
  const r = rig();
  const on = r.sw.on();
  await r.sw.toggle();
  assert.equal(r.sw.state, 'off');
  const track = await r.grant();
  await on;
  assert.equal(r.sw.state, 'off');
  assert.ok(track.stopped);
  assert.deepEqual(r.sent, []);
});

test('turned off while the track goes on the sender, it comes off again', async () => {
  const r = rig();
  let release;
  const sending = new Promise((resolve) => (release = resolve));
  const sw = new DeviceSwitch({
    acquire: async () => (r.track = new FakeTrack()),
    send: async (track) => {
      r.sent.push(track);
      if (track) await sending;
    },
  });
  const on = sw.on();
  await settle();
  await sw.off();
  release();
  await on;
  assert.equal(sw.state, 'off');
  assert.ok(r.track.stopped);
  assert.deepEqual(r.sent, [r.track, null]);
});

test('a refusal after turning off does not block', async () => {
  const r = rig();
  const on = r.sw.on();
  await r.sw.off();
  await r.deny('NotAllowedError');
  await on;
  assert.equal(r.sw.state, 'off');
});

test('a sender that refuses the track blocks the switch and stops the track', async () => {
  const r = rig({ sendFails: true });
  const on = r.sw.on();
  const track = await r.grant();
  await on;
  assert.equal(r.sw.state, 'blocked');
  assert.equal(r.sw.why, 'sender closed');
  assert.ok(track.stopped);
});

test('a track that ends on its own turns the switch off', async () => {
  const r = rig();
  const on = r.sw.on();
  const track = await r.grant();
  await on;
  track.end();
  await settle();
  assert.equal(r.sw.state, 'off');
  assert.deepEqual(r.sent, [track, null]);
});

test('on twice asks once; off twice sends once', async () => {
  const r = rig();
  const on = r.sw.on();
  await r.sw.on();
  assert.equal(r.pending.length, 1);
  await r.grant();
  await on;
  await r.sw.on();
  await r.sw.off();
  await r.sw.off();
  assert.deepEqual(r.sent.length, 2);
});

test('reasons a person can act on', () => {
  assert.equal(reason({ name: 'NotFoundError' }), 'This computer has none');
  assert.equal(reason({ name: 'NotReadableError' }), 'Another program is using it');
  assert.equal(reason(new Error('odd')), 'odd');
  assert.equal(reason('plain'), 'plain');
});

test('the page asks for one microphone channel and the camera at the size the host shows', () => {
  assert.equal(MIC_CONSTRAINTS.audio.channelCount, 1);
  assert.equal(MIC_CONSTRAINTS.audio.echoCancellation, true);
  assert.deepEqual(CAMERA_CONSTRAINTS.video.width, { ideal: 1280 });
  assert.deepEqual(CAMERA_CONSTRAINTS.video.height, { ideal: 720 });
});

test('Constrained Baseline in mode 1 goes first, the rest keep their order', () => {
  const vp8 = { mimeType: 'video/VP8' };
  const rtx = { mimeType: 'video/rtx', sdpFmtpLine: 'apt=96' };
  const high = { mimeType: 'video/H264', sdpFmtpLine: 'packetization-mode=1;profile-level-id=640c1f' };
  const mode0 = { mimeType: 'video/H264', sdpFmtpLine: 'packetization-mode=0;profile-level-id=42e01f' };
  const cb = { mimeType: 'video/H264', sdpFmtpLine: 'level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f' };
  assert.deepEqual(constrainedBaselineFirst([vp8, rtx, high, mode0, cb]), [cb, vp8, rtx, high, mode0]);
  assert.equal(constrainedBaselineFirst([vp8, rtx, { mimeType: 'video/H264' }]), null);
});

test('switched off and on again while the browser asks, only the second track is sent', async () => {
  const r = rig();
  const first = r.sw.on();
  await r.sw.off();
  const second = r.sw.on();
  assert.equal(r.pending.length, 2);
  const stale = await r.grant();
  await first;
  assert.ok(stale.stopped, 'the first answer is stopped');
  assert.equal(r.sw.state, 'starting');
  const track = await r.grant();
  await second;
  assert.equal(r.sw.state, 'on');
  assert.ok(!track.stopped);
  assert.deepEqual(r.sent, [track]);
});

test('switched off and on again while the first track goes on the sender, the second stays on', async () => {
  const sent = [];
  const tracks = [];
  let release;
  const held = new Promise((resolve) => (release = resolve));
  const sw = new DeviceSwitch({
    acquire: async () => {
      const t = new FakeTrack();
      tracks.push(t);
      return t;
    },
    send: async (track) => {
      sent.push(track);
      if (track === tracks[0]) await held;
    },
  });
  const first = sw.on();
  await settle();
  await sw.off();
  const second = sw.on();
  await second;
  release();
  await first;
  assert.equal(sw.state, 'on');
  assert.ok(tracks[0].stopped, 'the first track is stopped');
  assert.ok(!tracks[1].stopped, 'the second track is live');
  assert.equal(sent.at(-1), tracks[1], 'the sender ends with the second track');
});
