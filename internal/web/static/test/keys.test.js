import test from 'node:test';
import assert from 'node:assert/strict';
import { FakeElement, fakeOverlay, installDom } from './dom.js';
import { Keys } from '../keys.js';
import { MAX_MACRO_STEPS, MAX_MACRO_TEXT_BYTES } from '../macro.js';

installDom();
const turn = () => new Promise((resolve) => setImmediate(resolve));

let clipboard = async () => '';
Object.defineProperty(globalThis, 'navigator', {
  value: { clipboard: { readText: () => clipboard() } },
  configurable: true,
});

const box = (list = []) => ({ host: 'alice@box', list });
const lock = { id: 'lock', name: 'Lock', steps: [{ keys: [{ key: 'Meta', code: 'MetaLeft' }, { key: 'l', code: 'KeyL' }] }, { pause: 5000 }, { text: 'never' }] };
const terminal = { id: 'term', name: 'Terminal', host: 'alice@box', steps: [{ keys: [{ key: 'Control', code: 'ControlLeft' }, { key: 't', code: 'KeyT' }] }, { text: 'htop\n' }] };

// setup builds a Keys of its own, on a clock of the test's: a sleep lasts
// until the test ends it, or its signal aborts.
function setup(macros) {
  globalThis.window = new EventTarget();
  document.activeElement = document.body;
  clipboard = async () => '';
  const overlay = fakeOverlay();
  const toggle = new FakeElement('button');
  const sent = [];
  const signals = [];
  const sleeps = [];
  const input = { capture: null };
  const link = { open: true, signal: true };
  const keys = new Keys({
    overlay,
    toggle,
    send: (m) => sent.push(m.t === 'key' ? `${m.down ? '+' : '-'}${m.key}` : `text:${m.text}`),
    live: () => link.open,
    signal: (m) => {
      signals.push(m);
      return link.signal;
    },
    input,
    sleep: (ms, signal) => new Promise((resolve) => {
      sleeps.push({ ms, end: resolve });
      signal.addEventListener('abort', resolve);
    }),
  });
  if (macros) keys.configure(macros);
  const section = overlay.section;
  const find = (match) => section.find(match);
  const button = (text) => find((el) => el.tagName === 'BUTTON' && el.textContent === text);
  const t = {
    keys, toggle, section, sent, signals, sleeps, input, link, find, button,
    click(text) {
      const b = button(text);
      assert.ok(b, `a ${JSON.stringify(text)} button`);
      b.click();
    },
    byClass: (cls) => find((el) => el.className === cls),
    note: () => t.byClass('keys-note'),
    editor: () => t.byClass('macro-editor'),
    steps: () => t.byClass('steps').children.map((li) => li.children[0].textContent),
    removes: () => t.byClass('steps').children.map((li) => li.children[1]),
    macroRow: () => section.children[1].children[1].children.map((b) => b.textContent || b.children[0]?.textContent),
    // key hands a key to the recording as Input would, while there is one.
    key: (k, code, down) => input.capture?.({ t: 'key', key: k, code, down, repeat: false }),
    tap(k, code) {
      t.key(k, code, true);
      t.key(k, code, false);
    },
    // settle ends every sleep as it comes, as if the time passed, until
    // nothing waits.
    async settle() {
      for (;;) {
        await turn();
        const s = sleeps.shift();
        if (!s) return;
        s.end();
      }
    },
    // until ends the sleeps before the first of ms, and leaves that one.
    async until(ms) {
      for (;;) {
        await turn();
        if (sleeps.some((s) => s.ms === ms)) return;
        const s = sleeps.shift();
        assert.ok(s, `a sleep of ${ms} ms`);
        s.end();
      }
    },
  };
  return t;
}

