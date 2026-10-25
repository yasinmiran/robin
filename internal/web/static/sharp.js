// Sharp text and actual size, the picture's two switches in the status tab.
// Sharp text draws the agent's lossless tiles of the still parts of the
// screen on a canvas laid over the video, at the desktop's own resolution;
// tiles.js decides what may show. Actual size lays the picture out each of
// its pixels a whole number of device pixels and pans it, with the pointer
// at the window's edge or two fingers, when it is larger than the window.

import { onBulk } from './bulk.js';
import { registerKind } from './bulkframe.js';
import { onControl } from './control.js';
import { touchFirst } from './device.js';
import { switchButton, switchNote } from './switches.js';
import { TILE_BATCH_LIMIT, TileBook, atlasSize, parseTileBatch } from './tiles.js';
import { actualBox, edgePan } from './actualsize.js';

const KIND = 'tile';
// MAX_OPEN bounds the tile transfers open at once. The agent keeps two
// under way; more than a few means something else is sending them. A
// transfer's place lasts until its verdict, which waits for the atlas to
// decode, so this bounds the decodes under way as well as the bytes held.
const MAX_OPEN = 4;
// TRACE_FRAMES bounds what a trace keeps, about a minute of video.
const TRACE_FRAMES = 4096;

const NO_FRAME_CLOCK = 'Not available in this browser: it does not report which video frame is on screen. Chrome, Edge and Firefox do.';
const PAN_POINTER = 'Hold the pointer at an edge of the window to see the rest';
const PAN_TOUCH = 'Drag with two fingers to see the rest';
// The notes' ids, which the switches name in aria-describedby.
const SHARP_NOTE = 'sharp-note';
const ACTUAL_NOTE = 'actual-note';

registerKind(KIND, TILE_BATCH_LIMIT);

export class SharpText {
  #video;
  #input;
  #send;
  #canvas;
  #ctx;
  #book = new TileBook();
  #section;
  #want = true; // the switch
  #asked = false; // what the agent was last asked for
  #open = false; // the control channel
  #answer = null; // the agent's protocol.Tiles to the latest request
  #clock = null; // whether the browser reports frames' RTP timestamps
  #warned = false;
  #epoch = 0; // counts resets, so a batch decoded across one is dropped
  #bytes = 0;
  #batches = 0;
  #actual = false;
  #overflow = false; // the actual-size picture is larger than the window
  #pan = { x: 0, y: 0 };
  #pointer = null;
  #panning = 0;
  #trace = null;

