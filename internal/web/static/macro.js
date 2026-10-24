// Key macros and the keys the browser keeps for itself, as the page plays
// them. A macro is a name and steps, each a chord, a run of text or a
// pause (protocol.Macro), and it goes out as the key and text controls the
// keyboard sends, so the agent needs nothing of its own for it. No DOM
// here, so node --test can check it.

import { textMessages } from './ime.js';

// The bounds the CLI keeps macros within, as internal/protocol states them.
export const MAX_MACROS = 100;
export const MAX_MACRO_STEPS = 100;
export const MAX_MACRO_CHORD_KEYS = 8;
export const MAX_MACRO_NAME_BYTES = 64;
export const MAX_MACRO_TEXT_BYTES = 16 * 1024;
export const MAX_MACRO_PAUSE_MS = 60000;
export const MAX_MACRO_KEY_BYTES = 32;
export const MAX_MACRO_HOST_BYTES = 255;
export const MAX_MACROS_BYTES = 256 * 1024;
export const MAX_MACRO_ID_BYTES = 32;

// How long a chord's keys stay down before they go up, and the wait
// between two steps, so the desktop sees each chord as pressed and let go.
export const CHORD_HOLD_MS = 20;
export const STEP_GAP_MS = 30;

const MODIFIERS = new Set(['Shift', 'Control', 'Alt', 'AltGraph', 'Meta', 'OS']);

const KEY_LABELS = {
  Control: 'Ctrl',
  Meta: 'Super',
  OS: 'Super',
  AltGraph: 'AltGr',
  ' ': 'Space',
  Escape: 'Esc',
  PrintScreen: 'Print Screen',
};

const key = (k, code = k) => ({ key: k, code });
const CTRL = key('Control', 'ControlLeft');
const ALT = key('Alt', 'AltLeft');

// BUILT_INS are the chords the browser keeps outside full screen, where
// keyboard lock cannot have them: pressed on the keyboard they would close
// the tab or switch the local windows instead. Ctrl+Alt+F keys are left
// out on purpose: on an attached desktop they move the host's console
// away from the session.
export const BUILT_INS = [
  { keys: [CTRL, ALT, key('Delete')], title: 'The desktop’s own Ctrl+Alt+Delete; the local system keeps the one on the keyboard' },
  { keys: [ALT, key('Tab')], title: 'Switch windows on the desktop, not locally' },
  { keys: [ALT, key('F4')], title: 'Close the desktop’s active window' },
  { keys: [key('Meta', 'MetaLeft')], title: 'The desktop’s Super key, which the local system keeps for itself' },
  { keys: [CTRL, key('w', 'KeyW')], title: 'Ctrl+W on the desktop; on the keyboard it closes this tab' },
  { keys: [CTRL, key('t', 'KeyT')], title: 'Ctrl+T on the desktop; on the keyboard it opens a tab' },
  { keys: [CTRL, key('n', 'KeyN')], title: 'Ctrl+N on the desktop; on the keyboard it opens a browser window' },
  { keys: [CTRL, key('q', 'KeyQ')], title: 'Ctrl+Q on the desktop; on the keyboard it can quit the browser' },
  { keys: [key('PrintScreen')], title: 'Print Screen on the desktop' },
].map((b) => ({ ...b, label: chordLabel(b.keys) }));

// chordLabel names a chord as people write it: Ctrl+Alt+Delete.
export function chordLabel(keys) {
  return keys.map((k) => KEY_LABELS[k.key] ?? (k.key.length === 1 ? k.key.toUpperCase() : k.key)).join('+');
}

// stepLabel is how the editor lists a step.
export function stepLabel(step) {
  if (step.keys?.length) return chordLabel(step.keys);
  if (step.text) return `“${shown(step.text)}”`;
  return `Wait ${step.pause} ms`;
}

// shown is text as one short line: line breaks and tabs as marks, the rest
// cut after 32 characters.
function shown(text) {
  const chars = [...text.replace(/\n/g, '⏎').replace(/\t/g, '⇥')];
  return chars.length > 32 ? `${chars.slice(0, 31).join('')}…` : chars.join('');
}

