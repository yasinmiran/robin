// The actual-size view: the picture laid out each of its pixels a whole
// number of device pixels, centred while it fits the window and panned
// while it does not. Every edge lands on a device pixel, so text in it is
// never resampled. Nothing here touches the DOM.

// EDGE_PX is the band along the window's edges where the pointer pans the
// picture, and PAN_PX the most one frame pans it, both in CSS pixels.
export const EDGE_PX = 24;
export const PAN_PX = 24;

// actualBox lays out a picture of pw by ph pixels in a window of ww by wh
// CSS pixels at devicePixelRatio dpr, shifted by pan, the picture's offset
// in CSS pixels where it is larger than the window. Each picture pixel
// takes as many device pixels as fit whole in a CSS pixel, at least one: a
// screen of twice the density shows it at two, the size fit gives a desktop
// that matches the window, rather than at half. It returns the box in CSS
// pixels, the pan as it may be, and whether the picture overflows the
// window on either axis.
export function actualBox(pw, ph, ww, wh, dpr, pan = { x: 0, y: 0 }) {
  const n = Math.max(1, Math.floor(dpr));
  const width = (pw * n) / dpr;
  const height = (ph * n) / dpr;
  const x = axis(width, ww, pan.x, dpr);
  const y = axis(height, wh, pan.y, dpr);
  return { left: x.at, top: y.at, width, height, pan: { x: x.pan, y: y.pan }, overflow: width > ww || height > wh };
}

function axis(size, room, pan, dpr) {
  const snap = (v) => Math.round(v * dpr) / dpr;
  if (size <= room) return { at: snap((room - size) / 2), pan: 0 };
  const at = snap(Math.min(Math.max(pan, room - size), 0));
  return { at, pan: at };
}

// edgePan is how far one frame shifts the pan for a pointer at x, y in a
// ww by wh window: toward whichever edge it is within EDGE_PX of, so the
// picture beyond that edge comes into view, faster the nearer the edge.
export function edgePan(x, y, ww, wh) {
  return { dx: push(x, ww), dy: push(y, wh) };
}

function push(at, room) {
  if (at < EDGE_PX) return Math.ceil((PAN_PX * (EDGE_PX - Math.max(at, 0))) / EDGE_PX);
  if (at > room - EDGE_PX) return -Math.ceil((PAN_PX * (Math.min(at, room) - (room - EDGE_PX))) / EDGE_PX);
  return 0;
}
