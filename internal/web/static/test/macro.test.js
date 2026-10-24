import test from 'node:test';
import assert from 'node:assert/strict';
import {
  BUILT_INS, CHORD_HOLD_MS, MAX_MACROS_BYTES, MAX_MACRO_PAUSE_MS, MAX_MACRO_STEPS, MAX_MACRO_TEXT_BYTES, Player, Recorder,
  STEP_GAP_MS, chordLabel, checkMacro, newId, stepLabel,
} from '../macro.js';

const k = (key, code = key) => ({ key, code });
const macro = (steps, extra = {}) => ({ id: 'm1', name: 'Mine', steps, ...extra });

test('the built-in keys are the ones the browser keeps, and no console switch', () => {
  assert.deepEqual(BUILT_INS.map((b) => b.label), [
    'Ctrl+Alt+Delete', 'Alt+Tab', 'Alt+F4', 'Super', 'Ctrl+W', 'Ctrl+T', 'Ctrl+N', 'Ctrl+Q', 'Print Screen',
  ]);
  for (const b of BUILT_INS) {
    assert.ok(b.title, b.label);
    assert.equal(checkMacro(macro([{ keys: b.keys }])), '', b.label);
    const ctrlAlt = b.keys.some((x) => x.key === 'Control') && b.keys.some((x) => x.key === 'Alt');
    assert.ok(!(ctrlAlt && b.keys.some((x) => /^F\d+$/.test(x.key))), `${b.label} switches the host's console`);
  }
  assert.deepEqual(BUILT_INS[4].keys, [k('Control', 'ControlLeft'), k('w', 'KeyW')]);
});

test('chords and steps read as people write them', () => {
  assert.equal(chordLabel([k('Control', 'ControlRight'), k('Shift', 'ShiftLeft'), k('Escape')]), 'Ctrl+Shift+Esc');
  assert.equal(chordLabel([k('AltGraph'), k(' ', 'Space')]), 'AltGr+Space');
  assert.equal(chordLabel([k('OS', 'MetaLeft'), k('ArrowUp')]), 'Super+ArrowUp');
  assert.equal(stepLabel({ keys: [k('a', 'KeyA')] }), 'A');
  assert.equal(stepLabel({ text: 'ls -l\n\tx' }), '“ls -l⏎⇥x”');
  assert.equal(stepLabel({ text: 'x'.repeat(40) }), `“${'x'.repeat(31)}…”`);
  assert.equal(stepLabel({ text: '日'.repeat(32) }), `“${'日'.repeat(32)}”`);
  assert.equal(stepLabel({ pause: 500 }), 'Wait 500 ms');
});

