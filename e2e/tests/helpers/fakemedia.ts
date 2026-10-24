import fs from 'node:fs';

// Chrome's fake microphone and camera, playing files of what the tests then
// look for on the host: a tone of TONE_HZ and a picture of COLOUR with a bar
// that moves, so every frame differs. The defaults Chrome plays without
// files are beeps and a pattern too busy to check a colour in.

export const TONE_HZ = 440;
// In BT.601 limited range, a strong red no encoder mistakes for black.
export const COLOUR = { y: 81, u: 90, v: 240 };
export const CAMERA_SIZE = { width: 640, height: 360 };

const AUDIO = '/tmp/robin-e2e-tone.wav';
const VIDEO = '/tmp/robin-e2e-camera.y4m';

// writeTone writes two seconds of the tone swelling and fading twice a
// second. A steady tone would not last: the browser's noise suppression
// takes anything that never changes for noise.
function writeTone() {
  const rate = 48000;
  const samples = rate * 2;
  const data = Buffer.alloc(samples * 2);
  for (let i = 0; i < samples; i++) {
    const swell = 0.5 + 0.5 * Math.sin((2 * Math.PI * 2 * i) / rate);
    data.writeInt16LE(Math.round(12000 * swell * Math.sin((2 * Math.PI * TONE_HZ * i) / rate)), 2 * i);
  }
  const header = Buffer.alloc(44);
  header.write('RIFF', 0);
  header.writeUInt32LE(36 + data.length, 4);
  header.write('WAVEfmt ', 8);
  header.writeUInt32LE(16, 16);
  header.writeUInt16LE(1, 20); // PCM
  header.writeUInt16LE(1, 22); // mono
  header.writeUInt32LE(rate, 24);
  header.writeUInt32LE(rate * 2, 28);
  header.writeUInt16LE(2, 32);
  header.writeUInt16LE(16, 34);
  header.write('data', 36);
  header.writeUInt32LE(data.length, 40);
  writeWhole(AUDIO, Buffer.concat([header, data]));
}

function writeCamera() {
  const { width: w, height: h } = CAMERA_SIZE;
  const frames = 30;
  const parts = [Buffer.from(`YUV4MPEG2 W${w} H${h} F30:1 Ip A1:1 C420jpeg\n`)];
  for (let f = 0; f < frames; f++) {
    const y = Buffer.alloc(w * h, COLOUR.y);
    // A white bar across the top fifth, moving right a little each frame.
    const x0 = Math.floor((f * w) / frames);
    for (let row = 0; row < h / 5; row++) y.fill(235, row * w + x0, row * w + Math.min(w, x0 + 40));
    parts.push(Buffer.from('FRAME\n'), y, Buffer.alloc((w * h) / 4, COLOUR.u), Buffer.alloc((w * h) / 4, COLOUR.v));
  }
  writeWhole(VIDEO, Buffer.concat(parts));
}

// writeWhole puts data at path by rename, so a Chrome opening the file
// never reads it half written, whoever else is writing it.
function writeWhole(path: string, data: Buffer) {
  const tmp = `${path}.${process.pid}`;
  fs.writeFileSync(tmp, data);
  fs.renameSync(tmp, path);
}

// fakeMediaArgs writes the files afresh, so they always hold what the
// constants above say, and returns the flags that make Chrome play them as
// its microphone and camera and grant both without asking. Both take
// milliseconds to make.
export function fakeMediaArgs(): string[] {
  writeTone();
  writeCamera();
  return [
    '--use-fake-ui-for-media-stream',
    '--use-fake-device-for-media-stream',
    `--use-file-for-fake-audio-capture=${AUDIO}`,
    `--use-file-for-fake-video-capture=${VIDEO}`,
  ];
}

// pcm decodes base64 of 16-bit little-endian samples.
export function pcm(b64: string): Int16Array {
  const b = Buffer.from(b64.trim(), 'base64');
  return new Int16Array(b.buffer, b.byteOffset, Math.floor(b.length / 2));
}

// toneOf is the pitch of 48 kHz mono, counted in swings past a small
// threshold over the 20 ms blocks that are loud, and the share of blocks
// that are.
export function toneOf(samples: Int16Array): { hz: number; loud: number } {
  const swing = 300;
  const peakFloor = 2000;
  const block = 960;
  let swings = 0;
  let loud = 0;
  let blocks = 0;
  for (let b = 0; b + block <= samples.length; b += block) {
    blocks++;
    let peak = 0;
    let high = samples[b] > 0;
    let n = 0;
    for (let i = b; i < b + block; i++) {
      const v = samples[i];
      peak = Math.max(peak, Math.abs(v));
      if ((high && v < -swing) || (!high && v > swing)) {
        high = !high;
        n++;
      }
    }
    if (peak > peakFloor) {
      loud++;
      swings += n;
    }
  }
  return { hz: loud ? swings / 2 / (loud * 0.02) : 0, loud: blocks ? loud / blocks : 0 };
}

// pixel is the Y, U and V at x, y of a base64 I420 frame of width x height.
export function pixel(b64: string, width: number, height: number, x: number, y: number) {
  const f = Buffer.from(b64.trim(), 'base64');
  if (f.length !== (width * height * 3) / 2) throw new Error(`frame of ${f.length} bytes, want ${width}x${height} I420`);
  const c = Math.floor(y / 2) * (width / 2) + Math.floor(x / 2);
  return { y: f[y * width + x], u: f[width * height + c], v: f[(width * height * 5) / 4 + c] };
}

export const near = (a: number, b: number, tolerance = 12) => Math.abs(a - b) <= tolerance;