export function utf8Bytes(s) {
  return new TextEncoder().encode(s).length;
}

// printable reports whether s is text without control characters, as the
// CLI's unicode.IsControl sees them.
const printable = (s) => typeof s === 'string' && !/[\u0000-\u001f\u007f-\u009f]/.test(s);

// typesText reports whether a KeyboardEvent.key is the one character the
// key types, as opposed to a name such as Enter or F4.
const typesText = (k) => [...k].length === 1 && printable(k);

// checkMacro says what would make the CLI refuse m, as macros.Check does,
// or returns '' for a macro it takes.
export function checkMacro(m) {
  if (typeof m.id !== 'string' || !new RegExp(`^[A-Za-z0-9_-]{1,${MAX_MACRO_ID_BYTES}}$`).test(m.id)) return 'The macro has no usable ID';
  const why = checkName(m.name);
  if (why) return why;
  if (m.host !== undefined && (!printable(m.host) || utf8Bytes(m.host) > MAX_MACRO_HOST_BYTES)) return 'This host’s name is too long to pin a macro to';
  if (!Array.isArray(m.steps) || !m.steps.length) return 'Add a step: record keys, add text or a pause';
  if (m.steps.length > MAX_MACRO_STEPS) return `A macro takes ${MAX_MACRO_STEPS} steps at most`;
  for (const [i, step] of m.steps.entries()) {
    const why = checkStep(step);
    if (why) return `Step ${i + 1}: ${why}`;
  }
  if (utf8Bytes(JSON.stringify(m)) > MAX_MACROS_BYTES) return `The macro is over the ${MAX_MACROS_BYTES / 1024} KiB the file holds`;
  return '';
}

// checkName says what is wrong with a macro's name, or returns ''.
export function checkName(name) {
  if (typeof name !== 'string' || !name.trim()) return 'Name the macro to save it';
  if (!printable(name)) return 'A name takes no line breaks or other control characters';
  if (utf8Bytes(name) > MAX_MACRO_NAME_BYTES) return `Shorten the name to ${MAX_MACRO_NAME_BYTES} bytes or fewer`;
  return '';
}

function checkStep(step) {
  const kinds = [step.keys?.length > 0, Boolean(step.text), Boolean(step.pause)].filter(Boolean).length;
  if (kinds !== 1) return 'a step is one chord, one run of text or one pause';
  if (step.keys?.length) {
    if (step.keys.length > MAX_MACRO_CHORD_KEYS) return `a chord takes ${MAX_MACRO_CHORD_KEYS} keys at most`;
    for (const k of step.keys) {
      if (!k.key || !printable(k.key) || utf8Bytes(k.key) > MAX_MACRO_KEY_BYTES) return 'a key has no usable name';
      if (!/^[A-Za-z0-9]*$/.test(k.code ?? '') || (k.code ?? '').length > MAX_MACRO_KEY_BYTES) return 'a key has no usable code';
    }
    return '';
  }
  if (step.text) return utf8Bytes(step.text) > MAX_MACRO_TEXT_BYTES ? `text is ${MAX_MACRO_TEXT_BYTES / 1024} KiB at most` : '';
  return Number.isInteger(step.pause) && step.pause > 0 && step.pause <= MAX_MACRO_PAUSE_MS ? '' : `a pause is 1 to ${MAX_MACRO_PAUSE_MS} ms`;
}

// newId names a new macro; the CLI keys its changes by it.
export function newId(now = Date.now(), random = Math.random) {
  return `${now.toString(36)}-${Math.floor(random() * 2 ** 40).toString(36)}`;
}

// Recorder turns the keys pressed while recording into steps. A key that
// types one character, alone or with Shift, is text, so a typed command is
// one step and not one per letter. Any other key is a chord with the
// modifiers held as it went down, and modifiers pressed and let go with no
// other key, such as Super alone, are a chord of their own. It takes key
// and text messages as Input would send them, which go nowhere else
// meanwhile; text, from an input method or a soft keyboard, is text too.
export class Recorder {
  #held = new Map(); // code -> key, in the order pressed
  #lone = []; // modifiers pressed since every key was up, while no chord went out
  #chorded = false;
  #onStep;