test('checkMacro refuses what the CLI refuses', async (t) => {
  const chord = { keys: [k('Control', 'ControlLeft'), k('l', 'KeyL')] };
  const accepted = [
    ['every kind of step, pinned', macro([chord, { text: 'é\n' }, { pause: 1 }], { host: 'alice@box' })],
    ['text and a pause at their caps', macro([{ text: 'x'.repeat(MAX_MACRO_TEXT_BYTES) }, { pause: MAX_MACRO_PAUSE_MS }])],
    ['as many steps as a macro takes', macro(Array(MAX_MACRO_STEPS).fill(chord))],
    ['a name at its cap', { ...macro([chord]), name: 'é'.repeat(32) }],
    ['a key without a code', macro([{ keys: [k('a', '')] }])],
    ['an empty chord beside text', macro([{ keys: [], text: 'x' }])],
  ];
  for (const [name, m] of accepted) {
    await t.test(`takes ${name}`, () => assert.equal(checkMacro(m), ''));
  }

  const refused = [
    ['no ID', { ...macro([chord]), id: '' }, /no usable ID/],
    ['an ID with a slash', { ...macro([chord]), id: 'a/b' }, /no usable ID/],
    ['a long ID', { ...macro([chord]), id: 'x'.repeat(33) }, /no usable ID/],
    ['a blank name', { ...macro([chord]), name: '  ' }, /^Name the macro to save it$/],
    ['a name that is no string', { ...macro([chord]), name: 7 }, /^Name the macro to save it$/],
    ['a long name', { ...macro([chord]), name: 'é'.repeat(33) }, /^Shorten the name to 64 bytes or fewer$/],
    ['a name with a line break', { ...macro([chord]), name: 'a\nb' }, /no line breaks/],
    ['a host too long to pin to', macro([chord], { host: 'x'.repeat(256) }), /too long to pin/],
    ['no steps', macro([]), /Add a step/],
    ['no step list', { id: 'm1', name: 'M' }, /Add a step/],
    ['too many steps', macro(Array(MAX_MACRO_STEPS + 1).fill(chord)), /100 steps at most/],
    ['an empty step', macro([chord, {}]), /^Step 2: a step is one chord/],
    ['a step of two kinds', macro([{ keys: [k('a')], pause: 5 }]), /a step is one/],
    ['a chord of nine keys', macro([{ keys: Array(9).fill(k('a')) }]), /8 keys at most/],
    ['a key without a name', macro([{ keys: [k('')] }]), /no usable name/],
    ['a control character key', macro([{ keys: [k('\u001b', 'Escape')] }]), /no usable name/],
    ['a long key', macro([{ keys: [k('x'.repeat(33), 'KeyX')] }]), /no usable name/],
    ['a code with punctuation', macro([{ keys: [k('a', 'Key-A')] }]), /no usable code/],
    ['a long code', macro([{ keys: [k('a', 'K'.repeat(33))] }]), /no usable code/],
    ['long text', macro([{ text: 'x'.repeat(MAX_MACRO_TEXT_BYTES + 1) }]), /16 KiB at most/],
    ['a negative pause', macro([{ pause: -5 }]), /1 to 60000 ms/],
    ['a fractional pause', macro([{ pause: 1.5 }]), /1 to 60000 ms/],
    ['a long pause', macro([{ pause: MAX_MACRO_PAUSE_MS + 1 }]), /1 to 60000 ms/],
    ['more than the file holds', macro(Array(20).fill({ text: 'x'.repeat(MAX_MACRO_TEXT_BYTES) })), new RegExp(`over the ${MAX_MACROS_BYTES / 1024} KiB`)],
  ];
  for (const [name, m, want] of refused) {
    await t.test(`refuses ${name}`, () => assert.match(checkMacro(m), want));
  }
});

test('a new ID is one the CLI takes, and differs each time', () => {
  const a = newId();
  assert.match(a, /^[A-Za-z0-9_-]{1,32}$/);
  assert.notEqual(a, newId());
  assert.equal(newId(36 ** 4, () => 0.5), `10000-${(2 ** 39).toString(36)}`);
  assert.ok(newId(8.64e15, () => 0.9999999999).length <= 32);
});

// press and release feed the recorder what Input hands it.
const down = (key, code = key, repeat = false) => ({ t: 'key', key, code, down: true, repeat });
const up = (key, code = key) => ({ t: 'key', key, code, down: false, repeat: false });

function record(events) {
  const steps = [];
  const r = new Recorder((s) => steps.push(s));
  for (const e of events) r.key(e);
  return { steps, r };
}

test('recording takes each key with the modifiers held as it went down', () => {
  const { steps } = record([
    down('Control', 'ControlLeft'), down('Alt', 'AltLeft'), down('t', 'KeyT'), up('t', 'KeyT'), up('Alt', 'AltLeft'), up('Control', 'ControlLeft'),
    down('Control', 'ControlLeft'), down('Shift', 'ShiftLeft'), down('R', 'KeyR'), up('R', 'KeyR'), up('Shift', 'ShiftLeft'), up('Control', 'ControlLeft'),
    down('F4', 'F4'), up('F4', 'F4'),
  ]);
  assert.deepEqual(steps, [
    { keys: [k('Control', 'ControlLeft'), k('Alt', 'AltLeft'), k('t', 'KeyT')] },
    { keys: [k('Control', 'ControlLeft'), k('Shift', 'ShiftLeft'), k('R', 'KeyR')] },
    { keys: [k('F4')] },
  ]);
});

test('a key that types a character, alone or with Shift, is text', () => {
  const { steps } = record([
    down('e', 'KeyE'), up('e', 'KeyE'), down(' ', 'Space'), up(' ', 'Space'), down('é', 'Quote'), up('é', 'Quote'),
    down('Shift', 'ShiftLeft'), down('R', 'KeyR'), up('R', 'KeyR'), up('Shift', 'ShiftLeft'),
    down('Alt', 'AltLeft'), down('a', 'KeyA'), up('a', 'KeyA'), up('Alt', 'AltLeft'),
  ]);
  assert.deepEqual(steps, [{ text: 'e' }, { text: ' ' }, { text: 'é' }, { text: 'R' }, { keys: [k('Alt', 'AltLeft'), k('a', 'KeyA')] }],
    'Shift let go after typing is no chord of its own');
});

