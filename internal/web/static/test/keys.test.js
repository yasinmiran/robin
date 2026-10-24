import test from 'node:test';
import assert from 'node:assert/strict';
import { fakeOverlay, installDom } from './dom.js';
import { Keys } from '../keys.js';
import { MAX_MACRO_STEPS, MAX_MACRO_TEXT_BYTES } from '../macro.js';

installDom();
const overlay = fakeOverlay();
const section = overlay.section;
const later = (ms = 0) => new Promise((resolve) => setTimeout(resolve, ms));

let clipboard = async () => '';
Object.defineProperty(globalThis, 'navigator', {
  value: { clipboard: { readText: () => clipboard() } },
  configurable: true,
});

const sent = [];
const signals = [];
const input = { capture: null };
const link = { open: true, signal: true };
const keys = new Keys({
  overlay,
  send: (m) => sent.push(m.t === 'key' ? `${m.down ? '+' : '-'}${m.key}` : `text:${m.text}`),
  live: () => link.open,
  signal: (m) => {
    signals.push(m);
    return link.signal;
  },
  input,
});

const find = (match) => section.find(match);
const button = (text) => find((el) => el.tagName === 'BUTTON' && el.textContent === text);
const click = (text) => {
  const b = button(text);
  assert.ok(b, `a ${JSON.stringify(text)} button`);
  b.click();
};
const byClass = (cls) => find((el) => el.className === cls);
const note = () => byClass('keys-note');
const editor = () => byClass('macro-editor');
const steps = () => byClass('steps').children.map((li) => li.children[0].textContent);
const key = (k, code, down) => input.capture({ t: 'key', key: k, code, down, repeat: false });
// played waits for a short macro to finish.
const played = () => later(150);

const box = { host: 'alice@box', list: [] };

test('the built-in keys go out as chords, nothing held at the end', async () => {
  const [label, row] = section.children[0].children;
  assert.equal(label.textContent, 'Keys');
  assert.deepEqual(row.children.map((b) => b.textContent), [
    'Ctrl+Alt+Delete', 'Alt+Tab', 'Alt+F4', 'Super', 'Ctrl+W', 'Ctrl+T', 'Ctrl+N', 'Ctrl+Q', 'Print Screen', 'Type clipboard',
  ]);
  assert.ok(row.children.every((b) => b.dataset.keys === 'local' && b.title));
  click('Ctrl+W');
  await played();
  assert.deepEqual(sent.splice(0), ['+Control', '+w', '-w', '-Control']);

  link.open = false;
  click('Alt+Tab');
  assert.equal(note().textContent, 'Not connected to the desktop');
  assert.deepEqual(sent, []);
  link.open = true;
});

test('Type clipboard types the local clipboard on the click, and says when it cannot', async () => {
  clipboard = async () => 'pass\n';
  click('Type clipboard');
  await played();
  assert.deepEqual(sent.splice(0), ['text:pass\n']);

  clipboard = async () => { throw new Error('denied'); };
  click('Type clipboard');
  await later();
  assert.equal(note().textContent, 'The browser did not hand over the clipboard: denied');
  clipboard = async () => '';
  click('Type clipboard');
  await later();
  assert.equal(note().textContent, 'The clipboard holds no text');
  clipboard = async () => 'x'.repeat(MAX_MACRO_TEXT_BYTES + 1);
  click('Type clipboard');
  await later();
  assert.match(note().textContent, /over 16 KiB of text; paste it instead/);
  assert.deepEqual(sent, []);
});

test('no macros row until the CLI hands over macros, and its error shows', () => {
  keys.configure(undefined);
  assert.equal(section.children[1].hidden, true);
  keys.configure({ host: 'box', list: [], error: 'macros.json: bad; Robin leaves it as it is' });
  assert.equal(section.children[1].hidden, false);
  assert.equal(note().textContent, 'macros.json: bad; Robin leaves it as it is');
  assert.equal(note().hidden, false);
  keys.configure(box);
});

