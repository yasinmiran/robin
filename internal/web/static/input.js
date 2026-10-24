// Turns local pointer, touch, keyboard, clipboard and viewport events into
// Control messages for the remote desktop.

import { touchFirst } from './device.js';
import { Gestures } from './gesture.js';
import { fromComposition, fromInput, imeOwns } from './ime.js';
import { View, remotePoint } from './view.js';

const LINE_PX = 40;
const PASTE_WAIT_MS = 100;
const PASTE_HOLD_MS = 3000;
const RESIZE_DEBOUNCE_MS = 250;
const MODIFIERS = new Set(['Shift', 'Control', 'Alt', 'AltGraph', 'Meta', 'OS']);

export const isMac = /Mac|iPhone|iPad/.test(navigator.userAgentData?.platform ?? navigator.platform);

export class Input {
  #video;
  #text;
  #preedit;
  #send;
  #active;
  #paste;
  #held = new Map(); // code -> key, as sent to the remote
  #buttons = 0;
  #move = null;
  #moveFrame = 0;
  #pasteQueue = null; // key messages held back while waiting for a paste event
  #pasteTimer = 0;
  #resizeTimer = 0;
  #view = new View(window.innerWidth, window.innerHeight);
  #gestures = new Gestures((g) => this.#onGesture(g));
  // scale turns CSS pixels into remote pixels: devicePixelRatio when the
  // session asked for HiDPI, 1 otherwise.
  scale = 1;

