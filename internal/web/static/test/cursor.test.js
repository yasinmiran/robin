import { test } from 'node:test';
import assert from 'node:assert/strict';
import { MAX_CURSORS, MAX_CURSOR_PNG, RemoteCursor } from '../cursor.js';

const element = () => ({ style: { cursor: '' } });

test('a hidden pointer hides the cursor', () => {
  const el = element();
  new RemoteCursor(el).apply({ hidden: true, id: 1, png: 'AAAA' });
  assert.equal(el.style.cursor, 'none');
});

test('an image arrives once and is worn again by id', () => {
  const el = element();
  const c = new RemoteCursor(el);
  c.apply({ id: 7, png: 'iVBORw==', hotX: 3, hotY: 4 });
  const seven = 'url(data:image/png;base64,iVBORw==) 3 4, default';
  assert.equal(el.style.cursor, seven);

  c.apply({ id: 8, png: 'R0lG' });
  assert.equal(el.style.cursor, 'url(data:image/png;base64,R0lG) 0 0, default', 'hot spot defaults to the corner');

  c.apply({ id: 7 });
  assert.equal(el.style.cursor, seven);
});

test('an id the page never got an image for keeps the default arrow', () => {
  const el = element();
  new RemoteCursor(el).apply({ id: 99 });
  assert.equal(el.style.cursor, 'default');
});

test('the cache keeps the newest images to arrive, a re-sent one counting as new', () => {
  const el = element();
  const c = new RemoteCursor(el);
  for (let id = 1; id <= MAX_CURSORS; id++) c.apply({ id, png: 'AAAA' });
  c.apply({ id: 1, png: 'AAAB' });
  c.apply({ id: MAX_CURSORS + 1, png: 'AAAA' });
  assert.equal(c.images.size, MAX_CURSORS);
  c.apply({ id: 2 });
  assert.equal(el.style.cursor, 'default', 'the oldest image was kept');
  c.apply({ id: 1 });
  assert.equal(el.style.cursor, 'url(data:image/png;base64,AAAB) 0 0, default');
});

test('an image the agent may still name by id alone is never forgotten', () => {
  // The agent keeps 64 images and forgets them in the order the desktop
  // reported them, which the page never sees. Here the desktop reported
  // 1 to 64, the page was sent them from 64 down to 1, and then 65: the
  // agent forgot 1 and still counts 64 as sent.
  const el = element();
  const c = new RemoteCursor(el);
  for (let id = 64; id >= 1; id--) c.apply({ id, png: 'AAAA' });
  c.apply({ id: 65, png: 'AAAA' });
  c.apply({ id: 64 });
  assert.equal(el.style.cursor, 'url(data:image/png;base64,AAAA) 0 0, default');
});

test('an image outlives as many newer ones as the agent can send while it still counts it', () => {
  // The worst case: the agent held 63 others when it sent the target, so
  // 63 of those and 63 new ones may still follow it.
  const el = element();
  const c = new RemoteCursor(el);
  for (let id = 1; id <= 63; id++) c.apply({ id, png: 'AAAA' });
  c.apply({ id: 'target', png: 'iVBORw==' });
  for (let id = 1; id <= 126; id++) c.apply({ id: 1000 + id, png: 'AAAA' });
  c.apply({ id: 'target' });
  assert.equal(el.style.cursor, 'url(data:image/png;base64,iVBORw==) 0 0, default');
});

test('an image that is not base64, or a hot spot that is not a pixel, is dropped', () => {
  const el = element();
  const c = new RemoteCursor(el);
  c.apply({ id: 1, png: 'AAAA' });
  const worn = el.style.cursor;
  for (const bad of [
    { id: 2, png: 'x) , url(http://example.invalid/a' },
    { id: 3, png: 'AAA' },
    { id: 4, png: 42 },
    { id: 5, png: 'A'.repeat(MAX_CURSOR_PNG + 4) },
    { id: 6, png: 'AAAA', hotX: '1) , url(http://example.invalid/a' },
    { id: 7, png: 'AAAA', hotY: -1 },
    { id: 8, png: 'AAAA', hotX: 1.5 },
  ]) {
    c.apply(bad);
    assert.equal(el.style.cursor, worn, JSON.stringify(bad).slice(0, 80));
  }
  assert.equal(c.images.size, 1);
  c.apply({ id: 9, png: 'iVBORw==', hotX: 31, hotY: 0 });
  assert.equal(el.style.cursor, 'url(data:image/png;base64,iVBORw==) 31 0, default');
});
