// Tile batches as the agent marshals them, for the tile tests.

// pngHead is the start of a PNG of width by height pixels: its signature
// and IHDR, with no picture after.
export function pngHead(width, height) {
  const p = new Uint8Array(24);
  p.set([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 13, 0x49, 0x48, 0x44, 0x52]);
  const v = new DataView(p.buffer);
  v.setUint32(16, width);
  v.setUint32(20, height);
  return p;
}

// marshal lays a batch out as the agent does, with png after the index.
export function marshal({ rtp = 1, gen = 1, tiles, png }) {
  const out = new Uint8Array(10 + 4 * tiles.length + png.length);
  const v = new DataView(out.buffer);
  v.setUint32(0, rtp);
  v.setUint32(4, gen);
  v.setUint16(8, tiles.length);
  tiles.forEach((t, k) => v.setUint32(10 + 4 * k, t));
  out.set(png, 10 + 4 * tiles.length);
  return out;
}
