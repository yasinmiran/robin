import { test } from 'node:test';
import assert from 'node:assert/strict';
import { ALONE_PX, Gestures, LONG_PRESS_MS, PINCH_SLOP_PX, TAP_SLOP_PX } from '../gesture.js';

// setup returns a Gestures whose timers run when fire is called, and the
// list of what it emitted.
function setup() {
  const out = [];
  let pending = null;
  let next = 1;
  const timers = {
    set(fn, ms) {
      assert.equal(ms, LONG_PRESS_MS);
      pending = { fn, id: next++ };
      return pending.id;
    },
    clear(id) {
      if (pending?.id === id) pending = null;
    },
  };
  const g = new Gestures((e) => out.push(e), timers);
  const fire = () => {
    const p = pending;
    pending = null;
    p?.fn();
  };
  return { g, out, fire, armed: () => pending !== null };
}

const click = (button, x, y) => [
  { t: 'button', button, down: true, x, y },
  { t: 'button', button, down: false, x, y },
];

test('a tap clicks where the finger lifts', () => {
  const { g, out, armed } = setup();
  g.down(1, 100, 100);
  g.move(1, 100 + TAP_SLOP_PX - 1, 100);
  g.up(1, 104, 101);
  assert.deepEqual(out, [{ t: 'move', x: 104, y: 101 }, ...click(0, 104, 101)]);
  assert.equal(armed(), false);
  assert.equal(g.mode, 'idle');
});

test('a long press right clicks once and nothing more', () => {
  const { g, out, fire } = setup();
  g.down(1, 50, 60);
  g.move(1, 53, 60);
  fire();
  assert.deepEqual(out, [{ t: 'move', x: 53, y: 60 }, ...click(2, 53, 60)]);
  g.move(1, 300, 300);
  g.up(1, 300, 300);
  assert.equal(out.length, 3);
  assert.equal(g.mode, 'idle');
});

test('one finger drags with the button held', () => {
  const { g, out, armed } = setup();
  g.down(1, 10, 10);
  g.move(1, 40, 10);
  assert.equal(armed(), false, 'a drag is no long press');
  g.move(1, 80, 20);
  g.down(2, 500, 500); // a second finger during a drag is ignored
  g.move(2, 600, 600);
  g.up(2, 600, 600);
  g.up(1, 90, 25);
  assert.deepEqual(out, [
    { t: 'move', x: 10, y: 10 },
    { t: 'button', button: 0, down: true, x: 10, y: 10 },
    { t: 'move', x: 40, y: 10 },
    { t: 'move', x: 80, y: 20 },
    { t: 'move', x: 90, y: 25 },
    { t: 'button', button: 0, down: false, x: 90, y: 25 },
  ]);
});

test('two fingers moving together scroll, the content following them', () => {
  const { g, out, armed } = setup();
  g.down(1, 100, 300);
  g.down(2, 200, 300);
  assert.equal(armed(), false, 'two fingers are no long press');
  g.move(1, 100, 280); // one finger alone settles nothing yet
  assert.deepEqual(out, []);
  g.move(2, 200, 280);
  assert.equal(g.mode, 'scroll');
  g.move(1, 100, 270);
  g.move(2, 200, 270);
  // Centre from 150,300 to 150,280, then on to 150,270: fingers going up
  // scroll down.
  assert.deepEqual(out, [
    { t: 'wheel', x: 150, y: 280, dx: 0, dy: 20 },
    { t: 'wheel', x: 150, y: 275, dx: 0, dy: 5 },
    { t: 'wheel', x: 150, y: 270, dx: 0, dy: 5 },
  ]);
  // The first finger lifting ends it; nothing clicks.
  g.up(1, 100, 270);
  g.move(2, 200, 100);
  g.up(2, 200, 100);
  assert.equal(out.length, 3);
  assert.equal(g.mode, 'idle');
});

test('two fingers spreading pinch, and keep pinching once they do', () => {
  const { g, out } = setup();
  g.down(1, 100, 100);
  g.down(2, 200, 100);
  g.move(1, 100, 100);
  g.move(2, 200 + PINCH_SLOP_PX + 1, 100);
  assert.equal(g.mode, 'pinch');
  assert.deepEqual(out, [
    { t: 'pinch', from: { cx: 150, cy: 100, span: 100 }, to: { cx: 150 + (PINCH_SLOP_PX + 1) / 2, cy: 100, span: 100 + PINCH_SLOP_PX + 1 } },
  ]);
  // Moving together now pans rather than scrolls.
  g.move(1, 110, 100);
  assert.equal(out.length, 2);
  assert.equal(out[1].t, 'pinch');
  assert.deepEqual(out[1].from, out[0].to);
  g.cancel(1);
  g.cancel(2);
  assert.equal(g.mode, 'idle');
});

test('fingers reported one at a time are judged once both have moved', () => {
  // Pinching in by 18 px a side: the first finger alone moves the centre
  // 9 px, as far as a scroll would, and the spread only 18 px.
  let { g } = setup();
  g.down(1, 300, 300);
  g.down(2, 700, 300);
  g.move(1, 318, 300);
  assert.equal(g.mode, 'two');
  g.move(2, 682, 300);
  assert.equal(g.mode, 'pinch');

  // Scrolling sideways with the fingers side by side: each alone changes
  // the spread, both together only the centre.
  ({ g } = setup());
  g.down(1, 300, 300);
  g.down(2, 400, 300);
  g.move(1, 320, 300);
  g.move(2, 420, 300);
  assert.equal(g.mode, 'scroll');

  // One finger still, the other far enough: settled anyway.
  ({ g } = setup());
  g.down(1, 300, 300);
  g.down(2, 400, 300);
  g.move(2, 400, 300 + ALONE_PX + 1);
  assert.equal(g.mode, 'scroll');
});

test('a cancelled finger clicks nothing and lets go of a drag', () => {
  const { g, out, armed } = setup();
  g.down(1, 10, 10);
  g.cancel(1);
  assert.deepEqual(out, []);
  assert.equal(armed(), false);

  g.down(1, 10, 10);
  g.move(1, 50, 50);
  g.cancel(1);
  assert.deepEqual(out.at(-1), { t: 'button', button: 0, down: false, x: 50, y: 50 });

  // Pointers it never saw go down change nothing.
  g.move(9, 1, 1);
  g.up(9, 1, 1);
  g.cancel(9);
  assert.equal(g.mode, 'idle');
});

test('the long press timer is ignored once the press became something else', () => {
  // A timer that fires anyway, after a second finger came down.
  const out = [];
  let fire;
  const g = new Gestures((e) => out.push(e), {
    set: (fn) => {
      fire = fn;
      return 1;
    },
    clear() {},
  });
  g.down(1, 0, 0);
  g.down(2, 50, 0);
  fire();
  assert.deepEqual(out, []);
  assert.equal(g.mode, 'two');
});
