import type { Page } from '@playwright/test';

export type Region = { x: number; y: number; width: number; height: number };

export type FrameSample = {
  width: number;
  height: number;
  mean: number; // luma, 0..255
  std: number;
  changed?: number; // pixels whose luma moved more than LUMA_DELTA
  compared?: number;
};

// LUMA_DELTA is far above H.264 ringing on static text and far below the
// black-on-white swing of a glyph appearing.
export const LUMA_DELTA = 48;

// CHANGED_FLOOR is the fewest changed pixels that count as new content: a few
// glyphs of xterm's default 6x13 font.
export const CHANGED_FLOOR = 150;

export const isNonBlack = (s: FrameSample) => s.width > 0 && (s.mean > 24 || s.std > 12);

type SampleArgs = { selector: string; save?: string; compare?: string; region?: Region; delta: number };

// sampleFrame draws the current frame of the video at selector into a canvas
// at its native resolution. save keeps the luma plane under a name; compare
// counts pixels that differ from a saved plane, within region when given
// (region is in video pixels, which are remote display pixels).
export function sampleFrame(
  page: Page,
  selector: string,
  opts: { save?: string; compare?: string; region?: Region } = {},
): Promise<FrameSample> {
  return page.evaluate(sampleInPage, { selector, ...opts, delta: LUMA_DELTA });
}

// Runs in the page: it must stay self-contained.
function sampleInPage({ selector, save, compare, region, delta }: SampleArgs): FrameSample {
  const video = document.querySelector(selector) as HTMLVideoElement | null;
  if (!video) throw new Error(`no element ${selector}`);
  const w = video.videoWidth;
  const h = video.videoHeight;
  if (!w || !h) return { width: 0, height: 0, mean: 0, std: 0 };

  const g = window as any;
  const canvas: HTMLCanvasElement = (g.__rjCanvas ??= document.createElement('canvas'));
  if (canvas.width !== w) canvas.width = w;
  if (canvas.height !== h) canvas.height = h;
  const ctx = canvas.getContext('2d', { willReadFrequently: true })!;
  ctx.drawImage(video, 0, 0, w, h);
  const rgba = ctx.getImageData(0, 0, w, h).data;

  const luma = new Uint8Array(w * h);
  let sum = 0;
  let sumSq = 0;
  for (let p = 0, i = 0; p < luma.length; p++, i += 4) {
    const y = (rgba[i] * 77 + rgba[i + 1] * 150 + rgba[i + 2] * 29) >> 8;
    luma[p] = y;
    sum += y;
    sumSq += y * y;
  }
  const n = luma.length;
  const mean = sum / n;
  const result: FrameSample = { width: w, height: h, mean, std: Math.sqrt(Math.max(0, sumSq / n - mean * mean)) };

  g.__rjFrames ??= {};
  if (compare) {
    const ref = g.__rjFrames[compare];
    if (!ref) throw new Error(`no saved frame ${compare}`);
    if (ref.w !== w || ref.h !== h) {
      result.changed = result.compared = n;
    } else {
      const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, Math.round(v)));
      const x0 = clamp(region?.x ?? 0, 0, w);
      const y0 = clamp(region?.y ?? 0, 0, h);
      const x1 = clamp(region ? region.x + region.width : w, x0, w);
      const y1 = clamp(region ? region.y + region.height : h, y0, h);
      let changed = 0;
      for (let y = y0; y < y1; y++) {
        for (let x = x0, p = y * w + x0; x < x1; x++, p++) {
          if (Math.abs(luma[p] - ref.luma[p]) > delta) changed++;
        }
      }
      result.changed = changed;
      result.compared = (x1 - x0) * (y1 - y0);
    }
  }
  if (save) g.__rjFrames[save] = { w, h, luma };
  return result;
}

// waitForStillFrame waits until two samples 400 ms apart barely differ, so a
// later diff measures the test's action rather than a picture still settling.
// Returns false if the picture never settles within timeoutMs.
export async function waitForStillFrame(page: Page, selector: string, timeoutMs = 10_000): Promise<boolean> {
  const deadline = Date.now() + timeoutMs;
  await sampleFrame(page, selector, { save: '__still' });
  while (Date.now() < deadline) {
    await page.waitForTimeout(400);
    const s = await sampleFrame(page, selector, { compare: '__still', save: '__still' });
    if ((s.changed ?? Infinity) < 20) return true;
  }
  return false;
}
