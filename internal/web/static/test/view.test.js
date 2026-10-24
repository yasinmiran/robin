import { test } from 'node:test';
import assert from 'node:assert/strict';
import { MAX_ZOOM, View, remotePoint } from '../view.js';

const box = (left, top, width, height) => ({ left, top, width, height });

test('remotePoint maps through the letterbox', () => {
  // A 1280x720 picture in a 1000x1000 box: scaled to 1000x562.5, centred.
  const b = box(0, 0, 1000, 1000);
  assert.deepEqual(remotePoint(500, 500, b, 1280, 720), { x: 640, y: 360, inside: true });
  assert.deepEqual(remotePoint(0, 218.75, b, 1280, 720), { x: 0, y: 0, inside: true });
  // In the bars: clamped to the edge, and not inside.
  assert.deepEqual(remotePoint(500, 10, b, 1280, 720), { x: 640, y: 0, inside: false });
  assert.deepEqual(remotePoint(1000, 990, b, 1280, 720), { x: 1279, y: 719, inside: false });
  assert.equal(remotePoint(1, 1, b, 0, 0), null);
});

test('remotePoint maps through a zoomed box', () => {
  // The layout box of a 1280x720 element zoomed 2x about the viewport's
  // centre, as getBoundingClientRect reports it.
  const v = new View(1280, 720);
  v.pinch({ cx: 640, cy: 360, span: 100 }, { cx: 640, cy: 360, span: 200 });
  const b = box(v.x, v.y, 1280 * v.scale, 720 * v.scale);
  assert.deepEqual(remotePoint(640, 360, b, 1280, 720), { x: 640, y: 360, inside: true });
  assert.deepEqual(remotePoint(0, 0, b, 1280, 720), { x: 320, y: 180, inside: true });
  assert.deepEqual(remotePoint(1279, 719, b, 1280, 720), { x: 959, y: 539, inside: true });
});

test('View zooms about the fingers and pans with them', () => {
  const v = new View(1280, 720);
  assert.equal(v.zoomed, false);
  assert.equal(v.transform, '');

  v.pinch({ cx: 640, cy: 360, span: 100 }, { cx: 640, cy: 360, span: 200 });
  assert.equal(v.scale, 2);
  assert.deepEqual([v.x, v.y], [-640, -360]);
  assert.equal(v.transform, 'translate(-640px, -360px) scale(2)');

  // Same spread, centre 100 px to the right: the picture follows.
  v.pinch({ cx: 640, cy: 360, span: 200 }, { cx: 740, cy: 360, span: 200 });
  assert.deepEqual([v.scale, v.x, v.y], [2, -540, -360]);

  // Panned past the left edge: held so the picture still covers it.
  v.pinch({ cx: 0, cy: 0, span: 200 }, { cx: 2000, cy: 2000, span: 200 });
  assert.deepEqual([v.x, v.y], [0, 0]);
  v.pinch({ cx: 2000, cy: 2000, span: 200 }, { cx: -5000, cy: -5000, span: 200 });
  assert.deepEqual([v.x, v.y], [-1280, -720]);
});

test('View keeps its zoom between 1 and MAX_ZOOM', () => {
  const v = new View(800, 600);
  v.pinch({ cx: 400, cy: 300, span: 10 }, { cx: 400, cy: 300, span: 1000 });
  assert.equal(v.scale, MAX_ZOOM);
  v.pinch({ cx: 400, cy: 300, span: 1000 }, { cx: 400, cy: 300, span: 10 });
  assert.deepEqual([v.scale, v.x, v.y, v.zoomed], [1, 0, 0, false]);
  // A spread of nothing changes nothing.
  v.pinch({ cx: 400, cy: 300, span: 0 }, { cx: 400, cy: 300, span: 0 });
  assert.equal(v.scale, 1);
});

test('View stays inside a resized viewport', () => {
  const v = new View(1280, 720);
  v.pinch({ cx: 1280, cy: 720, span: 100 }, { cx: 1280, cy: 720, span: 300 });
  assert.deepEqual([v.x, v.y], [-2560, -1440]);
  v.resize(640, 360);
  assert.deepEqual([v.x, v.y], [-1280, -720]);
  v.reset();
  assert.deepEqual([v.scale, v.x, v.y], [1, 0, 0]);
});