  constructor(onStep) {
    this.#onStep = onStep;
  }

  key({ t, text, key: k, code = '', down, repeat }) {
    if (t === 'text') {
      if (text) this.#onStep({ text });
      return;
    }
    if (!down) {
      if (!this.#held.delete(code) || this.#held.size) return;
      if (!this.#chorded && this.#lone.length) this.#onStep({ keys: this.#lone });
      this.#lone = [];
      this.#chorded = false;
      return;
    }
    if (repeat || this.#held.has(code) || !k || utf8Bytes(k) > MAX_MACRO_KEY_BYTES) return;
    this.#held.set(code, k);
    if (MODIFIERS.has(k)) {
      if (!this.#chorded) this.#lone.push({ key: k, code });
      return;
    }
    this.#chorded = true;
    const keys = [];
    for (const [c, held] of this.#held) {
      if (c !== code && MODIFIERS.has(held)) keys.push({ key: held, code: c });
    }
    if (typesText(k) && keys.every((m) => m.key === 'Shift')) {
      this.#onStep({ text: k });
      return;
    }
    keys.push({ key: k, code });
    this.#onStep({ keys: keys.slice(-MAX_MACRO_CHORD_KEYS) });
  }

  // lift forgets the keys held, for when their releases go to another
  // window.
  lift() {
    this.#held.clear();
    this.#lone = [];
    this.#chorded = false;
  }
}

// Player plays steps through send, which takes one control message. A
// chord's keys go down in order and up in reverse after CHORD_HOLD_MS; text
// goes as text messages; steps are STEP_GAP_MS apart. live says whether
// the control channel is open. Once it is not, or stop is called (the
// window lost the keyboard), the macro ends there, and nothing it pressed
// stays down.
export class Player {
  #send;
  #live;
  #sleep;
  #run = null;

  // sleep(ms, signal) waits ms, or less once signal aborts.
  constructor({ send, live, sleep = wait }) {
    this.#send = send;
    this.#live = live;
    this.#sleep = sleep;
  }

  // playing is the name of what plays now, or null.
  get playing() {
    return this.#run?.name ?? null;
  }

  // play plays steps under a name, ending what played before, and
  // resolves with 'done', or 'stopped' when it ended early.
  async play(steps, name) {
    this.stop();
    const run = { name, abort: new AbortController(), held: [] };
    this.#run = run;
    try {
      for (const [i, step] of steps.entries()) {
        if (!this.#going(run)) return 'stopped';
        if (i > 0) {
          await this.#sleep(STEP_GAP_MS, run.abort.signal);
          if (!this.#going(run)) return 'stopped';
        }
        await this.#step(run, step);
      }
      return this.#going(run) ? 'done' : 'stopped';
    } finally {
      this.#letGo(run);
      if (this.#run === run) this.#run = null;
    }
  }

  stop() {
    const run = this.#run;
    if (!run) return;
    this.#run = null;
    run.abort.abort();
    this.#letGo(run);
  }

  #going(run) {
    return !run.abort.signal.aborted && this.#live();
  }

  async #step(run, step) {
    if (step.keys?.length) {
      for (const k of step.keys) {
        if (!this.#going(run)) return;
        run.held.push(k);
        this.#send(keyMessage(k, true));
      }
      await this.#sleep(CHORD_HOLD_MS, run.abort.signal);
      this.#letGo(run);
    } else if (step.text) {
      for (const m of textMessages(step.text)) this.#send(m);
    } else {
      await this.#sleep(step.pause, run.abort.signal);
    }
  }

  #letGo(run) {
    while (run.held.length) {
      const k = run.held.pop();
      if (this.#live()) this.#send(keyMessage(k, false));
    }
  }
}

function keyMessage(k, down) {
  return { t: 'key', key: k.key, code: k.code ?? '', down, repeat: false };
}

// wait waits ms, or less once signal aborts.
export function wait(ms, signal) {
  return new Promise((resolve) => {
    const done = () => {
      clearTimeout(timer);
      signal.removeEventListener('abort', done);
      resolve();
    };
    const timer = setTimeout(done, ms);
    signal.addEventListener('abort', done);
    if (signal.aborted) done();
  });
}