test('the toggle folds the section away, and holds the tab out while it shows', () => {
  const t = setup(box());
  assert.equal(t.section.hidden, true, 'folded by default');
  assert.equal(t.toggle.getAttribute('aria-controls'), t.section.id);
  assert.equal(t.toggle.getAttribute('aria-expanded'), 'false');

  // From the keyboard the toggle keeps the focus, to go on into the section.
  t.toggle.focus();
  t.toggle.click();
  assert.equal(t.section.hidden, false);
  assert.ok(t.section.hasAttribute('data-pinned'), 'the tab stays out while the section shows');
  assert.equal(t.toggle.getAttribute('aria-expanded'), 'true');
  assert.equal(document.activeElement, t.toggle);

  // A pointer's click gives the keyboard back, and folding stops a recording.
  t.click('New macro');
  t.click('Record keys');
  assert.ok(t.input.capture);
  t.toggle.focus();
  t.toggle.dispatchEvent(Object.assign(new Event('click'), { detail: 1 }));
  assert.equal(t.section.hidden, true);
  assert.ok(!t.section.hasAttribute('data-pinned'));
  assert.equal(t.toggle.getAttribute('aria-expanded'), 'false');
  assert.equal(t.input.capture, null, 'folding the section stops the recording');
  assert.equal(document.activeElement, document.body);
});

test('the built-in keys go out as chords, nothing held at the end', async () => {
  const t = setup();
  const [label, row] = t.section.children[0].children;
  assert.equal(label.textContent, 'Keys');
  assert.deepEqual(row.children.map((b) => b.textContent), [
    'Ctrl+Alt+Delete', 'Alt+Tab', 'Alt+F4', 'Super', 'Ctrl+W', 'Ctrl+T', 'Ctrl+N', 'Ctrl+Q', 'Print Screen',
  ]);
  assert.ok(row.children.every((b) => b.dataset.keys === 'local' && b.title));
  t.button('Ctrl+W').focus();
  t.click('Ctrl+W');
  assert.equal(document.activeElement, document.body, 'a chord gives the keyboard back to the desktop');
  await t.settle();
  assert.deepEqual(t.sent.splice(0), ['+Control', '+w', '-w', '-Control']);

  t.link.open = false;
  t.click('Alt+Tab');
  assert.equal(t.note().textContent, 'Not connected to the desktop');
  assert.equal(t.note().dataset.tone, 'error');
  assert.deepEqual(t.sent, []);
});

test('Type clipboard leads the macros, types the local clipboard on the click, and says when it cannot', async () => {
  const t = setup();
  assert.deepEqual(t.macroRow(), ['Type clipboard'], 'with no macros from the CLI, Type clipboard alone');
  clipboard = async () => 'pass\n';
  t.click('Type clipboard');
  await t.settle();
  assert.deepEqual(t.sent.splice(0), ['text:pass\n']);

  const refusals = [
    [async () => { throw Object.assign(new Error('Read permission denied.'), { name: 'NotAllowedError' }); },
      'The browser blocked reading the clipboard. Allow clipboard access for this page, or paste instead'],
    [async () => { throw new Error('gone'); }, 'Could not read the clipboard: gone'],
    [async () => '', 'The clipboard holds no text'],
    [async () => 'x'.repeat(MAX_MACRO_TEXT_BYTES + 1), 'The clipboard holds over 16 KiB of text; paste it instead'],
  ];
  for (const [read, want] of refusals) {
    clipboard = read;
    t.click('Type clipboard');
    await turn();
    assert.equal(t.note().textContent, want);
    assert.equal(t.note().dataset.tone, 'error', want);
  }
  assert.deepEqual(t.sent, []);
});

test('no New macro until the CLI hands over macros, and none while their file does not read', () => {
  const t = setup();
  assert.equal(t.button('New macro'), null);
  t.keys.configure({ host: 'box', list: [], error: 'Robin can’t read macros.json (bad)', readOnly: true });
  assert.equal(t.note().textContent, 'Robin can’t read macros.json (bad)');
  assert.equal(t.note().dataset.tone, 'error');
  const add = t.button('New macro');
  assert.equal(add.disabled, true, 'nobody builds a macro that cannot be saved');
  assert.equal(add.title, 'Robin can’t read macros.json (bad)');
  t.keys.configure(box());
  assert.equal(t.button('New macro').disabled, undefined);
});