test('a new macro records keys that reach nothing else, then saves through the CLI', async () => {
  click('New macro');
  assert.equal(editor().hidden, false);
  assert.ok(section.hasAttribute('data-pinned'), 'the tab stays out while editing');
  assert.equal(button('New macro'), null);
  assert.match(byClass('hint').textContent, /No steps yet/);
  assert.ok(find((el) => el.className === 'hint' && /plain text in macros\.json/.test(el.textContent)), 'the page says text is plain');

  click('Save');
  assert.match(note().textContent, /a name of up to 64 bytes/);
  assert.deepEqual(signals, []);

  byClass('macro-name').value = '  Terminal  ';
  click('Record keys');
  assert.ok(input.capture);
  assert.equal(byClass('record').textContent, 'Stop recording');
  assert.match(note().textContent, /^Recording/);
  key('Control', 'ControlLeft', true);
  key('Alt', 'AltLeft', true);
  key('t', 'KeyT', true);
  key('t', 'KeyT', false);
  key('Alt', 'AltLeft', false);
  key('Control', 'ControlLeft', false);
  click('Alt+Tab'); // a key the browser keeps goes into the recording
  clipboard = async () => 'from the clipboard';
  click('Type clipboard');
  await later();
  // A soft keyboard's letters join the text step before them.
  input.capture({ t: 'text', text: ' and' });
  input.capture({ t: 'text', text: ' more' });
  // Blur loses the releases; the recording starts over from nothing held.
  key('Shift', 'ShiftLeft', true);
  window.dispatchEvent(new Event('blur'));
  key('x', 'KeyX', true);
  click('Stop recording');
  assert.equal(input.capture, null);
  assert.deepEqual(sent, [], 'nothing reached the desktop while recording');

  byClass('macro-text').value = 'echo hi\n';
  click('Add text');
  assert.equal(byClass('macro-text').value, '');
  click('Add text'); // empty: nothing
  byClass('pause').value = '0';
  click('Add pause');
  assert.match(note().textContent, /A pause is 1 to 60000 ms/);
  byClass('pause').value = '250';
  click('Add pause');
  assert.deepEqual(steps(), ['Ctrl+Alt+T', 'Alt+Tab', '“from the clipboard and more”', 'X', '“echo hi⏎”', 'Wait 250 ms']);
  find((el) => el.className === 'remove' && el.title === 'Remove step 4').click();
  assert.deepEqual(steps(), ['Ctrl+Alt+T', 'Alt+Tab', '“from the clipboard and more”', '“echo hi⏎”', 'Wait 250 ms']);

  find((el) => el.tagName === 'INPUT' && el.type === 'checkbox').checked = true;
  click('Save');
  assert.equal(note().textContent, 'Saving…');
  assert.equal(button('Save').disabled, true);
  const [signal] = signals.splice(0);
  assert.equal(signal.type, 'macros');
  const m = signal.macroChange.put;
  assert.match(m.id, /^[A-Za-z0-9_-]{1,32}$/);
  assert.equal(m.name, 'Terminal');
  assert.equal(m.host, 'alice@box');
  assert.deepEqual(m.steps, [
    { keys: [{ key: 'Control', code: 'ControlLeft' }, { key: 'Alt', code: 'AltLeft' }, { key: 't', code: 'KeyT' }] },
    { keys: [{ key: 'Alt', code: 'AltLeft' }, { key: 'Tab', code: 'Tab' }] },
    { text: 'from the clipboard and more' },
    { text: 'echo hi\n' },
    { pause: 250 },
  ]);

  // The CLI refuses: the editor stays, and says why.
  keys.changed({ ...box, error: 'the file is full' });
  assert.equal(editor().hidden, false);
  assert.equal(note().textContent, 'the file is full');
  assert.equal(button('Save').disabled, false);

  click('Save');
  signals.splice(0);
  keys.changed({ ...box, list: [m] });
  assert.equal(editor().hidden, true);
  assert.ok(!section.hasAttribute('data-pinned'));
  assert.equal(note().hidden, true);
  assert.equal(button('Terminal').title, 'Play Terminal, only in sessions to alice@box');
  box.list = [m];
});

test('a macro plays on a click, and a second click stops it', async () => {
  const lock = { id: 'lock', name: 'Lock', steps: [{ keys: [{ key: 'Meta', code: 'MetaLeft' }, { key: 'l', code: 'KeyL' }] }, { pause: 5000 }, { text: 'never' }] };
  keys.configure({ ...box, list: [...box.list, lock] });
  click('Lock');
  await later(80);
  assert.equal(button('Lock').dataset.playing, '');
  assert.equal(button('Lock').title, 'Stop Lock');
  click('Lock');
  await later();
  assert.deepEqual(sent.splice(0), ['+Meta', '+l', '-l', '-Meta']);
  assert.equal(button('Lock').dataset.playing, undefined);

  // Losing the keyboard and closing the channel end it the same way.
  click('Lock');
  await later(10);
  window.dispatchEvent(new Event('blur'));
  await later();
  assert.deepEqual(sent.splice(0), ['+Meta', '+l', '-l', '-Meta']);
  click('Lock');
  await later(10);
  keys.stop();
  await later();
  assert.deepEqual(sent.splice(0), ['+Meta', '+l', '-l', '-Meta']);
  keys.configure(box);
});

test('an edit shows the macro, and Delete asks the CLI to remove it', () => {
  find((el) => el.className === 'edit' && el.title === 'Edit Terminal').click();
  assert.equal(byClass('macro-name').value, 'Terminal');
  assert.equal(find((el) => el.type === 'checkbox').checked, true);
  assert.equal(steps().length, 5);
  // A second edit replaces the first.
  click('Cancel');
  assert.equal(editor().hidden, true);
  find((el) => el.className === 'edit').click();
  find((el) => el.className === 'edit').click();
  click('Delete');
  assert.deepEqual(signals.splice(0), [{ type: 'macros', macroChange: { delete: box.list[0].id } }]);
  assert.equal(note().textContent, 'Deleting…');
  assert.equal(button('Delete').disabled, true);
  keys.changed({ ...box, error: 'no' });
  assert.equal(button('Delete').disabled, false);
  keys.changed(undefined);
  keys.changed({ ...box, list: [] });
  assert.equal(editor().hidden, true);
  assert.equal(button('Terminal'), null);
});

test('the editor keeps to the bounds, and says when the CLI is out of reach', () => {
  keys.configure({ host: '', list: [] });
  click('New macro');
  assert.equal(find((el) => el.type === 'checkbox'), null, 'no host to pin to');
  byClass('macro-name').value = 'Long';
  byClass('macro-text').value = 'x'.repeat(MAX_MACRO_TEXT_BYTES + 1);
  click('Add text');
  assert.match(note().textContent, /Text is 16 KiB at most/);
  byClass('pause').value = '1';
  for (let i = 0; i < MAX_MACRO_STEPS; i++) click('Add pause');
  click('Add pause');
  assert.match(note().textContent, /100 steps at most/);
  assert.equal(steps().length, MAX_MACRO_STEPS);

  link.signal = false;
  click('Save');
  assert.match(note().textContent, /not connected; nothing was saved/);
  assert.ok(!button('Save').disabled);
  link.signal = true;
  signals.splice(0);
  click('Record keys');
  click('Cancel');
  assert.equal(input.capture, null, 'closing the editor stops the recording');
});
