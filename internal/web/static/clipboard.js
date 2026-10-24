// The clipboard both ways: what is pasted into the page goes to the host's
// clipboard, and what is copied on the host comes to the browser's. Plain
// text that fits travels as a control message, everything else, HTML and
// images included, as a transfer of its own.

import { registerKind } from './bulkframe.js';
import { onBulk } from './bulk.js';
import { onControl } from './control.js';
import { CLIP_KIND, CLIP_LIMIT, MAX_CLIP, clipSize, decodeClip, encodeClip, fitsControl, splitPaste } from './clip.js';

registerKind(CLIP_KIND, CLIP_LIMIT);

export class Clipboard {
  // last is what the host copied most recently: { text, html, png }.
  last = null;
  #send;
  #bulk;
  #propose;

  // send delivers a control message, bulk(header, blob) sends a transfer
  // as sendBulk does, and propose takes files pasted into the page, which
  // must not leave it before the user asks.
  constructor({ send, bulk, propose }) {
    this.#send = send;
    this.#bulk = bulk;
    this.#propose = propose;
    onControl('clipboard', (c) => this.#copied({ text: c.text ?? '' }));
    onBulk(CLIP_KIND, (h) => {
      const bytes = new Uint8Array(h.size ?? 0);
      let at = 0;
      return {
        write: (chunk) => {
          bytes.set(chunk, at);
          at += chunk.length;
        },
        commit: () => this.#copied(decodeClip(bytes)),
        abort: () => {},
      };
    });
  }

  // paste takes what a paste event carries to the host, and resolves once
  // the host's clipboard holds it. Files in it other than one image wait
  // to be uploaded.
  async paste(data) {
    const { clip, image, uploads } = splitPaste({
      text: data?.getData('text/plain') ?? '',
      html: data?.getData('text/html') ?? '',
      files: [...(data?.files ?? [])],
    });
    if (uploads.length) this.#propose(uploads);
    if (image) await this.put({ ...clip, png: new Uint8Array(await image.arrayBuffer()) });
    else if (clip) await this.put(clip);
  }

  // put puts clip on the host's clipboard. An image too big to go is left
  // out, and the text with it still goes.
  async put(clip) {
    if (clipSize(clip) > MAX_CLIP) {
      console.warn('robin: clipboard content too large for the host', clipSize(clip));
      clip = { text: clip.text, html: clip.html };
      if (clipSize(clip) > MAX_CLIP) return;
    }
    if (fitsControl(clip)) {
      this.#send({ t: 'clipboard', text: clip.text ?? '' });
      return;
    }
    await this.#bulk({ kind: CLIP_KIND }, new Blob([encodeClip(clip)]));
  }

  // copied keeps what the host copied and offers it to the browser's
  // clipboard, which takes it only while the page has focus.
  #copied(clip) {
    this.last = clip;
    const clipboard = navigator.clipboard;
    if (!clipboard) return;
    const fallback = () => {
      if (clip.text) clipboard.writeText(clip.text).catch(() => {});
    };
    if (clip.html || clip.png) {
      if (typeof ClipboardItem !== 'function') {
        fallback();
        return;
      }
      const items = {};
      if (clip.text) items['text/plain'] = new Blob([clip.text], { type: 'text/plain' });
      if (clip.html) items['text/html'] = new Blob([clip.html], { type: 'text/html' });
      if (clip.png) items['image/png'] = new Blob([clip.png], { type: 'image/png' });
      clipboard.write([new ClipboardItem(items)]).catch(fallback);
      return;
    }
    fallback();
  }
}
