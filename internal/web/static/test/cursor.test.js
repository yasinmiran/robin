import { test } from 'node:test';
import assert from 'node:assert/strict';
import { RemoteCursor } from '../cursor.js';

const element = () => ({ style: { cursor: '' } });

test('a hidden pointer hides the cursor', () => {
  const el = element();
  new RemoteCursor(el).apply({ hidden: true, id: 1, png: 'AAAA' });
  assert.equal(el.style.cursor, 'none');
});

test('an image arrives once and is worn again by id', () => {
  const el = element();
  const c = new RemoteCursor(el);
  c.apply({ id: 7, png: 'iVBOR', hotX: 3, hotY: 4 });
  const seven = 'url(data:image/png;base64,iVBOR) 3 4, default';
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
