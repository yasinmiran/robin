import type { Page } from '@playwright/test';

export type VideoBox = {
  left: number; // content box, in CSS pixels of the page
  top: number;
  width: number;
  height: number;
  fit: string; // computed object-fit
  videoWidth: number;
  videoHeight: number;
};

// clientPoint maps a remote display pixel to the page point at its centre,
// honouring object-fit with the default centred object-position.
export function clientPoint(box: VideoBox, x: number, y: number): { x: number; y: number } {
  const fx = box.width / box.videoWidth;
  const fy = box.height / box.videoHeight;
  let sx: number;
  let sy: number;
  switch (box.fit) {
    case 'fill':
      [sx, sy] = [fx, fy];
      break;
    case 'cover':
      sx = sy = Math.max(fx, fy);
      break;
    case 'none':
      sx = sy = 1;
      break;
    case 'scale-down':
      sx = sy = Math.min(1, fx, fy);
      break;
    default:
      sx = sy = Math.min(fx, fy);
  }
  const offsetX = (box.width - box.videoWidth * sx) / 2;
  const offsetY = (box.height - box.videoHeight * sy) / 2;
  return { x: box.left + offsetX + (x + 0.5) * sx, y: box.top + offsetY + (y + 0.5) * sy };
}

export function videoBox(page: Page, selector: string): Promise<VideoBox> {
  return page.$eval(selector, (el) => {
    const v = el as HTMLVideoElement;
    const r = v.getBoundingClientRect();
    const cs = getComputedStyle(v);
    const px = (p: string) => parseFloat(cs.getPropertyValue(p)) || 0;
    const left = px('border-left-width') + px('padding-left');
    const top = px('border-top-width') + px('padding-top');
    return {
      left: r.left + left,
      top: r.top + top,
      width: r.width - left - px('border-right-width') - px('padding-right'),
      height: r.height - top - px('border-bottom-width') - px('padding-bottom'),
      fit: cs.objectFit,
      videoWidth: v.videoWidth,
      videoHeight: v.videoHeight,
    };
  });
}

export async function pagePoint(page: Page, selector: string, x: number, y: number) {
  const box = await videoBox(page, selector);
  if (!box.videoWidth || !box.videoHeight) throw new Error(`${selector} has no video frame yet`);
  return clientPoint(box, x, y);
}
