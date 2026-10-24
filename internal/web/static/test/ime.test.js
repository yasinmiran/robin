import { test } from 'node:test';
import assert from 'node:assert/strict';
import { MAX_TEXT_BYTES, fromComposition, fromInput, imeOwns, textMessages } from '../ime.js';

const tap = (key) => [
  { t: 'key', key, code: key, down: true, repeat: false },
  { t: 'key', key, code: key, down: false, repeat: false },
];

test('keys an input method or a soft keyboard owns are not sent', () => {
  for (const e of [
    { key: 'a', isComposing: true },
    { key: 'Process', keyCode: 229 }, // Chrome's first key of a composition
    { key: 'a', keyCode: 229 },
    { key: 'Dead', code: 'Quote' },
    { key: 'Unidentified', code: '' }, // Android's soft keyboard
  ]) {
    assert.equal(imeOwns(e), true, JSON.stringify(e));
  }
  for (const e of [
    { key: 'a', code: 'KeyA', keyCode: 65 },
    { key: 'Enter', code: 'Enter', keyCode: 13 },
    { key: 'v', code: 'KeyV', ctrlKey: true, keyCode: 86 },
    { key: 'é', code: 'Digit2', keyCode: 50 },
  ]) {
    assert.equal(imeOwns(e), false, JSON.stringify(e));
  }
});

test('input outside a composition becomes text or keys', () => {
  assert.deepEqual(fromInput('insertText', 'h'), [{ t: 'text', text: 'h' }]);
  assert.deepEqual(fromInput('insertReplacementText', 'hello'), [{ t: 'text', text: 'hello' }]);
  assert.deepEqual(fromInput('insertText', null), []);
  assert.deepEqual(fromInput('insertLineBreak', null), tap('Enter'));
  assert.deepEqual(fromInput('insertParagraph', null), tap('Enter'));
  assert.deepEqual(fromInput('deleteContentBackward', null), tap('Backspace'));
  assert.deepEqual(fromInput('deleteWordBackward', null), tap('Backspace'));
  assert.deepEqual(fromInput('deleteContentForward', null), tap('Delete'));
  assert.deepEqual(fromInput('insertFromPaste', 'x'), []);
  assert.deepEqual(fromInput('formatBold', null), []);
});

test('a composition sends what it committed', () => {
  assert.deepEqual(fromComposition('日本語'), [{ t: 'text', text: '日本語' }]);
  assert.deepEqual(fromComposition(''), []);
  assert.deepEqual(fromComposition(undefined), []);
});

test('long text splits between characters within the byte limit', () => {
  // 3 bytes each, so 1365 fit in one message.
  const cjk = '語'.repeat(1400);
  const parts = textMessages(cjk);
  assert.deepEqual(parts.map((p) => p.text.length), [1365, 35]);
  // 4 bytes each, two UTF-16 units: never split in half.
  const emoji = '😀'.repeat(1100);
  const eparts = textMessages(emoji);
  assert.deepEqual(eparts.map((p) => [...p.text].length), [1024, 76]);
  assert.equal(eparts.map((p) => p.text).join(''), emoji);
  for (const p of [...parts, ...eparts]) assert.ok(new TextEncoder().encode(p.text).length <= MAX_TEXT_BYTES);
  assert.deepEqual(textMessages('aé'), [{ t: 'text', text: 'aé' }]);
  assert.deepEqual(textMessages(''), []);
});