test('a new macro records keys that reach nothing else, then saves through the CLI', async () => {
  const t = setup(box());
  t.toggle.click();
  t.button('New macro').focus();
  t.click('New macro');
  assert.equal(t.editor().hidden, false);
  assert.equal(t.button('New macro'), null);
  assert.equal(document.activeElement, t.byClass('macro-name'), 'opened from the keyboard, the focus is in the name');
  assert.match(t.byClass('hint').textContent, /No steps yet/);
  assert.ok(t.find((el) => el.className === 'hint' && /plain text in macros\.json/.test(el.textContent)), 'the page says text is plain');
  assert.deepEqual(['Name', 'Text'], ['macro-name', 'macro-text'].map((id) => t.find((el) => el.htmlFor === id)?.textContent));

  document.activeElement = document.body;
  t.click('Save');
  assert.equal(t.note().textContent, 'Name the macro to save it');
  assert.equal(t.note().dataset.tone, 'error');
  assert.equal(t.byClass('macro-name').getAttribute('aria-invalid'), 'true');
  assert.equal(document.activeElement, t.byClass('macro-name'));
  assert.deepEqual(t.signals, []);
  t.byClass('macro-name').value = '  Terminal  ';
  t.byClass('macro-name').dispatchEvent(new Event('input'));
  assert.equal(t.byClass('macro-name').getAttribute('aria-invalid'), null);

  t.button('Record keys').focus();
  t.click('Record keys');
  assert.ok(t.input.capture);
  assert.notEqual(document.activeElement, t.button('Stop recording'), 'the button lets go of the keys it would keep');
  assert.equal(t.byClass('record').textContent, 'Stop recording');
  assert.equal(t.editor().dataset.recording, '');
  assert.equal(t.byClass('hint recording').hidden, false);
  t.key('Control', 'ControlLeft', true);
  t.key('Alt', 'AltLeft', true);
  t.tap('t', 'KeyT');
  t.key('Alt', 'AltLeft', false);
  t.key('Control', 'ControlLeft', false);
  // Typed letters, Shift's too, are one run of text.
  t.tap('l', 'KeyL');
  t.tap('s', 'KeyS');
  t.key('Shift', 'ShiftLeft', true);
  t.tap('A', 'KeyA');
  t.key('Shift', 'ShiftLeft', false);
  t.tap('Enter', 'Enter');
  t.click('Alt+Tab'); // a key the browser keeps goes into the recording
  clipboard = async () => 'from the clipboard';
  t.click('Type clipboard');
  await turn();
  // A soft keyboard's letters join the text step before them.
  t.input.capture({ t: 'text', text: ' and' });
  t.input.capture({ t: 'text', text: ' more' });
  // Blur loses the releases; the recording starts over from nothing held.
  t.key('Control', 'ControlLeft', true);
  window.dispatchEvent(new Event('blur'));
  t.key('F5', 'F5', true);
  t.click('Stop recording');
  assert.equal(t.input.capture, null);
  assert.equal(t.editor().dataset.recording, undefined);
  assert.deepEqual(t.sent, [], 'nothing reached the desktop while recording');

  t.byClass('macro-text').value = 'echo hi\n';
  t.click('Add text');
  assert.equal(t.byClass('macro-text').value, '');
  assert.equal(document.activeElement, t.byClass('macro-text'), 'Add text gives the focus back to the text');
  t.click('Add text'); // empty: nothing
  t.byClass('pause').value = '0';
  t.click('Add pause');
  assert.match(t.note().textContent, /A pause is 1 to 60000 ms/);
  t.byClass('pause').value = '250';
  t.button('Add pause').focus();
  t.click('Add pause');
  assert.equal(document.activeElement, t.button('Add pause'), 'an editor control keeps the focus');
  assert.deepEqual(t.steps(), ['Ctrl+Alt+T', '“lsA”', 'Enter', 'Alt+Tab', '“from the clipboard and more”', 'F5', '“echo hi⏎”', 'Wait 250 ms']);
  t.find((el) => el.className === 'remove' && el.title === 'Remove step 6').click();
  assert.deepEqual(t.steps(), ['Ctrl+Alt+T', '“lsA”', 'Enter', 'Alt+Tab', '“from the clipboard and more”', '“echo hi⏎”', 'Wait 250 ms']);
  assert.equal(document.activeElement, t.removes()[5], 'the focus goes on to the step that took its place');

  t.find((el) => el.type === 'checkbox').checked = true;
  t.click('Save');
  assert.equal(t.note().textContent, 'Saving…');
  assert.equal(t.note().dataset.tone, undefined);
  assert.equal(t.button('Save').disabled, true);
  const [signal] = t.signals.splice(0);
  assert.equal(signal.type, 'macros');
  const m = signal.macroChange.put;
  assert.match(m.id, /^[A-Za-z0-9_-]{1,32}$/);
  assert.equal(m.name, 'Terminal');
  assert.equal(m.host, 'alice@box');
  assert.deepEqual(m.steps, [
    { keys: [{ key: 'Control', code: 'ControlLeft' }, { key: 'Alt', code: 'AltLeft' }, { key: 't', code: 'KeyT' }] },
    { text: 'lsA' },
    { keys: [{ key: 'Enter', code: 'Enter' }] },
    { keys: [{ key: 'Alt', code: 'AltLeft' }, { key: 'Tab', code: 'Tab' }] },
    { text: 'from the clipboard and more' },
    { text: 'echo hi\n' },
    { pause: 250 },
  ]);

  // The CLI refuses: the editor stays, and says why.
  t.keys.changed({ ...box(), error: 'the file is full' });
  assert.equal(t.editor().hidden, false);
  assert.equal(t.note().textContent, 'the file is full');
  assert.equal(t.note().dataset.tone, 'error');
  assert.equal(t.button('Save').disabled, false);

  t.click('Save');
  t.signals.splice(0);
  t.keys.changed(box([m]));
  assert.equal(t.editor().hidden, true);
  assert.equal(t.note().hidden, true);
  assert.equal(t.button('Terminal').title, 'Play Terminal, only in sessions to alice@box');
  assert.ok(t.section.hasAttribute('data-pinned'), 'the section stays out after a save');
});

