// The picture's local zoom and pan, and how a point on the page maps to a
// remote pixel through them.

export const MAX_ZOOM = 4;

// remotePoint maps the page point x, y to remote pixels. box is the video
// element's box as laid out, getBoundingClientRect's, which includes the
// zoom; vw and vh are the video's size. object-fit: contain letterboxes the
// picture, so it is scaled by the tighter axis and centred in the box.
export function remotePoint(x, y, box, vw, vh) {
  if (!vw || !vh) return null;
  const scale = Math.min(box.width / vw, box.height / vh);
  const rx = (x - box.left - (box.width - vw * scale) / 2) / scale;
  const ry = (y - box.top - (box.height - vh * scale) / 2) / scale;
  return {
    x: clamp(Math.floor(rx), 0, vw - 1),
    y: clamp(Math.floor(ry), 0, vh - 1),
    inside: rx >= 0 && ry >= 0 && rx < vw && ry < vh,
  };
}

// View is the zoom of the picture: a scale about the top-left corner of a
// width by height element, then a shift, in CSS pixels. At scale 1 the
// element is where the page lays it out; zoomed in, it always covers that
// place, so no gap opens at an edge.
export class View {
  scale = 1;
  x = 0;
  y = 0;
  #w;
  #h;

  constructor(width, height) {
    this.#w = width;
    this.#h = height;
  }

  get zoomed() {
    return this.scale > 1;
  }

  // transform is the CSS transform that shows the view, for an element
  // whose transform-origin is its top-left corner.
  get transform() {
    return this.zoomed ? `translate(${this.x}px, ${this.y}px) scale(${this.scale})` : '';
  }

  // pinch follows two fingers from one centre and spread to another: the
  // point of the picture under the first centre ends up under the second,
  // scaled by how much the spread grew.
  pinch(from, to) {
    const scale = clamp(this.scale * (to.span / from.span || 1), 1, MAX_ZOOM);
    const px = (from.cx - this.x) / this.scale;
    const py = (from.cy - this.y) / this.scale;
    this.scale = scale;
    this.x = to.cx - px * scale;
    this.y = to.cy - py * scale;
    this.#clamp();
  }

  resize(width, height) {
    this.#w = width;
    this.#h = height;
    this.#clamp();
  }

  reset() {
    this.scale = 1;
    this.x = 0;
    this.y = 0;
  }

  #clamp() {
    if (this.scale <= 1) {
      this.reset();
      return;
    }
    this.x = clamp(this.x, this.#w * (1 - this.scale), 0);
    this.y = clamp(this.y, this.#h * (1 - this.scale), 0);
  }
}

function clamp(n, lo, hi) {
  return Math.min(Math.max(n, lo), hi);
}