test('modifiers pressed and let go alone are a chord of their own', () => {
  assert.deepEqual(record([down('Meta', 'MetaLeft'), up('Meta', 'MetaLeft')]).steps, [{ keys: [k('Meta', 'MetaLeft')] }]);
  assert.deepEqual(record([down('Control', 'ControlLeft'), down('Shift', 'ShiftLeft'), up('Control', 'ControlLeft'), up('Shift', 'ShiftLeft')]).steps,
    [{ keys: [k('Control', 'ControlLeft'), k('Shift', 'ShiftLeft')] }]);
  // Held through two chords, Ctrl is no chord of its own afterwards.
  assert.deepEqual(record([
    down('Control', 'ControlLeft'), down('a', 'KeyA'), up('a', 'KeyA'), down('b', 'KeyB'), up('b', 'KeyB'), up('Control', 'ControlLeft'),
  ]).steps.map((s) => chordLabel(s.keys)), ['Ctrl+A', 'Ctrl+B']);
});

test('recording skips repeats, keys it never saw go down, and keys a chord cannot hold', () => {
  const { steps, r } = record([
    up('x', 'KeyX'), down('a', 'KeyA'), down('a', 'KeyA', true), down('a', 'KeyA'), up('a', 'KeyA'), down('', ''), down('y'.repeat(33), 'KeyY'),
  ]);
  assert.deepEqual(steps, [{ text: 'a' }]);
  // A window that lost the keyboard never sends the releases; lift starts over.
  r.key(down('Control', 'ControlLeft'));
  r.lift();
  r.key(down('b', 'KeyB'));
  assert.deepEqual(steps.at(-1), { text: 'b' });
  r.key(up('b', 'KeyB'));
  r.key(up('Control', 'ControlLeft'));
  assert.equal(steps.length, 2);
});

test('text an input method or a soft keyboard typed while recording is a text step', () => {
  const { steps } = record([
    { t: 'text', text: '日本' }, { t: 'text', text: '' }, down('Enter'), up('Enter'),
  ]);
  assert.deepEqual(steps, [{ text: '日本' }, { keys: [k('Enter')] }]);
});

test('a chord holds at most eight keys, the last ones pressed', () => {
  const mods = ['Shift', 'Control', 'Alt', 'AltGraph', 'Meta', 'OS'];
  const events = [...mods.map((m, i) => down(m, `M${i}`)), down('Shift', 'ShiftRight'), down('Control', 'ControlRight'), down('a', 'KeyA')];
  const { steps } = record(events);
  assert.equal(steps[0].keys.length, 8);
  assert.deepEqual(steps[0].keys.at(-1), k('a', 'KeyA'));
  assert.equal(steps[0].keys[0].code, 'M1');
});

// clocked is a Player whose sleeps go on a log and end when the test says.
function clocked({ live = () => true } = {}) {
  const log = [];
  const sleeps = [];
  let woken = null;
  const player = new Player({
    send: (m) => log.push(m.t === 'key' ? `${m.down ? '+' : '-'}${m.key}` : `text:${m.text.length}`),
    live,
    sleep: (ms, signal) => new Promise((resolve) => {
      log.push(`wait ${ms}`);
      sleeps.push({ ms, resolve });
      signal.addEventListener('abort', resolve);
      woken?.();
    }),
  });
  // asleep resolves once the player waits in a sleep the test has not ended.
  const asleep = () => (sleeps.length ? Promise.resolve() : new Promise((resolve) => { woken = resolve; }));
  // run ends every sleep as it comes until the play ends.
  const run = async (played) => {
    let done = false;
    played.then(() => {
      done = true;
      woken?.();
    });
    for (;;) {
      await asleep();
      if (done) return played;
      sleeps.shift().resolve();
    }
  };
  return { player, log, sleeps, asleep, run };
}

test('a macro plays its steps with the gaps between them', async () => {
  const { player, log, run } = clocked();
  const steps = [
    { keys: [k('Control', 'ControlLeft'), k('Alt', 'AltLeft'), k('t', 'KeyT')] },
    { pause: 400 },
    { text: 'é'.repeat(3000) },
    { keys: [k('Enter')] },
  ];
  assert.equal(await run(player.play(steps, 'm1')), 'done');
  assert.deepEqual(log, [
    '+Control', '+Alt', '+t', `wait ${CHORD_HOLD_MS}`, '-t', '-Alt', '-Control',
    `wait ${STEP_GAP_MS}`, 'wait 400',
    `wait ${STEP_GAP_MS}`, 'text:2048', 'text:952',
    `wait ${STEP_GAP_MS}`, '+Enter', `wait ${CHORD_HOLD_MS}`, '-Enter',
  ]);
  assert.equal(player.playing, null);
});

