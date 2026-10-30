// The keys section of the status tab, folded behind its toggle in the
// tab's controls. It sends what the keyboard cannot: the chords the
// browser keeps for itself outside full screen, where keyboard lock cannot
// catch them, and the local clipboard typed as keys, for prompts that take
// no paste. It holds the user's macros too, which the CLI keeps in
// macros.json, since this page's origin is a new port every session and
// its storage goes with it. All of it goes out as the key and text
// controls the keyboard sends.

import {
  BUILT_INS, MAX_MACRO_PAUSE_MS, MAX_MACRO_STEPS, MAX_MACRO_TEXT_BYTES, Player, Recorder, checkMacro, checkName, newId, stepLabel,
  utf8Bytes, wait,
} from './macro.js';

const CLIPBOARD = 'clipboard';
const DEFAULT_PAUSE_MS = 500;
// CONFIRM_MS is how long Delete waits for the click that confirms it.
const CONFIRM_MS = 3000;
const SVG = 'http://www.w3.org/2000/svg';
const PENCIL = 'M10.5 3.5l2 2M2.5 13.5l.6-2.9 7.9-7.9a1.4 1.4 0 0 1 2 0l.3.3a1.4 1.4 0 0 1 0 2l-7.9 7.9z';

export class Keys {
  #overlay;
  #toggle;
  #signal;
  #input;
  #live;
  #sleep;
  #player;
  #section;
  #macroList;
  #editorBox;
  #note;
  #title;
  #withheld = '';
  #macros = null; // protocol.Macros, as the CLI last told
  #editor = null; // the macro being edited and its controls
  #recorder = null;

