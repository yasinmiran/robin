// The keys section of the status tab. It sends what the keyboard cannot:
// the chords the browser keeps for itself outside full screen, where
// keyboard lock cannot catch them, and the local clipboard typed as keys,
// for prompts that take no paste. It holds the user's macros too, which
// the CLI keeps in macros.json, since this page's origin is a new port
// every session and its storage goes with it. All of it goes out as the
// key and text controls the keyboard sends.

import {
  BUILT_INS, MAX_MACRO_PAUSE_MS, MAX_MACRO_STEPS, MAX_MACRO_TEXT_BYTES, Player, Recorder, checkMacro, newId, stepLabel, utf8Bytes,
} from './macro.js';

const CLIPBOARD = 'clipboard';
const DEFAULT_PAUSE_MS = 500;

export class Keys {
  #overlay;
  #signal;
  #input;
  #live;
  #player;
  #section;
  #macroRow;
  #macroList;
  #editorBox;
  #note;
  #macros = null; // protocol.Macros, as the CLI last told
  #editor = null; // the macro being edited and its controls
  #recorder = null;

  // send delivers a control message, and live says whether the control
  // channel is open. signal sends a message to the CLI and says whether it
  // went. input is the page's Input, whose keys a recording takes.
  constructor({ overlay, send, live, signal, input }) {
    this.#overlay = overlay;
    this.#signal = signal;
    this.#input = input;
    this.#live = live;
    this.#player = new Player({ send, live });
    this.#build(overlay.addSection('keys'));
    window.addEventListener('blur', () => {
      this.#player.stop();
      this.#recorder?.lift();
    });
  }

  // configure takes the macros a Config carries.
  configure(macros) {
    this.#macros = macros ?? null;
    this.#renderMacros();
    if (macros?.error) this.#say(macros.error);
  }

  // changed takes the CLI's answer to a change: the macros as they are
  // now, and why the change failed, if it did.
  changed(macros) {
    if (!macros) return;
    this.#macros = macros;
    const e = this.#editor;
    if (e && !macros.error) {
      this.#closeEditor();
    } else if (e) {
      e.save.disabled = false;
      if (e.remove) e.remove.disabled = false;
    }
    this.#renderMacros();
    this.#say(macros.error ?? '');
  }

  // stop ends whatever plays, as when the control channel closed.
  stop() {
    this.#player.stop();
    this.#renderMacros();
  }