  // send delivers a control message to the agent; input is the page's
  // Input, whose zoom actual size replaces.
  constructor({ video, overlay, send, input }) {
    this.#video = video;
    this.#input = input;
    this.#send = send;
    this.#canvas = document.createElement('canvas');
    this.#canvas.id = 'tiles';
    this.#canvas.setAttribute('aria-hidden', 'true');
    this.#canvas.width = this.#canvas.height = 0;
    this.#canvas.style.visibility = 'hidden';
    video.after(this.#canvas);
    this.#ctx = this.#canvas.getContext('2d');
    this.#section = overlay.addSection('picture');

    onControl('frame', (c) => this.#manifest(c.frame));
    onControl('tiles', (c) => {
      const answer = c.tiles ?? null;
      // An answer the other way is to a request since overtaken.
      if (Boolean(answer?.on || answer?.unsupported) !== this.#asked) return;
      this.#answer = answer;
      this.#render();
    });
    onBulk(KIND, (h) => this.#accept(h), { inbound: MAX_OPEN });

    input.onPinch = (g) => this.#pinch(g);
    window.addEventListener('resize', () => this.#layout());
    video.addEventListener('resize', () => this.#layout());
    new MutationObserver(() => this.#place()).observe(video, { attributes: true, attributeFilter: ['style', 'class'] });
    window.addEventListener('pointermove', (e) => this.#onPointer(e), { passive: true });
    document.documentElement.addEventListener('pointerleave', () => { this.#pointer = null; });

    if (typeof video.requestVideoFrameCallback === 'function') {
      video.requestVideoFrameCallback((now, meta) => this.#onFrame(meta));
    } else {
      this.#clock = false;
    }
    this.#render();
  }

  // state is what the switches and the canvas show, for the status tab's
  // tests: the switch, whether the agent answered the latest request and
  // what, whether tiles may be seen, how many are drawn, and what came in.
  get state() {
    return {
      want: this.#want,
      answered: this.#answer !== null,
      on: this.#on,
      unsupported: this.#unsupported,
      visible: this.#canvas.style.visibility !== 'hidden',
      drawn: this.#book.drawn,
      grid: this.#book.gen === null ? null : { gen: this.#book.gen, w: this.#book.width, h: this.#book.height },
      bytes: this.#bytes,
      batches: this.#batches,
      actual: this.#actual,
    };
  }

  // trace starts keeping, for every frame shown, when it was, its RTP
  // timestamp, whether tiles could be seen and which were drawn, with the
  // batch each came from; and every manifest, with when it arrived.
  // stopTrace returns what was kept.
  trace() {
    this.#trace = { frames: [], manifests: [] };
  }

  stopTrace() {
    const t = this.#trace;
    this.#trace = null;
    return t;
  }

  // toggle flips a switch, 'sharp' or 'actual', as a click would.
  toggle(key) {
    if (key === 'sharp') this.#setWant(!this.#want);
    else if (key === 'actual') this.#setActual(!this.#actual);
  }

  // open is the control channel opening: a new agent peer, which has to be
  // asked for tiles again.
  open() {
    this.#open = true;
    this.#asked = false;
    this.#sync();
  }

  // reset forgets the peer: its tiles and its answer go.
  reset() {
    this.#open = false;
    this.#asked = false;
    this.#answer = null;
    this.#clear();
    this.#render();
  }

  get #on() {
    return Boolean(this.#answer?.on) && this.#asked;
  }

  get #unsupported() {
    if (this.#clock === false) return NO_FRAME_CLOCK;
    return this.#answer?.unsupported ?? '';
  }

  #sync() {
    const want = this.#want && this.#clock === true && this.#open;
    if (want === this.#asked) return;
    this.#asked = want;
    this.#answer = null;
    this.#send({ t: 'tiles', on: want });
    if (!want) this.#clear();
    this.#render();
  }

  #setWant(on) {
    this.#want = on;
    this.#sync();
    this.#render();
  }

  #clear() {
    this.#epoch++;
    this.#book.reset();
    this.#canvas.width = this.#canvas.height = 0;
    this.#show();
    if (this.#actual) this.#layout();
  }

  // accept takes a tile transfer.
  #accept(h) {
    if (!this.#asked) throw new Error('sharp text is off');
    const parts = [];
    return {
      write: (chunk) => { parts.push(chunk); },
      commit: async () => {
        const bytes = new Uint8Array(h.size ?? 0);
        let at = 0;
        for (const p of parts) {
          bytes.set(p, at);
          at += p.length;
        }
        this.#bytes += bytes.length;
        this.#batches++;
        await this.#take(parseTileBatch(bytes));
      },
      abort: () => {},
    };
  }

  async #take(batch) {
    const epoch = this.#epoch;
    const image = await createImageBitmap(new Blob([batch.png], { type: 'image/png' }));
    let due;
    try {
      // A decoder that ignored the PNG's header must not slip a picture of
      // another size through.
      const { width, height } = atlasSize(batch.tiles.length);
      if (image.width !== width || image.height !== height) {
        throw new Error(`malformed tile batch: a ${image.width}x${image.height} atlas of ${batch.tiles.length} tiles`);
      }
      if (!this.#asked || epoch !== this.#epoch) {
        image.close();
        return;
      }
      due = this.#book.batch({ ...batch, image });
    } catch (err) {
      image.close();
      this.#refuse(err);
      throw err;
    }
    this.#draw(due);
    this.#show();
  }

  // refuse drops every tile for a manifest or batch outside the bounds
  // protocol.Frame states: a broken or hostile agent costs blur, never a
  // hang.
  #refuse(err) {
    if (!this.#warned) console.warn('robin: sharp text:', err.message);
    this.#warned = true;
    this.#clear();
  }

  #manifest(frame) {
    if (!this.#asked) return;
    let reset;
    let clear;
    try {
      ({ reset, clear } = this.#book.manifest(frame));
    } catch (err) {
      this.#refuse(err);
      return;
    }
    if (reset) {
      this.#canvas.width = frame.w;
      this.#canvas.height = frame.h;
      if (this.#actual) this.#layout();
      this.#place();
    }
    for (const i of clear) {
      const r = this.#book.rect(i);
      this.#ctx.clearRect(r.x, r.y, r.w, r.h);
    }
    this.#trace?.manifests.push({ at: performance.now(), rtp: frame.rtp, gen: frame.gen, gone: frame.gone ?? [] });
    this.#show();
  }

  #onFrame(meta) {
    const rtp = meta?.rtpTimestamp;
    if (typeof rtp !== 'number') {
      this.#clock = false;
      this.#sync();
      this.#render();
      return;
    }
    if (this.#clock === null) {
      this.#clock = true;
      this.#sync();
      this.#render();
    }
    this.#video.requestVideoFrameCallback((now, m) => this.#onFrame(m));
    if (!this.#asked) return;
    this.#draw(this.#book.frame(rtp));
    this.#place();
    this.#show();
    const t = this.#trace;
    if (t && t.frames.length < TRACE_FRAMES) {
      t.frames.push({ at: performance.now(), rtp, gen: this.#book.gen, visible: this.#book.visible, drawn: this.#book.entries() });
    }
  }

  #draw({ draws, spent }) {
    for (const d of draws) this.#ctx.drawImage(d.image, d.sx, d.sy, d.w, d.h, d.x, d.y, d.w, d.h);
    for (const image of spent) image.close();
  }

  #show() {
    this.#canvas.style.visibility = this.#asked && this.#book.visible ? '' : 'hidden';
  }

  // place lays the canvas over the picture the video shows, letterbox and
  // zoom included.
  #place() {
    const v = this.#video;
    const vw = v.videoWidth;
    const vh = v.videoHeight;
    if (!vw || !vh || !this.#canvas.width) return;
    const box = v.getBoundingClientRect();
    const scale = Math.min(box.width / vw, box.height / vh);
    const s = this.#canvas.style;
    s.left = `${box.left + (box.width - vw * scale) / 2}px`;
    s.top = `${box.top + (box.height - vh * scale) / 2}px`;
    s.width = `${vw * scale}px`;
    s.height = `${vh * scale}px`;
  }

  #setActual(on) {
    this.#actual = on;
    this.#pan = { x: 0, y: 0 };
    this.#input.view.reset();
    this.#video.style.transform = '';
    this.#video.classList.toggle('actual', on);
    this.#canvas.classList.toggle('actual', on);
    this.#layout();
    this.#render();
  }

  // layout sizes the video for actual size: the desktop's own size when
  // tiles carry it, the video's otherwise. A video at that size scales by
  // whole device pixels, so it is drawn without smoothing, as the tiles are.
  #layout() {
    const v = this.#video;
    if (!this.#actual) {
      for (const p of ['left', 'top', 'width', 'height']) v.style.removeProperty(`--actual-${p}`);
      v.classList.remove('whole');
      this.#setOverflow(false);
      this.#place();
      return;
    }
    const pw = this.#book.width || v.videoWidth;
    const ph = this.#book.height || v.videoHeight;
    if (!pw || !ph) return;
    const box = actualBox(pw, ph, window.innerWidth, window.innerHeight, window.devicePixelRatio || 1, this.#pan);
    this.#pan = box.pan;
    for (const p of ['left', 'top', 'width', 'height']) v.style.setProperty(`--actual-${p}`, `${box[p]}px`);
    v.classList.toggle('whole', v.videoWidth === pw && v.videoHeight === ph);
    this.#setOverflow(box.overflow);
    this.#place();
  }

  // setOverflow says how to pan while the picture is larger than the window.
  #setOverflow(on) {
    if (on === this.#overflow) return;
    this.#overflow = on;
    this.#render();
  }

  #pinch(g) {
    if (!this.#actual) return false;
    this.#panBy(g.to.cx - g.from.cx, g.to.cy - g.from.cy);
    return true;
  }

  #panBy(dx, dy) {
    const before = this.#pan;
    this.#pan = { x: before.x + dx, y: before.y + dy };
    this.#layout();
    return this.#pan.x !== before.x || this.#pan.y !== before.y;
  }

  #onPointer(e) {
    if (e.pointerType === 'touch') return;
    // The status tab hangs over the top band: reaching for it must not pan.
    if (e.target?.closest?.('#hud')) {
      this.#pointer = null;
      return;
    }
    this.#pointer = { x: e.clientX, y: e.clientY };
    if (this.#actual && !this.#panning) this.#panning = requestAnimationFrame(() => this.#edgeStep());
  }

  #edgeStep() {
    this.#panning = 0;
    const p = this.#pointer;
    if (!this.#actual || !p) return;
    const { dx, dy } = edgePan(p.x, p.y, window.innerWidth, window.innerHeight);
    if ((dx || dy) && this.#panBy(dx, dy)) this.#panning = requestAnimationFrame(() => this.#edgeStep());
  }

  #render() {
    const unsupported = this.#unsupported;
    const sharp = {
      key: 'sharp',
      label: 'Sharp text',
      on: this.#want && !unsupported,
      busy: this.#want && this.#asked && !this.#answer && !unsupported,
      disabled: Boolean(unsupported),
      describedBy: unsupported ? SHARP_NOTE : '',
      title: unsupported ? '' : (this.#want
        ? 'Still parts of the screen are sent without loss and drawn over the video'
        : 'Send still parts of the screen without loss, so text is as sharp as on the host'),
    };
    const pan = this.#actual && this.#overflow ? (touchFirst ? PAN_TOUCH : PAN_POINTER) : '';
    const actual = {
      key: 'actual',
      label: 'Actual size',
      on: this.#actual,
      busy: false,
      describedBy: pan ? ACTUAL_NOTE : '',
      title: this.#actual
        ? 'Each pixel of the desktop is a whole number of pixels of this screen'
        : 'Show each pixel of the desktop as a whole number of pixels of this screen instead of fitting the window',
    };
    const row = document.createElement('div');
    row.className = 'switches';
    const toggle = (key) => this.toggle(key);
    row.append(switchButton(sharp, toggle), switchButton(actual, toggle));
    const notes = [];
    if (unsupported) notes.push(switchNote(SHARP_NOTE, unsupported));
    if (pan) notes.push(switchNote(ACTUAL_NOTE, pan));
    this.#section.replaceChildren(row, ...notes);
  }
}
