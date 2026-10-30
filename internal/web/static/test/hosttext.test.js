import test from 'node:test';
import assert from 'node:assert/strict';
import { shown } from '../hosttext.js';

test('shown escapes the marks that reorder text, as the CLI does', () => {
  const marks = ['\u061c', '\u200e', '\u200f', '\u202a', '\u202b', '\u202c', '\u202d', '\u202e', '\u2066', '\u2067', '\u2068', '\u2069', '\u2028', '\u2029'];
  for (const m of marks) {
    assert.equal(shown(`a${m}b`), `a\\u${m.charCodeAt(0).toString(16).padStart(4, '0')}b`);
  }
  assert.equal(shown('invoice\u202efdp.exe'), 'invoice\\u202efdp.exe');
  assert.equal(shown('café, 2026.pdf'), 'café, 2026.pdf', 'other text stays as it is');
  assert.equal(shown('شاشة HDMI-1'), 'شاشة HDMI-1');
  assert.equal(shown(42), '42');
});
