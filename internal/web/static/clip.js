// Clipboard content in the formats the page and the agent trade, as
// internal/protocol/clipboard.go lays them out: plain text, HTML and a PNG
// image, the three types navigator.clipboard.write takes. Nothing here
// touches the DOM.

export const CLIP_KIND = 'clipboard';
// MAX_CLIP bounds the data of one clipboard content, every format together.
export const MAX_CLIP = 16 * 1024 * 1024;
// CLIP_LIMIT is the most a clipboard transfer carries: the data and the
// framing of its parts.
export const CLIP_LIMIT = MAX_CLIP + 64;
// CLIP_INBOUND is how many clipboard transfers the agent may have open to
// the page at once, as many as it takes from the page (maxClipsIn): a
// newer copy cancels the transfer of an older one, whose close may reach
// the page after the newer one's header.
export const CLIP_INBOUND = 2;
// CONTROL_TEXT is the longest plain text sent as a control message rather
// than a transfer, as the agent's clipboardSize.
export const CONTROL_TEXT = 256 * 1024 - 1024;

export const TEXT = 'text/plain';
export const HTML = 'text/html';
export const PNG = 'image/png';
const ORDER = [TEXT, HTML, PNG];

const utf8 = new TextEncoder();

// clipSize is the bytes of data clip holds: { text, html, png }, text and
// html as strings and png as a Uint8Array, each optional.
export function clipSize(clip) {
  return utf8.encode(clip.text ?? '').length + utf8.encode(clip.html ?? '').length + (clip.png?.length ?? 0);
}

// fitsControl reports whether clip can go as a control message: plain text
// and nothing else, short enough.
export function fitsControl(clip) {
  return !clip.html && !clip.png?.length && utf8.encode(clip.text ?? '').length <= CONTROL_TEXT;
}

// encodeClip lays clip out for a transfer: per format it holds, one byte
// of MIME type length, the type, four bytes of big-endian data length and
// the data.
export function encodeClip(clip) {
  const parts = [];
  for (const [mime, data] of [[TEXT, utf8.encode(clip.text ?? '')], [HTML, utf8.encode(clip.html ?? '')], [PNG, clip.png ?? new Uint8Array()]]) {
    if (data.length) parts.push([utf8.encode(mime), data]);
  }
  const total = parts.reduce((n, [, d]) => n + d.length, 0);
  if (total > MAX_CLIP) throw new Error(`clipboard content of ${total} bytes, the limit is ${MAX_CLIP}`);
  const out = new Uint8Array(parts.reduce((n, [m, d]) => n + 1 + m.length + 4 + d.length, 0));
  const view = new DataView(out.buffer);
  let at = 0;
  for (const [mime, data] of parts) {
    out[at] = mime.length;
    out.set(mime, at + 1);
    view.setUint32(at + 1 + mime.length, data.length);
    out.set(data, at + 5 + mime.length);
    at += 5 + mime.length + data.length;
  }
  return out;
}

// decodeClip reads a transfer's payload, skipping formats it does not
// know. It throws on a format given twice, text that is not UTF-8, data
// past the end and more than MAX_CLIP of data.
export function decodeClip(bytes) {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const text = new TextDecoder('utf-8', { fatal: true });
  const clip = {};
  const seen = new Set();
  let total = 0;
  for (let at = 0; at < bytes.length;) {
    const n = bytes[at];
    if (bytes.length < at + 1 + n + 4) throw new Error('malformed clipboard payload: part header runs past the end');
    const mime = new TextDecoder().decode(bytes.subarray(at + 1, at + 1 + n));
    const size = view.getUint32(at + 1 + n);
    at += 5 + n;
    if (size > bytes.length - at) throw new Error(`malformed clipboard payload: ${mime} data runs past the end`);
    const data = bytes.subarray(at, at + size);
    at += size;
    if (seen.has(mime)) throw new Error(`malformed clipboard payload: ${mime} given twice`);
    seen.add(mime);
    total += size;
    if (total > MAX_CLIP) throw new Error(`malformed clipboard payload: more than ${MAX_CLIP} bytes`);
    try {
      if (mime === TEXT) clip.text = text.decode(data);
      else if (mime === HTML) clip.html = text.decode(data);
    } catch {
      throw new Error(`malformed clipboard payload: ${mime} is not UTF-8`);
    }
    if (mime === PNG) clip.png = data.slice();
  }
  return clip;
}

// clipTypes lists the MIME types clip holds, in the order of ORDER.
export function clipTypes(clip) {
  return ORDER.filter((m) => (m === TEXT ? clip.text : m === HTML ? clip.html : clip.png?.length));
}

// splitPaste decides what a paste in the page becomes, from its text, its
// HTML and the files it carries ({ name, type }). One PNG image and
// nothing but text beside it is clipboard content: that is how a browser
// pastes an image copied from a page or a screenshot tool. Any other file
// is an upload, since a browser's clipboard cannot hand a file on as a
// file. Returns { clip, image, uploads }: clip the text and HTML, null
// when there is neither, and image the PNG file to go beside it, or null.
export function splitPaste({ text = '', html = '', files = [] }) {
  const image = files.length === 1 && files[0].type === PNG ? files[0] : null;
  const uploads = image ? [] : [...files];
  // Text beside uploads is the files' names, which no one wants pasted.
  if (uploads.length) return { clip: null, image: null, uploads };
  const clip = {};
  if (text) clip.text = text;
  if (html) clip.html = html;
  return { clip: Object.keys(clip).length ? clip : null, image, uploads };
}