  // toggle is the tab's button that shows and hides the section. send
  // delivers a control message, and live says whether the control channel
  // is open. signal sends a message to the CLI and says whether it went.
  // input is the page's Input, whose keys a recording takes. sleep(ms,
  // signal) waits ms, or less once signal aborts.
  constructor({ overlay, toggle, send, live, signal, input, sleep = wait }) {
    this.#overlay = overlay;
    this.#toggle = toggle;
    this.#title = toggle.title;
    this.#signal = signal;
    this.#input = input;
    this.#live = live;
    this.#sleep = sleep;
    this.#player = new Player({ send, live, sleep });
    this.#build(overlay.addSection('keys'));
    toggle.setAttribute('aria-controls', this.#section.id);
    toggle.setAttribute('aria-expanded', 'false');
    toggle.addEventListener('click', (e) => {
      // A pointer's click gives the keyboard back to the desktop; Enter or
      // Space keeps it here, to go on into the section.
      if (e.detail) toggle.blur();
      this.#expand(this.#section.hidden);
    });
    window.addEventListener('blur', () => {
      this.#player.stop();
      this.#recorder?.lift();
    });
  }

  // configure takes the macros a Config carries.
  configure(macros) {
    this.#macros = macros ?? null;
    // A Config after a reconnect: the answer to a change sent before it
    // was lost with the socket.
    const settled = this.#editor?.pending && this.#settle();
    this.#renderMacros();
    if (macros?.error) this.#say(macros.error, { error: true });
    else if (settled) this.#say('');
  }

  // changed takes the CLI's answer to a change: the macros as they are
  // now, and why the change failed, if it did.
  changed(macros) {
    if (!macros) return;
    this.#macros = macros;
    if (this.#editor) this.#settle();
    if (this.#editor && !macros.error) this.#closeEditor();
    this.#renderMacros();
    this.#say(macros.error ?? '', { error: Boolean(macros.error) });
  }

  // withhold says why what the section plays cannot reach the desktop, as
  // on a guest's page without control, or lets it again for ''. Macros
  // can still be recorded and kept.
  withhold(reason) {
    this.#withheld = reason;
    this.#toggle.title = reason ? `Keys and macros: ${reason}` : this.#title;
    this.#say(reason);
  }

  // stop ends whatever plays, as when the control channel closed.
  stop() {
    this.#player.stop();
    this.#renderMacros();
  }

  #build(section) {
    this.#section = section;
    section.id = 'keys-section';
    section.hidden = true;
    const [keys, list] = row('Keys');
    for (const b of BUILT_INS) {
      list.append(button(b.label, b.title, () => this.#builtIn(b), 'key'));
    }
    let macroRow;
    [macroRow, this.#macroList] = row('Macros');
    this.#editorBox = document.createElement('div');
    this.#editorBox.className = 'macro-editor';
    this.#editorBox.dataset.keys = 'local';
    this.#editorBox.hidden = true;
    this.#note = document.createElement('p');
    this.#note.className = 'keys-note';
    this.#note.setAttribute('role', 'status');
    this.#note.hidden = true;
    section.append(keys, macroRow, this.#editorBox, this.#note);
    this.#renderMacros();
  }

  // expand shows the section, and holds the tab out while it shows, or
  // folds it away.
  #expand(on) {
    this.#section.hidden = !on;
    this.#section.toggleAttribute('data-pinned', on);
    this.#toggle.setAttribute('aria-expanded', String(on));
    if (!on) this.#stopRecording();
    this.#overlay.reveal();
  }

  // say shows a note under the section; an error says so by its look.
  #say(text, { error = false } = {}) {
    this.#note.textContent = text;
    this.#note.hidden = !text;
    if (error && text) this.#note.dataset.tone = 'error';
    else delete this.#note.dataset.tone;
  }

  // builtIn sends a built-in chord, or puts it in the macro being recorded.
  #builtIn(b) {
    if (this.#recorder) this.#addStep({ keys: b.keys.map((k) => ({ ...k })) });
    else this.#play([{ keys: b.keys }], b.label);
  }

  async #typeClipboard() {
    if (this.#withheld) {
      this.#say(this.#withheld, { error: true });
      return;
    }
    let text;
    try {
      text = await navigator.clipboard.readText();
    } catch (err) {
      this.#say(err?.name === 'NotAllowedError'
        ? 'The browser blocked reading the clipboard. Allow clipboard access for this page, or paste instead'
        : `Could not read the clipboard: ${err?.message ?? err}`, { error: true });
      return;
    }
    if (!text) {
      this.#say('The clipboard holds no text', { error: true });
    } else if (utf8Bytes(text) > MAX_MACRO_TEXT_BYTES) {
      this.#say(`The clipboard holds over ${MAX_MACRO_TEXT_BYTES / 1024} KiB of text; paste it instead`, { error: true });
    } else if (this.#recorder) {
      this.#addStep({ text });
    } else {
      this.#play([{ text }], CLIPBOARD);
    }
  }

  #play(steps, name) {
    if (this.#withheld) {
      this.#say(this.#withheld, { error: true });
      return;
    }
    if (!this.#live()) {
      this.#say('Not connected to the desktop', { error: true });
      return;
    }
    this.#say('');
    const played = this.#player.play(steps, name);
    this.#renderMacros();
    played.then(() => this.#renderMacros());
  }

  // renderMacros fills the Macros row: Type clipboard, the built-in macro,
  // then the user's, then New macro while no editor is open.
  #renderMacros() {
    const items = [button('Type clipboard', 'Type the text on this computer’s clipboard as keys, for a prompt that takes no paste',
      () => this.#typeClipboard(), 'key clipboard')];
    for (const m of this.#macros?.list ?? []) {
      const playing = this.#player.playing === m.id;
      const where = m.host ? `, only in sessions to ${m.host}` : '';
      const play = button(m.name, playing ? `Stop ${m.name}` : `Play ${m.name}${where}`, () => this.#playOrStop(m), 'macro');
      play.dataset.macro = m.id;
      play.setAttribute('aria-pressed', String(playing));
      if (playing) play.dataset.playing = '';
      const edit = button('', `Edit ${m.name}`, (e) => this.#edit(m, e), 'edit');
      edit.setAttribute('aria-label', `Edit ${m.name}`);
      edit.append(pencil());
      const group = document.createElement('span');
      group.className = 'macro-group';
      group.append(play, edit);
      items.push(group);
    }
    if (this.#macros && !this.#editor) {
      const add = button('New macro', 'Record keys, add text and pauses, and keep them as a macro', (e) => this.#edit(null, e), 'new');
      if (this.#macros.readOnly) {
        add.disabled = true;
        add.title = this.#macros.error;
      }
      items.push(add);
    }
    this.#macroList.replaceChildren(...items);
  }

  #playOrStop(m) {
    if (this.#player.playing === m.id) this.stop();
    else this.#play(m.steps, m.id);
  }

  // edit opens the editor on m, or on a new macro when m is null. Opened
  // from the keyboard, it puts the focus in the name.
  #edit(m, click) {
    if (this.#editor) this.#closeEditor();
    const e = { id: m?.id ?? newId(), steps: structuredClone(m?.steps ?? []), at: 0, removes: [] };
    this.#editor = e;
    const box = this.#editorBox;

    e.name = document.createElement('input');
    e.name.type = 'text';
    e.name.id = 'macro-name';
    e.name.className = 'macro-name';
    e.name.value = m?.name ?? '';
    e.name.setAttribute('aria-label', 'Macro name');
    e.name.addEventListener('input', () => e.name.removeAttribute('aria-invalid'));
    const head = div('editor-row', fieldLabel('Name', e.name), e.name);
    const host = this.#macros?.host;
    if (host) {
      e.pin = document.createElement('input');
      e.pin.type = 'checkbox';
      // A new macro stays on this host until unticked: what one host's
      // session recorded should not play into every other.
      e.pin.checked = m ? Boolean(m.host) : true;
      const pin = document.createElement('label');
      pin.className = 'pin';
      pin.title = `Show this macro only in sessions to ${host}, the host as typed`;
      pin.append(e.pin, document.createTextNode(` Only on ${host}`));
      head.append(pin);
    }

    e.list = document.createElement('ol');
    e.list.className = 'steps';
    e.list.setAttribute('aria-label', 'Steps');
    e.list.addEventListener('keydown', (ev) => this.#onStepKey(ev));
    e.empty = document.createElement('p');
    e.empty.className = 'hint';
    e.empty.textContent = 'No steps yet: record keys, or add text or a pause.';

    // Record keys lets go of the focus, unlike the other editor controls:
    // keys pressed on a page control stay there, out of the recording, and
    // only a click can stop a recording that takes every key.
    e.record = button('Record keys', 'Press keys to add them; nothing reaches the desktop while recording',
      () => this.#toggleRecording(), 'record');
    e.recording = document.createElement('span');
    e.recording.className = 'hint recording';
    e.recording.textContent = 'Recording: nothing reaches the desktop until you stop';
    e.recording.hidden = true;
    e.pause = document.createElement('input');
    e.pause.type = 'number';
    e.pause.className = 'pause';
    e.pause.min = '1';
    e.pause.max = String(MAX_MACRO_PAUSE_MS);
    e.pause.step = '100';
    e.pause.value = String(DEFAULT_PAUSE_MS);
    e.pause.setAttribute('aria-label', 'Pause in milliseconds');
    const ms = document.createElement('span');
    ms.className = 'hint';
    ms.textContent = 'ms';
    const addPause = button('Add pause', 'Wait this long before the next step', () => this.#addPause(), 'add-pause', { keepFocus: true });
    const tools = div('editor-row', e.record, e.recording, addPause, e.pause, ms);

    e.text = document.createElement('textarea');
    e.text.id = 'macro-text';
    e.text.className = 'macro-text';
    e.text.rows = 2;
    e.text.setAttribute('aria-label', 'Text to type');
    const plain = document.createElement('p');
    plain.className = 'hint';
    plain.textContent = 'Text is saved as plain text in macros.json on this computer.';
    const addText = button('Add text', 'Type this text as a step', () => this.#addText(), 'add-text', { keepFocus: true });
    const text = div('editor-row', fieldLabel('Text', e.text), e.text, addText);

    e.save = button('Save', '', () => this.#save(), 'save');
    const actions = div('editor-row actions');
    if (m) {
      e.remove = button('Delete macro', `Delete ${m.name} from macros.json`, () => this.#delete(), 'delete', { keepFocus: true });
      actions.append(e.remove);
    }
    actions.append(e.save, button('Cancel', '', () => this.#closeEditor(), 'cancel'));

    box.replaceChildren(head, e.list, e.empty, tools, text, plain, actions);
    box.hidden = false;
    this.#renderSteps();
    this.#renderMacros();
    if (!click?.detail) e.name.focus();
  }

  // renderSteps lists the steps. Their remove buttons are one stop for
  // Tab, the arrows move between them and Delete removes one; with focus
  // set, the focus goes to the step at e.at, or to Record keys once none
  // is left.
  #renderSteps(focus = false) {
    const e = this.#editor;
    e.at = Math.max(0, Math.min(e.at, e.steps.length - 1));
    e.removes = [];
    e.list.replaceChildren(...e.steps.map((step, i) => {
      const li = document.createElement('li');
      const label = document.createElement('span');
      label.textContent = stepLabel(step);
      // The label cuts long text short; the whole of it is a hover away.
      if (step.text) li.title = step.text;
      const remove = button('×', `Remove step ${i + 1}`, () => this.#removeStep(i), 'remove', { keepFocus: true });
      remove.setAttribute('aria-label', `Remove step ${i + 1}, ${label.textContent}`);
      remove.tabIndex = i === e.at ? 0 : -1;
      remove.addEventListener('focus', () => this.#rove(i));
      e.removes.push(remove);
      li.append(label, remove);
      return li;
    }));
    e.list.hidden = !e.steps.length;
    e.empty.hidden = e.steps.length > 0;
    if (focus) (e.removes[e.at] ?? e.record).focus();
  }

  #rove(at) {
    const e = this.#editor;
    e.at = at;
    for (const [i, r] of e.removes.entries()) r.tabIndex = i === at ? 0 : -1;
  }

  #onStepKey(ev) {
    const e = this.#editor;
    const n = e.steps.length;
    if (!n) return;
    let at;
    switch (ev.key) {
      case 'ArrowRight':
      case 'ArrowDown':
        at = (e.at + 1) % n;
        break;
      case 'ArrowLeft':
      case 'ArrowUp':
        at = (e.at - 1 + n) % n;
        break;
      case 'Home':
        at = 0;
        break;
      case 'End':
        at = n - 1;
        break;
      case 'Delete':
      case 'Backspace':
        ev.preventDefault();
        this.#removeStep(e.at);
        return;
      default:
        return;
    }
    ev.preventDefault();
    this.#rove(at);
    e.removes[at].focus();
  }

  #removeStep(i) {
    const e = this.#editor;
    e.steps.splice(i, 1);
    e.at = i;
    this.#renderSteps(true);
  }

  // addStep adds a step to the macro being edited. Text that follows text
  // joins it, so typed letters make one step. A recording that reaches the
  // cap stops there.
  #addStep(step) {
    const e = this.#editor;
    if (!e) return;
    const last = e.steps.at(-1);
    if (step.text && last?.text && utf8Bytes(last.text + step.text) <= MAX_MACRO_TEXT_BYTES) {
      last.text += step.text;
      this.#renderSteps();
      return;
    }
    if (e.steps.length >= MAX_MACRO_STEPS) {
      const recording = Boolean(this.#recorder);
      this.#stopRecording();
      this.#say(`A macro takes ${MAX_MACRO_STEPS} steps at most${recording ? '; recording stopped there' : ''}`, { error: true });
      return;
    }
    e.steps.push(step);
    this.#renderSteps();
  }

  #addText() {
    const e = this.#editor;
    const text = e.text.value;
    e.text.focus();
    if (!text) return;
    if (utf8Bytes(text) > MAX_MACRO_TEXT_BYTES) {
      this.#say(`Text is ${MAX_MACRO_TEXT_BYTES / 1024} KiB at most`, { error: true });
      return;
    }
    e.text.value = '';
    this.#addStep({ text });
  }

  #addPause() {
    const ms = Number(this.#editor.pause.value);
    if (!Number.isInteger(ms) || ms < 1 || ms > MAX_MACRO_PAUSE_MS) {
      this.#say(`A pause is 1 to ${MAX_MACRO_PAUSE_MS} ms`, { error: true });
      return;
    }
    this.#addStep({ pause: ms });
  }

  #toggleRecording() {
    if (this.#recorder) {
      this.#stopRecording();
      return;
    }
    const e = this.#editor;
    this.#player.stop();
    this.#recorder = new Recorder((step) => this.#addStep(step));
    this.#input.capture = (msg) => this.#recorder?.key(msg);
    e.record.textContent = 'Stop recording';
    e.record.dataset.recording = '';
    e.recording.hidden = false;
    this.#editorBox.dataset.recording = '';
    this.#say('');
  }

  #stopRecording() {
    if (!this.#recorder) return;
    const e = this.#editor;
    this.#recorder = null;
    this.#input.capture = null;
    e.record.textContent = 'Record keys';
    delete e.record.dataset.recording;
    e.recording.hidden = true;
    delete this.#editorBox.dataset.recording;
  }

  #save() {
    const e = this.#editor;
    this.#stopRecording();
    const m = { id: e.id, name: e.name.value.trim(), steps: e.steps };
    if (e.pin?.checked) m.host = this.#macros.host;
    const named = checkName(m.name);
    if (named) {
      e.name.setAttribute('aria-invalid', 'true');
      e.name.focus();
      this.#say(named, { error: true });
      return;
    }
    const why = checkMacro(m);
    if (why) {
      this.#say(why, { error: true });
      return;
    }
    this.#change({ put: m }, 'Saving…');
  }

  // delete asks for a second click within CONFIRM_MS, since nothing
  // brings a deleted macro back, then asks the CLI to delete it.
  #delete() {
    const e = this.#editor;
    if (!e.confirm) {
      const confirm = new AbortController();
      e.confirm = confirm;
      e.remove.textContent = 'Click again to delete';
      e.remove.dataset.confirm = '';
      this.#sleep(CONFIRM_MS, confirm.signal).then(() => {
        if (!confirm.signal.aborted) this.#unconfirm(e);
      });
      return;
    }
    this.#unconfirm(e);
    e.remove.blur();
    this.#stopRecording();
    this.#change({ delete: e.id }, 'Deleting…');
  }

  #unconfirm(e) {
    e.confirm?.abort();
    e.confirm = null;
    if (!e.remove) return;
    e.remove.textContent = 'Delete macro';
    delete e.remove.dataset.confirm;
  }

  #change(macroChange, doing) {
    const e = this.#editor;
    if (!this.#signal({ type: 'macros', macroChange })) {
      this.#say('Robin is not connected; nothing was saved', { error: true });
      return;
    }
    e.pending = true;
    e.save.disabled = true;
    if (e.remove) e.remove.disabled = true;
    this.#say(doing);
  }

  // settle lets the editor make another change, the last one answered or
  // lost. It reports whether one was waiting.
  #settle() {
    const e = this.#editor;
    const was = Boolean(e.pending);
    e.pending = false;
    e.save.disabled = false;
    if (e.remove) e.remove.disabled = false;
    return was;
  }

  #closeEditor() {
    this.#stopRecording();
    if (this.#editor) this.#unconfirm(this.#editor);
    this.#editor = null;
    this.#editorBox.replaceChildren();
    this.#editorBox.hidden = true;
    this.#renderMacros();
  }
}

// row is a line of the section: a label, and a list beside it for the
// buttons. It returns both the line and the list.
function row(text) {
  const label = document.createElement('span');
  label.className = 'keys-label';
  label.textContent = text;
  const list = div('keys-list');
  return [div('keys-row', label, list), list];
}

function div(className, ...children) {
  const d = document.createElement('div');
  d.className = className;
  d.append(...children);
  return d;
}

// fieldLabel is the visible name of an editor field.
function fieldLabel(text, control) {
  const l = document.createElement('label');
  l.className = 'field-label';
  l.htmlFor = control.id;
  l.textContent = text;
  return l;
}

function pencil() {
  const svg = document.createElementNS(SVG, 'svg');
  svg.setAttribute('viewBox', '0 0 16 16');
  svg.setAttribute('aria-hidden', 'true');
  const path = document.createElementNS(SVG, 'path');
  path.setAttribute('d', PENCIL);
  svg.append(path);
  return svg;
}

// button is a page control: it keeps no keys from the remote. Clicked, it
// gives the keyboard back to the remote, unless keepFocus: a control in
// the editor keeps it there, for the next one.
function button(label, title, onClick, className, { keepFocus = false } = {}) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = className;
  b.dataset.keys = 'local';
  b.textContent = label;
  if (title) b.title = title;
  b.addEventListener('click', (e) => {
    if (!keepFocus) b.blur();
    onClick(e);
  });
  return b;
}
