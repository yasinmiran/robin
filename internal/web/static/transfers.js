// The files moving between the page and the host, as the status tab lists
// them: uploads the user started and downloads the host offered. This is
// the bookkeeping only; files.js draws it and moves the bytes. Nothing
// here touches the DOM.

export const FILE_KIND = 'file';
// MAX_FILE bounds one file either way, as protocol.MaxFile does. A download
// is held in memory until it is whole.
export const MAX_FILE = 4 * 1024 ** 3;

// formatSize writes bytes in the binary units the agent uses.
export function formatSize(n) {
  if (n < 1024) return `${n} B`;
  let v = n;
  let i = -1;
  while (v >= 1024 && i < 3) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${'KMGT'[i]}iB`;
}

// The states of a transfer.
//   upload:   queued -> sending -> kept | failed
//   download: offered -> fetching -> saved, or back to offered with an error
export const QUEUED = 'queued';
export const SENDING = 'sending';
export const KEPT = 'kept';
export const FAILED = 'failed';
export const OFFERED = 'offered';
export const FETCHING = 'fetching';
export const SAVED = 'saved';

// Transfers holds every transfer the tab shows, in the order they came.
// Each change calls onChange.
export class Transfers {
  #list = new Map();
  #onChange;

  constructor(onChange = () => {}) {
    this.#onChange = onChange;
  }

  get(id) {
    return this.#list.get(id);
  }

  // list is every transfer, oldest first.
  list() {
    return [...this.#list.values()];
  }

  // waiting reports whether something needs the user or is under way, for
  // keeping the tab out.
  waiting() {
    return this.list().some((t) => [QUEUED, SENDING, OFFERED, FETCHING].includes(t.state));
  }

  #set(id, fields) {
    const t = this.#list.get(id);
    if (!t) return false;
    Object.assign(t, fields);
    this.#onChange();
    return true;
  }

  // upload adds a file the user is sending, waiting its turn.
  upload(id, name, size) {
    this.#list.set(id, { id, dir: 'up', name, size, done: 0, state: QUEUED, saved: '', error: '' });
    this.#onChange();
  }

  // offer adds a file the host offers, or leaves one it already offered as
  // it is: a page that reconnects hears every open offer again.
  offer(id, name, size) {
    if (this.#list.has(id)) return;
    this.#list.set(id, { id, dir: 'down', name, size, done: 0, state: OFFERED, saved: '', error: '' });
    this.#onChange();
  }

  sending(id) {
    return this.#set(id, { state: SENDING, done: 0, error: '' });
  }

  // fetch marks an offer the user asked for, unless it is not on offer.
  fetch(id) {
    const t = this.get(id);
    if (t?.state !== OFFERED) return false;
    return this.#set(id, { state: FETCHING, done: 0, error: '' });
  }

  fetching(id) {
    return this.get(id)?.state === FETCHING;
  }

  progress(id, done) {
    return this.#set(id, { done });
  }

  // saved records the name the host kept an upload under, which can come
  // before or after the upload's own verdict.
  saved(id, name) {
    return this.#set(id, { saved: name });
  }

  // finished ends a transfer that worked.
  finished(id) {
    const t = this.get(id);
    if (!t) return false;
    return this.#set(id, { state: t.dir === 'up' ? KEPT : SAVED, done: t.size });
  }

  // failed ends an upload, or puts a download back on offer, with why.
  failed(id, error) {
    const t = this.get(id);
    if (!t) return false;
    return this.#set(id, { state: t.dir === 'up' ? FAILED : OFFERED, error: String(error), done: 0 });
  }

  remove(id) {
    if (this.#list.delete(id)) this.#onChange();
  }
}

// statusOf is the line a transfer shows beside its name.
export function statusOf(t) {
  const percent = t.size ? ` ${Math.floor((t.done / t.size) * 100)}%` : '';
  switch (t.state) {
    case QUEUED:
      return 'Waiting to upload';
    case SENDING:
      return `Uploading${percent}`;
    case KEPT:
      return t.saved && t.saved !== t.name ? `Uploaded as ${t.saved}` : 'Uploaded';
    case FAILED:
      return `Upload failed: ${t.error}`;
    case OFFERED:
      return t.error ? `Download failed: ${t.error}` : 'Offered by the host';
    case FETCHING:
      return `Downloading${percent}`;
    case SAVED:
      return 'Downloaded';
  }
  return t.state;
}
