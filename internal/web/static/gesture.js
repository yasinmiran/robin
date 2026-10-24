// Turns fingers on the picture into what a mouse would do, and a local zoom.
// One finger taps to click, holds still to right click, and drags to drag.
// Two fingers drag to scroll, or pinch to zoom the picture and pan it;
// which of the two is settled by what they do first.

export const TAP_SLOP_PX = 10; // how far a finger may wander and still tap
export const LONG_PRESS_MS = 500;
export const PINCH_SLOP_PX = 24; // how far the spread moves before two fingers zoom
export const SCROLL_SLOP_PX = 8; // how far two fingers move together before they scroll

// Gestures reads touch pointers and calls emit with what they mean, in page
// coordinates:
//   { t: 'move', x, y }
//   { t: 'button', button, down, x, y }   MouseEvent.button numbering
//   { t: 'wheel', x, y, dx, dy }          over x, y, in wheel pixels
//   { t: 'pinch', from, to }              centres cx, cy and spreads
// timers stand in for setTimeout and clearTimeout.
export class Gestures {
  #emit;
  #timers;
  #fingers = new Map(); // pointerId -> { x, y, x0, y0 }
  // idle, press (one finger, undecided), drag, held (long press fired),
  // two (undecided), scroll, pinch, or spent (ignored until all lift).
  #mode = 'idle';
  #first = null;
  #start = null; // centre and spread of two fingers when the second came down
  #last = null; // and at the last move
  #timer = 0;

  constructor(emit, timers = { set: setTimeout, clear: clearTimeout }) {
    this.#emit = emit;
    this.#timers = timers;
  }

  get mode() {
    return this.#mode;
  }

  down(id, x, y) {
    this.#fingers.set(id, { x, y, x0: x, y0: y });
    if (this.#mode === 'idle') {
      this.#mode = 'press';
      this.#first = id;
      this.#timer = this.#timers.set(() => this.#longPress(), LONG_PRESS_MS);
    } else if (this.#mode === 'press' && this.#fingers.size === 2) {
      this.#timers.clear(this.#timer);
      this.#mode = 'two';
      this.#last = this.#pair();
      this.#start = this.#last;
    }
    // A finger more than the gesture needs is ignored.
  }

  move(id, x, y) {
    const f = this.#fingers.get(id);
    if (!f) return;
    f.x = x;
    f.y = y;
    switch (this.#mode) {
      case 'press':
        if (Math.hypot(x - f.x0, y - f.y0) <= TAP_SLOP_PX) return;
        this.#timers.clear(this.#timer);
        this.#mode = 'drag';
        this.#emit({ t: 'move', x: f.x0, y: f.y0 });
        this.#emit({ t: 'button', button: 0, down: true, x: f.x0, y: f.y0 });
        this.#emit({ t: 'move', x, y });
        return;
      case 'drag':
        if (id === this.#first) this.#emit({ t: 'move', x, y });
        return;
      case 'two': {
        const now = this.#pair();
        if (Math.abs(now.span - this.#start.span) > PINCH_SLOP_PX) this.#mode = 'pinch';
        else if (Math.hypot(now.cx - this.#start.cx, now.cy - this.#start.cy) > SCROLL_SLOP_PX) this.#mode = 'scroll';
        else return;
        this.#two(this.#start, now);
        return;
      }
      case 'scroll':
      case 'pinch':
        this.#two(this.#last, this.#pair());
    }
  }

  up(id, x, y) {
    this.#lift(id, x, y, true);
  }

  // cancel is a finger the browser took away: whatever it was doing ends
  // without a click.
  cancel(id) {
    const f = this.#fingers.get(id);
    if (f) this.#lift(id, f.x, f.y, false);
  }

  #lift(id, x, y, click) {
    if (!this.#fingers.delete(id)) return;
    switch (this.#mode) {
      case 'press':
        this.#timers.clear(this.#timer);
        if (click) {
          this.#emit({ t: 'move', x, y });
          this.#emit({ t: 'button', button: 0, down: true, x, y });
          this.#emit({ t: 'button', button: 0, down: false, x, y });
        }
        break;
      case 'drag':
        if (id !== this.#first) return;
        this.#emit({ t: 'move', x, y });
        this.#emit({ t: 'button', button: 0, down: false, x, y });
        break;
    }
    this.#mode = this.#fingers.size ? 'spent' : 'idle';
  }

  #longPress() {
    if (this.#mode !== 'press') return;
    const f = this.#fingers.get(this.#first);
    this.#mode = 'held';
    this.#emit({ t: 'move', x: f.x, y: f.y });
    this.#emit({ t: 'button', button: 2, down: true, x: f.x, y: f.y });
    this.#emit({ t: 'button', button: 2, down: false, x: f.x, y: f.y });
  }

  // two acts on two fingers moving from one centre and spread to another.
  // Scrolling, the content follows the fingers, as on a phone, so the
  // wheel turns the other way to the motion.
  #two(from, to) {
    this.#last = to;
    if (this.#mode === 'pinch') {
      this.#emit({ t: 'pinch', from, to });
      return;
    }
    const dx = from.cx - to.cx;
    const dy = from.cy - to.cy;
    if (dx || dy) this.#emit({ t: 'wheel', x: to.cx, y: to.cy, dx, dy });
  }

  // pair is the centre and spread of the first two fingers down.
  #pair() {
    const [a, b] = this.#fingers.values();
    return { cx: (a.x + b.x) / 2, cy: (a.y + b.y) / 2, span: Math.hypot(a.x - b.x, a.y - b.y) };
  }
}
