import test from 'node:test';
import assert from 'node:assert/strict';
import { FakeElement, installDom } from './dom.js';

installDom();
Object.defineProperty(globalThis, 'navigator', { value: { platform: 'Linux x86_64', maxTouchPoints: 0 }, configurable: true });
globalThis.matchMedia = () => ({ matches: false });
globalThis.ResizeObserver = class { observe() {} };
globalThis.Element = FakeElement;
Object.assign(window, { innerWidth: 1280, innerHeight: 720 });
// input.js reads the device as it loads, so it loads once the stand-ins are up.
const { Input } = await import('../input.js');

function setup() {
  const text = new FakeElement('textarea');
  text.value = '';
  const sent = [];
  const input = new Input(new FakeElement('video'), text, new FakeElement('div'), (m) => sent.push(m), () => true);
  input.attach();
  const captured = [];
  return { input, text, sent, captured, record: () => { input.capture = (m) => captured.push(m); } };
}

const event = (type, props) => Object.assign(new Event(type, { cancelable: true }), props);
const key = (type, k, code) => window.dispatchEvent(event(type, { key: k, code, repeat: false }));

test('while recording, what an input method or a soft keyboard types goes to the recording and not to the desktop', () => {
  const { input, text, sent, captured, record } = setup();
  record();
  text.dispatchEvent(event('compositionend', { data: '日本' }));
  text.dispatchEvent(event('beforeinput', { inputType: 'insertText', data: 'a' }));
  text.dispatchEvent(event('beforeinput', { inputType: 'insertLineBreak' }));
  assert.deepEqual(sent, [], 'nothing reached the desktop');
  assert.deepEqual(captured, [
    { t: 'text', text: '日本' },
    { t: 'text', text: 'a' },
    { t: 'key', key: 'Enter', code: 'Enter', down: true, repeat: false },
    { t: 'key', key: 'Enter', code: 'Enter', down: false, repeat: false },
  ]);

  input.capture = null;
  text.dispatchEvent(event('compositionend', { data: '日本' }));
  assert.deepEqual(sent, [{ t: 'text', text: '日本' }], 'after the recording, typed text goes to the desktop again');
});

test('while recording, keys go to the recording, but the release of a key the desktop holds goes there', () => {
  const { input, sent, captured, record } = setup();
  key('keydown', 'Shift', 'ShiftLeft');
  record();
  key('keydown', 'a', 'KeyA');
  key('keyup', 'a', 'KeyA');
  key('keyup', 'Shift', 'ShiftLeft');
  assert.deepEqual(sent.map((m) => `${m.down ? '+' : '-'}${m.key}`), ['+Shift', '-Shift']);
  assert.deepEqual(captured.map((m) => `${m.down ? '+' : '-'}${m.key}`), ['+a', '-a']);
  input.capture = null;
});
