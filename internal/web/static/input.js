// Turns local pointer, keyboard, clipboard and viewport events into Control
// messages for the remote desktop.

const LINE_PX = 40;
const PASTE_WAIT_MS = 100;
const RESIZE_DEBOUNCE_MS = 250;
const MODIFIERS = new Set(['Shift', 'Control', 'Alt', 'AltGraph', 'Meta', 'OS']);

export const isMac = /Mac|iPhone|iPad/.test(navigator.userAgentData?.platform ?? navigator.platform);

export class Input {
  #video;
  #send;
  #active;
  #held = new Map(); // code -> key, as sent to the remote
  #buttons = 0;
  #move = null;
  #moveFrame = 0;
  #pasteQueue = null; // key messages held back while waiting for a paste event
  #pasteTimer = 0;
  #resizeTimer = 0;

  // send delivers one Control message; active reports whether input should
  // be captured at all, so a closed session leaves the browser's own keys
  // (reload, close tab) alone.
  constructor(video, send, active) {
    this.#video = video;
    this.#send = send;
    this.#active = active;
  }

  attach() {
    const v = this.#video;
    v.addEventListener('pointermove', (e) => this.#onPointerMove(e));
    v.addEventListener('pointerdown', (e) => this.#onPointerButton(e, true));
    v.addEventListener('pointerup', (e) => this.#onPointerButton(e, false));
    v.addEventListener('pointercancel', (e) => this.#onPointerButton(e, false));
    v.addEventListener('mousedown', (e) => e.preventDefault()); // no middle-click autoscroll
    v.addEventListener('wheel', (e) => this.#onWheel(e), { passive: false });
    window.addEventListener('contextmenu', (e) => e.preventDefault());

    window.addEventListener('keydown', (e) => this.#onKey(e, true));
    window.addEventListener('keyup', (e) => this.#onKey(e, false));
    window.addEventListener('paste', (e) => this.#onPaste(e));

    window.addEventListener('blur', () => this.release());
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'hidden') this.release();
    });

    new ResizeObserver(() => {
      clearTimeout(this.#resizeTimer);
      this.#resizeTimer = setTimeout(() => this.sendResize(), RESIZE_DEBOUNCE_MS);
    }).observe(document.documentElement);
  }

  sendResize() {
    this.#send({ t: 'resize', w: Math.round(window.innerWidth), h: Math.round(window.innerHeight) });
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

  // toRemote maps a pointer position to remote pixels. object-fit: contain
  // letterboxes the video, so the picture is scaled by the tighter axis and
  // centred in the element box.
  #toRemote(e) {
    const v = this.#video;
    const vw = v.videoWidth;
    const vh = v.videoHeight;
    if (!vw || !vh) return null;
    const box = v.getBoundingClientRect();
    const scale = Math.min(box.width / vw, box.height / vh);
    const x = (e.clientX - box.left - (box.width - vw * scale) / 2) / scale;
    const y = (e.clientY - box.top - (box.height - vh * scale) / 2) / scale;
    return {
      x: clamp(Math.floor(x), 0, vw - 1),
      y: clamp(Math.floor(y), 0, vh - 1),
      inside: x >= 0 && y >= 0 && x < vw && y < vh,
    };
  }

  #onPointerMove(e) {
    if (!this.#active()) return;
    const p = this.#toRemote(e);
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
    if (!this.#active()) return;
    e.preventDefault();
    const p = this.#toRemote(e);
    if (!p) return;
    const bit = 1 << e.button;
    if (down) {
      if (!p.inside) return;
      this.#video.setPointerCapture(e.pointerId);
      this.#buttons |= bit;
    } else {
      if (!(this.#buttons & bit)) return;
      this.#buttons &= ~bit;
    }
    this.#move = null;
    this.#send({ t: 'move', x: p.x, y: p.y });
    this.#send({ t: 'button', x: p.x, y: p.y, button: e.button, down });
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
    if (!this.#active() || e.isComposing) return;
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

  #onPaste(e) {
    if (!this.#active()) return;
    e.preventDefault();
    const text = e.clipboardData?.getData('text/plain');
    if (text) this.sendClipboard(text);
    this.#flushPaste();
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

function clamp(n, lo, hi) {
  return Math.min(Math.max(n, lo), hi);
}