  // text is the hidden text box that holds the keyboard's focus, so input
  // methods and soft keyboards have somewhere to type, and preedit shows
  // what an input method is composing. send delivers one Control message;
  // active reports whether input should be captured at all, so a closed
  // session leaves the browser's own keys (reload, close tab) alone. paste
  // hands what a paste event carries to the remote clipboard, by default
  // its plain text; it may return a promise, which the paste key waits on
  // for up to PASTE_HOLD_MS so that it finds the clipboard set.
  constructor(video, text, preedit, send, active, { paste } = {}) {
    this.#video = video;
    this.#text = text;
    this.#preedit = preedit;
    this.#send = send;
    this.#active = active;
    this.#paste = paste ?? ((data) => {
      const plain = data?.getData('text/plain');
      if (plain) this.sendClipboard(plain);
    });
  }

  get view() {
    return this.#view;
  }

  // gesture is what the fingers on the picture are doing, as Gestures.mode.
  get gesture() {
    return this.#gestures.mode;
  }

  attach() {
    const v = this.#video;
    v.addEventListener('pointermove', (e) => this.#onPointer(e, 'move'));
    v.addEventListener('pointerdown', (e) => this.#onPointer(e, 'down'));
    v.addEventListener('pointerup', (e) => this.#onPointer(e, 'up'));
    v.addEventListener('pointercancel', (e) => this.#onPointer(e, 'cancel'));
    // No middle-click autoscroll, and the text box keeps the focus.
    v.addEventListener('mousedown', (e) => e.preventDefault());
    v.addEventListener('wheel', (e) => this.#onWheel(e), { passive: false });
    window.addEventListener('contextmenu', (e) => e.preventDefault());

    window.addEventListener('keydown', (e) => this.#onKey(e, true));
    window.addEventListener('keyup', (e) => this.#onKey(e, false));
    window.addEventListener('paste', (e) => this.#onPaste(e));

    const t = this.#text;
    t.addEventListener('compositionstart', () => this.#showPreedit(''));
    t.addEventListener('compositionupdate', (e) => this.#showPreedit(e.data));
    t.addEventListener('compositionend', (e) => this.#onCompositionEnd(e));
    t.addEventListener('beforeinput', (e) => this.#onBeforeInput(e));
    t.addEventListener('input', (e) => {
      if (!e.isComposing) t.value = '';
    });
    // Wherever the focus goes when a page control lets go of it, the text
    // box takes it back.
    document.addEventListener('focusout', () => setTimeout(() => this.#keepFocus()));
    this.#keepFocus();

    window.addEventListener('blur', () => this.release());
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'hidden') this.release();
    });
    window.addEventListener('resize', () => {
      this.#view.resize(window.innerWidth, window.innerHeight);
      this.#showView();
    });

    new ResizeObserver(() => {
      clearTimeout(this.#resizeTimer);
      this.#resizeTimer = setTimeout(() => this.sendResize(), RESIZE_DEBOUNCE_MS);
    }).observe(document.documentElement);
  }

  sendResize() {
    this.#send({ t: 'resize', w: Math.round(window.innerWidth * this.scale), h: Math.round(window.innerHeight * this.scale) });
  }

  sendClipboard(text) {
    this.#send({ t: 'clipboard', text: String(text) });
  }

  // release tells the remote to let go of every key and button, for when
  // this window stops seeing the matching key-up and button-up events.
  release() {
    this.#cancelPaste();
    this.#held.clear();
    this.#buttons = 0;
    this.#move = null;
    this.#send({ t: 'release' });
  }

  // toggleKeyboard raises the soft keyboard by focusing the text box, or
  // puts it away.
  toggleKeyboard() {
    if (document.activeElement === this.#text) this.#text.blur();
    else this.#text.focus({ preventScroll: true });
  }

  #keepFocus() {
    const at = document.activeElement;
    if (touchFirst || (at && at !== document.body && at !== document.documentElement)) return;
    this.#text.focus({ preventScroll: true });
  }

  // placeText moves the text box to a page point, so an input method opens
  // its candidates there, by the pointer, rather than in a corner.
  #placeText(x, y) {
    this.#text.style.left = `${Math.round(x)}px`;
    this.#text.style.top = `${Math.round(y)}px`;
  }

  #toRemote(x, y) {
    const v = this.#video;
    return remotePoint(x, y, v.getBoundingClientRect(), v.videoWidth, v.videoHeight);
  }

  #onPointer(e, what) {
    if (!this.#active()) return;
    if (e.pointerType === 'touch') {
      e.preventDefault();
      this.#onTouch(e, what);
    } else if (what === 'move') {
      this.#onPointerMove(e);
    } else {
      this.#onPointerButton(e, what === 'down');
    }
  }

  #onPointerMove(e) {
    this.#queueMove(e.clientX, e.clientY);
  }

  #queueMove(x, y) {
    const p = this.#toRemote(x, y);
    if (!p || (!p.inside && !this.#buttons)) return;
    this.#move = { t: 'move', x: p.x, y: p.y };
    this.#moveFrame ||= requestAnimationFrame(() => this.#flushMove());
  }

  #flushMove() {
    this.#moveFrame = 0;
    if (!this.#move) return;
    this.#send(this.#move);
    this.#move = null;
  }

  #onPointerButton(e, down) {
    e.preventDefault();
    if (down) this.#placeText(e.clientX, e.clientY);
    if (this.#button(e.clientX, e.clientY, e.button, down) && down) this.#video.setPointerCapture(e.pointerId);
  }

  // button presses or releases a button at a page point, and reports
  // whether it did. A press outside the picture does nothing, and so does
  // a release of a button not pressed.
  #button(x, y, button, down) {
    const p = this.#toRemote(x, y);
    if (!p) return false;
    const bit = 1 << button;
    if (down) {
      if (!p.inside) return false;
      this.#buttons |= bit;
    } else {
      if (!(this.#buttons & bit)) return false;
      this.#buttons &= ~bit;
    }
    this.#move = null;
    this.#send({ t: 'move', x: p.x, y: p.y });
    this.#send({ t: 'button', x: p.x, y: p.y, button, down });
    return true;
  }

  #onTouch(e, what) {
    const g = this.#gestures;
    switch (what) {
      case 'down':
        this.#placeText(e.clientX, e.clientY);
        g.down(e.pointerId, e.clientX, e.clientY);
        break;
      case 'move':
        g.move(e.pointerId, e.clientX, e.clientY);
        break;
      case 'up':
        g.up(e.pointerId, e.clientX, e.clientY);
        break;
      default:
        g.cancel(e.pointerId);
    }
  }

  #onGesture(g) {
    switch (g.t) {
      case 'move':
        this.#queueMove(g.x, g.y);
        break;
      case 'button':
        this.#button(g.x, g.y, g.button, g.down);
        break;
      case 'wheel': {
        const p = this.#toRemote(g.x, g.y);
        if (!p?.inside) return;
        this.#move = null;
        this.#send({ t: 'move', x: p.x, y: p.y });
        this.#send({ t: 'wheel', dx: g.dx, dy: g.dy });
        break;
      }
      case 'pinch':
        this.#view.pinch(g.from, g.to);
        this.#showView();
    }
  }

  #showView() {
    this.#video.style.transform = this.#view.transform;
  }

  #onWheel(e) {
    if (!this.#active()) return;
    e.preventDefault();
    this.#flushMove();
    const k = e.deltaMode === WheelEvent.DOM_DELTA_LINE ? LINE_PX
      : e.deltaMode === WheelEvent.DOM_DELTA_PAGE ? window.innerHeight
        : 1;
    this.#send({ t: 'wheel', dx: e.deltaX * k, dy: e.deltaY * k });
  }

  #onKey(e, down) {
    if (!this.#active()) return;
    // Keys an input method or a soft keyboard is typing with are the text
    // box's; what they type arrives as text. Only the release of a key the
    // remote saw pressed still goes out.
    if (imeOwns(e) && (down || !this.#held.has(e.code))) return;
    // A page control that takes keys, such as the monitor menu, keeps them
    // in the same way.
    const local = e.target instanceof Element && e.target.closest('[data-keys="local"]');
    if (local && (down || !this.#held.has(e.code))) return;
    let { key, code } = e;
    const wasMeta = key === 'Meta' || code === 'MetaLeft' || code === 'MetaRight';
    if (isMac && wasMeta) {
      key = 'Control';
      code = code === 'MetaRight' ? 'ControlRight' : 'ControlLeft';
    }
    const msg = { t: 'key', key, code, down, repeat: e.repeat };

    // Let the browser run its paste shortcut so the paste event carries the
    // local clipboard; the key itself goes out once the clipboard has.
    if (down && !this.#pasteQueue && isPasteShortcut(e)) {
      this.#pasteQueue = [msg];
      this.#pasteTimer = setTimeout(() => this.#flushPaste(), PASTE_WAIT_MS);
      this.#held.set(code, key);
      return;
    }
    // Sent as a key, so the text box must not type it as well.
    e.preventDefault();

    if (down) this.#held.set(code, key);
    else this.#held.delete(code);
    this.#key(msg);

    // macOS swallows key-ups for keys pressed while Cmd is down, so let go of
    // them when Cmd itself goes up.
    if (isMac && wasMeta && !down) {
      for (const [c, k] of this.#held) {
        if (MODIFIERS.has(k)) continue;
        this.#held.delete(c);
        this.#key({ t: 'key', key: k, code: c, down: false, repeat: false });
      }
    }
  }

  #key(msg) {
    if (this.#pasteQueue) this.#pasteQueue.push(msg);
    else this.#send(msg);
  }

  #showPreedit(text) {
    this.#preedit.textContent = text ?? '';
    this.#preedit.hidden = !text;
  }

  #onCompositionEnd(e) {
    this.#showPreedit('');
    this.#text.value = '';
    if (!this.#active()) return;
    for (const m of fromComposition(e.data)) this.#key(m);
  }

  // onBeforeInput sends what a soft keyboard typed without naming a key.
  // Text an input method is composing waits for compositionend.
  #onBeforeInput(e) {
    if (e.isComposing || e.inputType === 'insertCompositionText') return;
    e.preventDefault();
    if (!this.#active()) return;
    for (const m of fromInput(e.inputType, e.data)) this.#key(m);
  }

  #onPaste(e) {
    if (!this.#active()) return;
    e.preventDefault();
    clearTimeout(this.#pasteTimer);
    let held;
    const hold = new Promise((resolve) => { held = setTimeout(resolve, PASTE_HOLD_MS); });
    Promise.race([Promise.resolve(this.#paste(e.clipboardData)), hold])
      .catch((err) => console.warn('robin: paste', err))
      .finally(() => {
        clearTimeout(held);
        this.#flushPaste();
      });
  }

  #flushPaste() {
    const queued = this.#pasteQueue ?? [];
    this.#cancelPaste();
    for (const m of queued) this.#send(m);
  }

  #cancelPaste() {
    clearTimeout(this.#pasteTimer);
    this.#pasteQueue = null;
  }
}

function isPasteShortcut(e) {
  const primary = isMac ? e.metaKey : e.ctrlKey;
  return primary && !e.altKey && !e.repeat && (e.code === 'KeyV' || e.key.toLowerCase() === 'v');
}
