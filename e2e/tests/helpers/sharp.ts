import type { Page } from '@playwright/test';
import { ssh } from './host';

// Pixels is a picture as 8-bit RGBA, rows packed.
export type Pixels = { width: number; height: number; rgba: Uint8Array };

export type Rect = { x: number; y: number; width: number; height: number };

// hostPixels is the X screen of the session as the host has it, from xwd,
// cut to rect when given: what the agent captures, before any encoder.
export async function hostPixels(rect?: Rect): Promise<Pixels> {
  const xwd = Buffer.from(await ssh(`robin-x sh -c 'xwd -root -silent | base64 -w0'`, 60_000), 'base64');
  return crop(parseXwd(xwd), rect);
}

// parseXwd reads an X window dump of a TrueColor screen, 24 or 32 bits a
// pixel.
export function parseXwd(b: Buffer): Pixels {
  const field = (i: number) => b.readUInt32BE(4 * i);
  const headerSize = field(0);
  const [format, width, height, byteOrder, bpp, bytesPerLine] = [field(2), field(4), field(5), field(7), field(11), field(12)];
  const [redMask, greenMask, blueMask, ncolors] = [field(14), field(15), field(16), field(19)];
  if (format !== 2 || (bpp !== 32 && bpp !== 24)) throw new Error(`xwd: format ${format} at ${bpp} bits a pixel`);
  const data = headerSize + 12 * ncolors;
  const shift = (m: number) => 31 - Math.clz32(m & -m);
  const [rs, gs, bs] = [shift(redMask), shift(greenMask), shift(blueMask)];
  const step = bpp / 8;
  const rgba = new Uint8Array(4 * width * height);
  for (let y = 0; y < height; y++) {
    for (let x = 0; x < width; x++) {
      const at = data + y * bytesPerLine + x * step;
      let p = 0;
      for (let k = 0; k < step; k++) p |= b[at + k] << (byteOrder === 0 ? 8 * k : 8 * (step - 1 - k));
      p >>>= 0;
      const o = 4 * (y * width + x);
      rgba[o] = (p & redMask) >>> rs;
      rgba[o + 1] = (p & greenMask) >>> gs;
      rgba[o + 2] = (p & blueMask) >>> bs;
      rgba[o + 3] = 255;
    }
  }
  return { width, height, rgba };
}

function crop(p: Pixels, r?: Rect): Pixels {
  if (!r || (r.x === 0 && r.y === 0 && r.width === p.width && r.height === p.height)) return p;
  const rgba = new Uint8Array(4 * r.width * r.height);
  for (let y = 0; y < r.height; y++) {
    rgba.set(p.rgba.subarray(4 * ((r.y + y) * p.width + r.x), 4 * ((r.y + y) * p.width + r.x + r.width)), 4 * y * r.width);
  }
  return { width: r.width, height: r.height, rgba };
}

// PagePixels is one frame on the page at the desktop's size: the video
// alone, the video with the tiles drawn over it as the page shows them,
// and which pixels a tile covers.
export type PagePixels = { width: number; height: number; video: Uint8Array; composite: Uint8Array; covered: Uint8Array; visible: boolean };

type Raw = { width: number; height: number; video: string; composite: string; covered: string; visible: boolean };

// pagePixels waits for the next frame the video shows and composes it with
// the tile canvas, at the canvas's size: the desktop's, which the video
// may be scaled down from.
export async function pagePixels(page: Page): Promise<PagePixels> {
  const raw: Raw = await page.evaluate(
    () =>
      new Promise<Raw>((resolve) => {
        const video = document.getElementById('screen') as HTMLVideoElement;
        const tiles = document.getElementById('tiles') as HTMLCanvasElement;
        video.requestVideoFrameCallback(() => {
          const w = tiles.width || video.videoWidth;
          const h = tiles.height || video.videoHeight;
          const c = document.createElement('canvas');
          c.width = w;
          c.height = h;
          const ctx = c.getContext('2d', { willReadFrequently: true })!;
          ctx.drawImage(video, 0, 0, w, h);
          const v = ctx.getImageData(0, 0, w, h).data;
          const visible = tiles.width > 0 && tiles.style.visibility !== 'hidden';
          const t = visible ? tiles.getContext('2d')!.getImageData(0, 0, w, h).data : null;
          const composite = new Uint8Array(v);
          const covered = new Uint8Array(w * h);
          if (t) {
            for (let p = 0; p < w * h; p++) {
              if (t[4 * p + 3] !== 255) continue;
              covered[p] = 1;
              composite.set(t.subarray(4 * p, 4 * p + 4), 4 * p);
            }
          }
          const b64 = (u: Uint8Array) => {
            let s = '';
            for (let i = 0; i < u.length; i += 0x8000) s += String.fromCharCode(...u.subarray(i, i + 0x8000));
            return btoa(s);
          };
          resolve({ width: w, height: h, video: b64(new Uint8Array(v)), composite: b64(composite), covered: b64(covered), visible });
        });
      }),
  );
  const bytes = (s: string) => new Uint8Array(Buffer.from(s, 'base64'));
  return { ...raw, video: bytes(raw.video), composite: bytes(raw.composite), covered: bytes(raw.covered) };
}

