// MAX_CURSORS bounds the images the page keeps. The agent keeps 64
// (maxCursors in internal/agent/session.go) and forgets them in the order
// the desktop reported them, which the page never sees. While the agent
// still counts an image as sent, at most 63 it already held and 63 new
// ones can be sent after it, so the newest 128 to arrive always hold it.
export const MAX_CURSORS = 128;
// MAX_CURSOR_PNG bounds one image's base64, as long as a whole control
// message from the agent may be (peer.MaxControlBytes).
export const MAX_CURSOR_PNG = 256 * 1024;

// The image and hot spot go into a CSS value, so only padded base64 and
// whole pixels get there.
const isBase64 = (s) => typeof s === 'string' && s.length <= MAX_CURSOR_PNG && s.length % 4 === 0 && /^[A-Za-z0-9+/]+={0,2}$/.test(s);
const isPixel = (n) => Number.isSafeInteger(n) && n >= 0;

// RemoteCursor shows the remote pointer image on the video. The agent sends
// each image's PNG once; later switches name it by id.
export class RemoteCursor {
  constructor(el) {
    this.el = el;
    this.images = new Map(); // oldest to arrive first
  }

  apply(c) {
    if (c.hidden) {
      this.el.style.cursor = 'none';
      return;
    }
    if (c.png !== undefined) {
      const { png, hotX = 0, hotY = 0 } = c;
      if (!isBase64(png) || !isPixel(hotX) || !isPixel(hotY)) return;
      this.images.delete(c.id);
      this.images.set(c.id, `url(data:image/png;base64,${png}) ${hotX} ${hotY}`);
      if (this.images.size > MAX_CURSORS) this.images.delete(this.images.keys().next().value);
    }
    const image = this.images.get(c.id);
    // An id this page never got an image for keeps the default arrow.
    this.el.style.cursor = image ? `${image}, default` : 'default';
  }
}