test('stop ends a macro at once and lets go of what it holds', async () => {
  const { player, log, asleep } = clocked();
  const played = player.play([{ keys: [k('Alt', 'AltLeft'), k('Tab')] }, { keys: [k('a', 'KeyA')] }], 'm1');
  await asleep();
  assert.equal(player.playing, 'm1');
  assert.deepEqual(log, ['+Alt', '+Tab', `wait ${CHORD_HOLD_MS}`]);
  player.stop();
  assert.deepEqual(log.slice(3), ['-Tab', '-Alt'], 'let go before stop returns');
  assert.equal(await played, 'stopped');
  assert.equal(log.length, 5, 'nothing after');
  assert.equal(player.playing, null);
  player.stop();
});

test('a long pause ends with stop', async () => {
  const { player, log, asleep } = clocked();
  const played = player.play([{ pause: MAX_MACRO_PAUSE_MS }, { text: 'never' }], 'm1');
  await asleep();
  player.stop();
  assert.equal(await played, 'stopped');
  assert.deepEqual(log, [`wait ${MAX_MACRO_PAUSE_MS}`]);
});

test('a closed channel ends a macro, which sends nothing more', async () => {
  let open = true;
  const { player, log, sleeps, asleep } = clocked({ live: () => open });
  const played = player.play([{ keys: [k('a', 'KeyA')] }, { keys: [k('b', 'KeyB')] }], 'm1');
  await asleep();
  open = false;
  sleeps.shift().resolve();
  assert.equal(await played, 'stopped');
  assert.deepEqual(log, ['+a', `wait ${CHORD_HOLD_MS}`]);

  open = true;
  const { player: p2, log: log2, sleeps: s2, asleep: asleep2 } = clocked({ live: () => open });
  const second = p2.play([{ keys: [k('a', 'KeyA')] }, { keys: [k('b', 'KeyB')] }], 'm2');
  await asleep2();
  s2.shift().resolve();
  await asleep2();
  open = false;
  s2.shift().resolve();
  assert.equal(await second, 'stopped');
  assert.deepEqual(log2, ['+a', `wait ${CHORD_HOLD_MS}`, '-a', `wait ${STEP_GAP_MS}`]);
});

test('playing another macro ends the one playing', async () => {
  const { player, log, asleep, run } = clocked();
  const first = player.play([{ keys: [k('Shift', 'ShiftLeft'), k('A', 'KeyA')] }], 'one');
  await asleep();
  const second = player.play([{ keys: [k('b', 'KeyB')] }], 'two');
  assert.equal(player.playing, 'two');
  assert.equal(await first, 'stopped');
  assert.equal(await run(second), 'done');
  assert.deepEqual(log, ['+Shift', '+A', `wait ${CHORD_HOLD_MS}`, '-A', '-Shift', '+b', `wait ${CHORD_HOLD_MS}`, '-b']);
});

test('the real clock waits, and stops waiting on stop', async () => {
  const sent = [];
  const player = new Player({ send: (m) => sent.push(m), live: () => true });
  const t0 = Date.now();
  assert.equal(await player.play([{ pause: 60 }, { text: 'x' }], 'm1'), 'done');
  assert.ok(Date.now() - t0 >= 60 + STEP_GAP_MS - 5, `took ${Date.now() - t0} ms`);
  assert.deepEqual(sent, [{ t: 'text', text: 'x' }]);

  const long = player.play([{ pause: MAX_MACRO_PAUSE_MS }], 'm2');
  const t1 = Date.now();
  setTimeout(() => player.stop(), 20);
  assert.equal(await long, 'stopped');
  assert.ok(Date.now() - t1 < 1000);

  const quick = new Player({ send: () => {}, live: () => true });
  const t2 = Date.now();
  const p = quick.play([{ pause: MAX_MACRO_PAUSE_MS }], 'm3');
  quick.stop();
  assert.equal(await p, 'stopped');
  assert.ok(Date.now() - t2 < 1000);
});