// Match counts, over the pixels a tile covers, those where the composite
// and the video alone differ from the host in any channel.
export type Match = { covered: number; total: number; composite: number; video: number };

export function match(host: Pixels, page: PagePixels): Match {
  if (host.width !== page.width || host.height !== page.height) {
    throw new Error(`host ${host.width}x${host.height}, page ${page.width}x${page.height}`);
  }
  const m: Match = { covered: 0, total: host.width * host.height, composite: 0, video: 0 };
  const differs = (a: Uint8Array, i: number) => a[i] !== host.rgba[i] || a[i + 1] !== host.rgba[i + 1] || a[i + 2] !== host.rgba[i + 2];
  for (let p = 0; p < m.total; p++) {
    if (!page.covered[p]) continue;
    m.covered++;
    if (differs(page.composite, 4 * p)) m.composite++;
    if (differs(page.video, 4 * p)) m.video++;
  }
  return m;
}

export type Trace = {
  frames: { at: number; rtp: number; gen: number; visible: boolean; drawn: [number, number][] }[];
  manifests: { at: number; rtp: number; gen: number; gone: number[] }[];
};

const after = (a: number, b: number) => ((a - b) | 0) > 0;

// changes maps each tile of each grid to the RTP timestamps of the frames
// that changed it, by the manifests' own account.
function changes(t: Trace): Map<string, { rtp: number; at: number }[]> {
  const out = new Map<string, { rtp: number; at: number }[]>();
  for (const m of t.manifests) {
    for (let k = 0; k + 1 < m.gone.length; k += 2) {
      for (let i = m.gone[k]; i < m.gone[k + 1]; i++) {
        const key = `${m.gen}:${i}`;
        out.set(key, [...(out.get(key) ?? []), { rtp: m.rtp, at: m.at }]);
      }
    }
  }
  return out;
}

// staleFrames lists the frames shown with a stale tile on screen: tiles
// could be seen, and a tile drawn from the batch of frame b was changed by
// a frame m with b < m <= the frame shown. Manifests that arrived after the
// frame count too; that is the point.
export function staleFrames(t: Trace): { at: number; rtp: number; tile: number }[] {
  const changed = changes(t);
  const out: { at: number; rtp: number; tile: number }[] = [];
  for (const f of t.frames) {
    if (!f.visible) continue;
    for (const [i, b] of f.drawn) {
      if ((changed.get(`${f.gen}:${i}`) ?? []).some((m) => after(m.rtp, b) && !after(m.rtp, f.rtp))) {
        out.push({ at: f.at, rtp: f.rtp, tile: i });
      }
    }
  }
  return out;
}

// sharpAgain is, for each tile a change named during the trace, how long
// after the manifest of its last change it was drawn sharp again, in ms;
// Infinity for one that never was.
export function sharpAgain(t: Trace): number[] {
  const out: number[] = [];
  for (const [key, list] of changes(t)) {
    const [gen, tile] = key.split(':').map(Number);
    const last = list[list.length - 1];
    const back = t.frames.find((f) => f.gen === gen && f.visible && f.drawn.some(([i, b]) => i === tile && !after(last.rtp, b)));
    out.push(back ? Math.round(back.at - last.at) : Infinity);
  }
  return out;
}

// hiddenTime is how long, in ms, the tiles were hidden during the trace
// while the canvas held some: frames shown before their manifest. late is,
// for each such frame, how long after it its manifest arrived.
export function hiddenTime(t: Trace): { frames: number; ms: number; late: number[] } {
  let frames = 0;
  let ms = 0;
  const late: number[] = [];
  t.frames.forEach((f, k) => {
    if (f.visible || f.drawn.length === 0) return;
    frames++;
    const next = t.frames[k + 1];
    if (next) ms += next.at - f.at;
    const m = t.manifests.find((m) => m.gen === f.gen && !after(f.rtp, m.rtp));
    late.push(m ? Math.round(m.at - f.at) : Infinity);
  });
  return { frames, ms: Math.round(ms), late };
}

// holdControl, run before the page loads, lets a test hold every message
// on the page's control channel for window.__robinHoldControlMs, in order,
// as a lost packet holds an ordered SCTP stream up behind it while the
// video goes on. window.__robinManifests counts the tile manifests that
// arrive.
export function holdControl() {
  const create = RTCPeerConnection.prototype.createDataChannel;
  RTCPeerConnection.prototype.createDataChannel = function (this: RTCPeerConnection, label: string, init?: RTCDataChannelInit) {
    const ch = create.call(this, label, init);
    if (label !== 'control') return ch;
    let handler: ((e: MessageEvent) => void) | null = null;
    let last = 0;
    Object.defineProperty(ch, 'onmessage', {
      get: () => handler,
      set: (h) => { handler = h; },
    });
    ch.addEventListener('message', (e) => {
      const w = window as any;
      if (typeof e.data === 'string' && e.data.startsWith('{"t":"frame"')) w.__robinManifests = (w.__robinManifests ?? 0) + 1;
      const hold = w.__robinHoldControlMs ?? 0;
      const now = performance.now();
      const at = Math.max(now + hold, last);
      last = at;
      if (at <= now) handler?.call(ch, e);
      else setTimeout(() => handler?.call(ch, e), at - now);
    });
    return ch;
  } as typeof create;
}