test('the steps are one stop for Tab, walked with the arrows, removed with Delete', () => {
  const t = setup(box([terminal]));
  t.find((el) => el.className === 'edit').click();
  t.byClass('pause').value = '100';
  t.click('Add pause');
  const tabStops = () => t.removes().map((r) => r.tabIndex);
  assert.deepEqual(tabStops(), [0, -1, -1]);
  const press = (key) => {
    const e = new Event('keydown', { cancelable: true });
    e.key = key;
    t.byClass('steps').dispatchEvent(e);
    return e.defaultPrevented;
  };
  assert.ok(press('ArrowRight'));
  assert.deepEqual(tabStops(), [-1, 0, -1]);
  assert.equal(document.activeElement, t.removes()[1]);
  press('End');
  assert.equal(document.activeElement, t.removes()[2]);
  press('ArrowDown');
  assert.equal(document.activeElement, t.removes()[0], 'the arrows wrap');
  press('ArrowUp');
  press('Home');
  press('ArrowLeft');
  assert.deepEqual(tabStops(), [-1, -1, 0]);
  assert.equal(press('a'), false, 'other keys are left alone');

  assert.ok(press('Delete'));
  assert.deepEqual(t.steps(), ['Ctrl+T', '“htop⏎”']);
  assert.equal(document.activeElement, t.removes()[1], 'the focus moves to the step before');
  press('Backspace');
  press('Backspace');
  assert.deepEqual(t.steps(), []);
  assert.equal(document.activeElement, t.byClass('record'), 'with no step left, the focus goes to Record keys');
  assert.equal(press('Delete'), false);
});

test('a macro plays on a click, and a second click stops it', async () => {
  const t = setup(box([lock]));
  t.click('Lock');
  await t.until(5000);
  assert.equal(t.button('Lock').dataset.playing, '');
  assert.equal(t.button('Lock').getAttribute('aria-pressed'), 'true');
  assert.equal(t.button('Lock').title, 'Stop Lock');
  t.click('Lock');
  await turn();
  assert.deepEqual(t.sent.splice(0), ['+Meta', '+l', '-l', '-Meta']);
  assert.equal(t.button('Lock').dataset.playing, undefined);
  assert.equal(t.button('Lock').getAttribute('aria-pressed'), 'false');

  // Losing the keyboard and closing the channel end it the same way.
  t.click('Lock');
  await t.until(5000);
  window.dispatchEvent(new Event('blur'));
  await turn();
  assert.deepEqual(t.sent.splice(0), ['+Meta', '+l', '-l', '-Meta']);
  t.click('Lock');
  await turn();
  t.keys.stop();
  await t.settle();
  assert.deepEqual(t.sent.splice(0), ['+Meta', '+l', '-l', '-Meta']);
  assert.equal(t.button('Lock').dataset.playing, undefined);
});