  #build(section) {
    this.#section = section;
    const [keys, list] = row('Keys', 'Keys the browser or the local system keeps for itself outside full screen');
    for (const b of BUILT_INS) {
      list.append(button(b.label, b.title, () => this.#builtIn(b), 'key'));
    }
    list.append(button('Type clipboard', 'Type the text on this computer’s clipboard as keys, for a prompt that takes no paste',
      () => this.#typeClipboard(), 'key clipboard'));
    [this.#macroRow, this.#macroList] = row('Macros');
    this.#macroRow.hidden = true;
    this.#editorBox = document.createElement('div');
    this.#editorBox.className = 'macro-editor';
    this.#editorBox.dataset.keys = 'local';
    this.#editorBox.hidden = true;
    this.#note = document.createElement('p');
    this.#note.className = 'keys-note';
    this.#note.setAttribute('role', 'status');
    this.#note.hidden = true;
    section.append(keys, this.#macroRow, this.#editorBox, this.#note);
  }

  #say(text) {
    this.#note.textContent = text;
    this.#note.hidden = !text;
  }

  // builtIn sends a built-in chord, or puts it in the macro being recorded.
  #builtIn(b) {
    if (this.#recorder) this.#addStep({ keys: b.keys.map((k) => ({ ...k })) });
    else this.#play([{ keys: b.keys }], b.label);
  }

  async #typeClipboard() {
    let text;
    try {
      text = await navigator.clipboard.readText();
    } catch (err) {
      this.#say(`The browser did not hand over the clipboard: ${err?.message ?? err}`);
      return;
    }
    if (!text) {
      this.#say('The clipboard holds no text');
    } else if (utf8Bytes(text) > MAX_MACRO_TEXT_BYTES) {
      this.#say(`The clipboard holds over ${MAX_MACRO_TEXT_BYTES / 1024} KiB of text; paste it instead`);
    } else if (this.#recorder) {
      this.#addStep({ text });
    } else {
      this.#play([{ text }], CLIPBOARD);
    }
  }

  #play(steps, name) {
    if (!this.#live()) {
      this.#say('Not connected to the desktop');
      return;
    }
    this.#say('');
    const played = this.#player.play(steps, name);
    this.#renderMacros();
    played.then(() => this.#renderMacros());
  }

  #renderMacros() {
    this.#macroRow.hidden = !this.#macros;
    if (!this.#macros) return;
    const items = [];
    for (const m of this.#macros.list ?? []) {
      const playing = this.#player.playing === m.id;
      const where = m.host ? `, only in sessions to ${m.host}` : '';
      const play = button(m.name, playing ? `Stop ${m.name}` : `Play ${m.name}${where}`, () => this.#toggle(m), 'macro');
      play.dataset.macro = m.id;
      if (playing) play.dataset.playing = '';
      const edit = button('✎', `Edit ${m.name}`, () => this.#edit(m), 'edit');
      edit.setAttribute('aria-label', `Edit ${m.name}`);
      const group = document.createElement('span');
      group.className = 'macro-group';
      group.append(play, edit);
      items.push(group);
    }
    if (!this.#editor) {
      items.push(button('New macro', 'Record keys, add text and pauses, and keep them as a macro', () => this.#edit(null), 'new'));
    }
    this.#macroList.replaceChildren(...items);
  }

  #toggle(m) {
    if (this.#player.playing === m.id) this.stop();
    else this.#play(m.steps, m.id);
  }

  #edit(m) {
    if (this.#editor) this.#closeEditor();
    const e = { id: m?.id ?? newId(), steps: structuredClone(m?.steps ?? []) };
    this.#editor = e;
    const box = this.#editorBox;

    e.name = document.createElement('input');
    e.name.type = 'text';
    e.name.className = 'macro-name';
    e.name.placeholder = 'Name';
    e.name.value = m?.name ?? '';
    e.name.setAttribute('aria-label', 'Macro name');
    const head = div('editor-row', e.name);
    const host = this.#macros?.host;
    if (host) {
      e.pin = document.createElement('input');
      e.pin.type = 'checkbox';
      e.pin.checked = Boolean(m?.host);
      const pin = document.createElement('label');
      pin.className = 'pin';
      pin.title = `Show this macro only in sessions to ${host}, the host as typed`;
      pin.append(e.pin, document.createTextNode(` Only on ${host}`));
      head.append(pin);
    }

    e.list = document.createElement('ol');
    e.list.className = 'steps';
    e.empty = document.createElement('p');
    e.empty.className = 'hint';
    e.empty.textContent = 'No steps yet: record keys, or add text or a pause.';

    e.record = button('Record keys', 'Press keys to add them; nothing reaches the desktop while recording', () => this.#toggleRecording(), 'record');
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
    const tools = div('editor-row', e.record, button('Add pause', 'Wait this long before the next step', () => this.#addPause(), 'add-pause'), e.pause, ms);

    e.text = document.createElement('textarea');
    e.text.className = 'macro-text';
    e.text.rows = 2;
    e.text.placeholder = 'Text to type';
    e.text.setAttribute('aria-label', 'Text to type');
    const plain = document.createElement('p');
    plain.className = 'hint';
    plain.textContent = 'Text is saved as plain text in macros.json on this computer.';
    const text = div('editor-row', e.text, button('Add text', 'Type this text as a step', () => this.#addText(), 'add-text'));

    e.save = button('Save', '', () => this.#save(), 'save');
    const actions = div('editor-row actions', e.save, button('Cancel', '', () => this.#closeEditor(), 'cancel'));
    if (m) {
      e.remove = button('Delete', `Delete ${m.name}`, () => this.#delete(), 'delete');
      actions.append(e.remove);
    }

    box.replaceChildren(head, e.list, e.empty, tools, text, plain, actions);
    box.hidden = false;
    this.#renderSteps();
    this.#renderMacros();
    this.#section.toggleAttribute('data-pinned', true);
    this.#overlay.reveal();
  }

  #renderSteps() {
    const e = this.#editor;
    e.list.replaceChildren(...e.steps.map((step, i) => {
      const li = document.createElement('li');
      const label = document.createElement('span');
      label.textContent = stepLabel(step);
      const remove = button('×', `Remove step ${i + 1}`, () => {
        e.steps.splice(i, 1);
        this.#renderSteps();
      }, 'remove');
      remove.setAttribute('aria-label', `Remove step ${i + 1}`);
      li.append(label, remove);
      return li;
    }));
    e.list.hidden = !e.steps.length;
    e.empty.hidden = e.steps.length > 0;
  }

  #addStep(step) {
    const e = this.#editor;
    if (!e) return;
    if (e.steps.length >= MAX_MACRO_STEPS) {
      this.#say(`A macro takes ${MAX_MACRO_STEPS} steps at most`);
      return;
    }
    e.steps.push(step);
    this.#renderSteps();
  }

  #addText() {
    const text = this.#editor.text.value;
    if (!text) return;
    if (utf8Bytes(text) > MAX_MACRO_TEXT_BYTES) {
      this.#say(`Text is ${MAX_MACRO_TEXT_BYTES / 1024} KiB at most`);
      return;
    }
    this.#editor.text.value = '';
    this.#addStep({ text });
  }

  #addPause() {
    const ms = Number(this.#editor.pause.value);
    if (!Number.isInteger(ms) || ms < 1 || ms > MAX_MACRO_PAUSE_MS) {
      this.#say(`A pause is 1 to ${MAX_MACRO_PAUSE_MS} ms`);
      return;
    }
    this.#addStep({ pause: ms });
  }

  #toggleRecording() {
    if (this.#recorder) {
      this.#stopRecording();
      return;
    }
    this.#player.stop();
    this.#recorder = new Recorder((step) => this.#addStep(step));
    this.#input.capture = (msg) => this.#recorder?.key(msg);
    this.#editor.record.textContent = 'Stop recording';
    this.#editor.record.dataset.recording = '';
    this.#say('Recording: press keys, or click a key above. Nothing reaches the desktop until you stop.');
  }

  #stopRecording() {
    if (!this.#recorder) return;
    this.#recorder = null;
    this.#input.capture = null;
    this.#editor.record.textContent = 'Record keys';
    delete this.#editor.record.dataset.recording;
    this.#say('');
  }

  #save() {
    const e = this.#editor;
    this.#stopRecording();
    const m = { id: e.id, name: e.name.value.trim(), steps: e.steps };
    if (e.pin?.checked) m.host = this.#macros.host;
    const why = checkMacro(m);
    if (why) {
      this.#say(why);
      return;
    }
    this.#change({ put: m }, 'Saving…');
  }

  #delete() {
    this.#stopRecording();
    this.#change({ delete: this.#editor.id }, 'Deleting…');
  }

  #change(macroChange, doing) {
    const e = this.#editor;
    if (!this.#signal({ type: 'macros', macroChange })) {
      this.#say('Robin is not connected; nothing was saved');
      return;
    }
    e.save.disabled = true;
    if (e.remove) e.remove.disabled = true;
    this.#say(doing);
  }

  #closeEditor() {
    this.#stopRecording();
    this.#editor = null;
    this.#editorBox.replaceChildren();
    this.#editorBox.hidden = true;
    this.#section.toggleAttribute('data-pinned', false);
    this.#renderMacros();
    this.#overlay.reveal();
  }
}

// row is a line of the section: a label, and a list beside it for the
// buttons. It returns both the line and the list.
function row(text, title) {
  const label = document.createElement('span');
  label.className = 'keys-label';
  label.textContent = text;
  if (title) label.title = title;
  const list = div('keys-list');
  return [div('keys-row', label, list), list];
}

function div(className, ...children) {
  const d = document.createElement('div');
  d.className = className;
  d.append(...children);
  return d;
}

// button is a page control: it keeps no keys from the remote, and gives
// the keyboard back once clicked.
function button(label, title, onClick, className) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = className;
  b.dataset.keys = 'local';
  b.textContent = label;
  if (title) b.title = title;
  b.addEventListener('click', () => {
    b.blur();
    onClick();
  });
  return b;
}
