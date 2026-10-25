import { test } from 'node:test';
import assert from 'node:assert/strict';
import { EDGE_PX, PAN_PX, actualBox, edgePan } from '../actualsize.js';

test('a picture that fits is centred at its own size', () => {
  assert.deepEqual(actualBox(800, 600, 1000, 700, 1), { left: 100, top: 50, width: 800, height: 600, pan: { x: 0, y: 0 }, overflow: false });
});

test('on a dense screen each picture pixel takes as many device pixels as fit whole', () => {
  // At a ratio of 2 a desktop the size of the window in CSS pixels keeps
  // that size, two device pixels to its pixel, rather than shrink to half.
  assert.deepEqual(actualBox(1000, 700, 1000, 700, 2), { left: 0, top: 0, width: 1000, height: 700, pan: { x: 0, y: 0 }, overflow: false });
  assert.deepEqual(actualBox(800, 600, 1000, 700, 3), { left: 100, top: 50, width: 800, height: 600, pan: { x: 0, y: 0 }, overflow: false });
  // At 2.5 two device pixels fit whole, so the picture is 4/5 of its size
  // in CSS pixels.
  assert.equal(actualBox(1000, 500, 1000, 700, 2.5).width, 800);
  // A ratio below 2 keeps one device pixel to the picture's pixel.
  assert.equal(actualBox(1000, 500, 1000, 700, 1.5).width, 1000 / 1.5);
});

test('a picture larger than the window pans, never past its edges', () => {
  const box = actualBox(2560, 1440, 1000, 700, 1, { x: -300, y: 50 });
  assert.deepEqual(box, { left: -300, top: 0, width: 2560, height: 1440, pan: { x: -300, y: 0 }, overflow: true });
  assert.deepEqual(actualBox(2560, 1440, 1000, 700, 1, { x: -5000, y: -5000 }).pan, { x: -1560, y: -740 });
  assert.equal(actualBox(1280, 600, 1000, 700, 1).overflow, true, 'wider only');
  assert.equal(actualBox(800, 800, 1000, 700, 1).overflow, true, 'taller only');
});

test('every edge lands on a device pixel', () => {
  // 1.25 device pixels to the CSS pixel, as on a 125% display.
  const box = actualBox(1001, 2000, 1000, 700, 1.25, { x: 0, y: -100.3 });
  assert.equal(box.width, 800.8);
  for (const v of [box.left, box.top]) assert.equal(Number.isInteger(v * 1.25), true, `${v} is between device pixels`);
  assert.equal(box.top, -100);
  assert.equal(Number.isInteger(box.left * 1.25), true);
});

test('the pointer at an edge pans toward it, faster the nearer it is', () => {
  assert.deepEqual(edgePan(500, 300, 1000, 700), { dx: 0, dy: 0 });
  assert.deepEqual(edgePan(0, 300, 1000, 700), { dx: PAN_PX, dy: 0 });
  assert.deepEqual(edgePan(-10, 300, 1000, 700), { dx: PAN_PX, dy: 0 });
  assert.deepEqual(edgePan(1000, 700, 1000, 700), { dx: -PAN_PX, dy: -PAN_PX });
  const near = edgePan(1000 - 2, 300, 1000, 700).dx;
  const far = edgePan(1000 - EDGE_PX + 2, 300, 1000, 700).dx;
  assert.ok(near < far && far < 0, `${near} nearer the edge, ${far} further in`);
  assert.equal(edgePan(EDGE_PX, 300, 1000, 700).dx, 0);
});