test('an edit shows the macro, and Delete asks for a second click before the CLI removes it', async () => {
  const t = setup(box([terminal]));
  t.find((el) => el.className === 'edit' && el.title === 'Edit Terminal').click();
  assert.equal(t.byClass('macro-name').value, 'Terminal');
  assert.equal(t.find((el) => el.type === 'checkbox').checked, true);
  assert.deepEqual(t.steps(), ['Ctrl+T', '“htop⏎”']);
  const actions = t.byClass('editor-row actions').children.map((b) => b.textContent);
  assert.deepEqual(actions, ['Delete macro', 'Save', 'Cancel'], 'Delete stands apart, at the start');
  // A second edit replaces the first.
  t.click('Cancel');
  assert.equal(t.editor().hidden, true);
  t.find((el) => el.className === 'edit').click();
  t.find((el) => el.className === 'edit').click();

  t.button('Delete macro').focus();
  t.click('Delete macro');
  assert.deepEqual(t.signals, [], 'one click deletes nothing');
  assert.equal(t.button('Click again to delete').dataset.confirm, '');
  assert.equal(document.activeElement, t.button('Click again to delete'), 'the focus stays for the second press');
  await t.settle(); // CONFIRM_MS passes
  assert.ok(t.button('Delete macro'), 'the second click is asked for a while only');
  t.click('Delete macro');
  t.click('Click again to delete');
  assert.deepEqual(t.signals.splice(0), [{ type: 'macros', macroChange: { delete: 'term' } }]);
  assert.equal(t.note().textContent, 'Deleting…');
  assert.equal(t.button('Delete macro').disabled, true);
  t.keys.changed({ ...box([terminal]), error: 'no' });
  assert.equal(t.button('Delete macro').disabled, false);
  t.keys.changed(undefined);
  t.keys.changed(box());
  assert.equal(t.editor().hidden, true);
  assert.equal(t.button('Terminal'), null);
});

test('a change whose answer a reconnect lost leaves the editor usable', () => {
  const t = setup(box([terminal]));
  t.find((el) => el.className === 'edit').click();
  t.click('Save');
  assert.equal(t.button('Save').disabled, true);
  t.keys.configure(box([terminal]));
  assert.equal(t.button('Save').disabled, false);
  assert.equal(t.button('Delete macro').disabled, false);
  assert.equal(t.note().hidden, true, 'Saving… is no longer true');
  t.byClass('macro-name').value = 'Kept';
  t.keys.configure(box([terminal]));
  assert.equal(t.byClass('macro-name').value, 'Kept', 'a Config with nothing pending leaves the editor alone');
});

test('the editor keeps to the bounds, and says when the CLI is out of reach', () => {
  const t = setup({ host: '', list: [] });
  t.click('New macro');
  assert.equal(t.find((el) => el.type === 'checkbox'), null, 'no host to pin to');
  t.byClass('macro-name').value = 'Long';
  t.byClass('macro-text').value = 'x'.repeat(MAX_MACRO_TEXT_BYTES + 1);
  t.click('Add text');
  assert.match(t.note().textContent, /Text is 16 KiB at most/);
  t.byClass('pause').value = '1';
  for (let i = 0; i < MAX_MACRO_STEPS; i++) t.click('Add pause');
  t.click('Add pause');
  assert.equal(t.note().textContent, 'A macro takes 100 steps at most');
  assert.equal(t.steps().length, MAX_MACRO_STEPS);

  // A recording that reaches the cap stops, and says so.
  t.click('Record keys');
  t.tap('F1', 'F1');
  assert.equal(t.input.capture, null);
  assert.equal(t.note().textContent, 'A macro takes 100 steps at most; recording stopped there');
  assert.equal(t.note().dataset.tone, 'error');

  t.link.signal = false;
  t.click('Save');
  assert.match(t.note().textContent, /not connected; nothing was saved/);
  assert.ok(!t.button('Save').disabled);
  t.link.signal = true;
  t.click('Record keys');
  t.click('Cancel');
  assert.equal(t.input.capture, null, 'closing the editor stops the recording');
});
