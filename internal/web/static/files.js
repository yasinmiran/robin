// Files between the page and the host, in a section of the status tab.
// Uploads come from its button, a drop anywhere on the page or a paste, and
// land in the host's download directory. Downloads are offered by the host
// (robin-send there) and fetched only when the user clicks Save, so the
// host cannot push a file into the browser unasked.

import { registerKind } from './bulkframe.js';
import { onBulk } from './bulk.js';
import { onControl } from './control.js';
import {
  FAILED, FETCHING, FILE_KIND, KEPT, MAX_FILE, OFFERED, QUEUED, SAVED, SENDING, Transfers, formatSize, statusOf,
} from './transfers.js';

// How long a finished transfer stays listed.
const CLEAR_AFTER_MS = 8000;
// A download gathers in blobs of this size, so the browser can keep it
// out of the page's own memory.
const PIECE_BYTES = 16 * 1024 * 1024;
// Revoking a download's URL at once can cancel the download itself.
const REVOKE_AFTER_MS = 60_000;

registerKind(FILE_KIND, MAX_FILE);

export class Files {
  #transfers = new Transfers(() => this.#changed());
  #overlay;
  #send;
  #bulk;
  #section;
  #list;
  #picker;
  #queue = [];
  #pumping = false;
  #aborts = new Map();
  #uploads = 0;
  #frame = 0;
  #dropTimer = 0;

  // send delivers a control message; bulk(header, blob, options) sends a
  // transfer, as sendBulk does.
  constructor({ overlay, send, bulk }) {
    this.#overlay = overlay;
    this.#send = send;
    this.#bulk = bulk;
    this.#build(overlay.addSection('files'));

    onControl('file-offer', ({ file }) => {
      if (file?.id) this.#transfers.offer(file.id, String(file.name ?? ''), Number(file.size ?? 0));
    });
    onControl('file-withdraw', ({ file }) => this.#transfers.remove(file?.id));
    onControl('file-saved', ({ file }) => this.#transfers.saved(file?.id, String(file?.name ?? '')));
    onBulk(FILE_KIND, (h) => this.#download(h));

    window.addEventListener('dragover', (e) => {
      if (!carriesFiles(e)) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'copy';
      document.body.dataset.dropping = '';
      // dragover repeats while a drag stays over the page.
      clearTimeout(this.#dropTimer);
      this.#dropTimer = setTimeout(() => delete document.body.dataset.dropping, 200);
    });
    window.addEventListener('drop', (e) => {
      if (!carriesFiles(e)) return;
      e.preventDefault();
      delete document.body.dataset.dropping;
      this.upload(e.dataTransfer.files);
    });
  }

  // list is what the tab shows, for tests.
  list() {
    return this.#transfers.list().map((t) => ({ ...t }));
  }

  // upload sends files to the host, one at a time, in order.
  upload(files) {
    for (const file of files) {
      const id = `up-${++this.#uploads}`;
      this.#transfers.upload(id, file.name, file.size);
      if (file.size > MAX_FILE) {
        this.#transfers.failed(id, `over the ${formatSize(MAX_FILE)} limit`);
        continue;
      }
      this.#queue.push({ id, file });
    }
    this.#pump();
  }

  async #pump() {
    if (this.#pumping) return;
    this.#pumping = true;
    while (this.#queue.length) {
      const { id, file } = this.#queue.shift();
      if (this.#transfers.get(id)?.state !== QUEUED) continue; // cancelled while waiting
      const abort = new AbortController();
      this.#aborts.set(id, abort);
      this.#transfers.sending(id);
      try {
        await this.#bulk({ kind: FILE_KIND, id, name: file.name }, file, {
          signal: abort.signal,
          progress: (done) => this.#transfers.progress(id, done),
        });
        this.#transfers.finished(id);
        this.#clearLater(id);
      } catch (err) {
        if (abort.signal.aborted) this.#transfers.remove(id);
        else this.#transfers.failed(id, reasonOf(err));
      } finally {
        this.#aborts.delete(id);
      }
    }
    this.#pumping = false;
  }

  // download takes a transfer the agent opened, which must be a file the
  // user asked to save.
  #download(h) {
    if (!this.#transfers.fetching(h.id)) throw new Error('nobody asked for this file');
    const t = this.#transfers.get(h.id);
    const pieces = [];
    let piece = [];
    let pieceBytes = 0;
    let done = 0;
    return {
      write: (chunk) => {
        piece.push(chunk);
        pieceBytes += chunk.length;
        done += chunk.length;
        if (pieceBytes >= PIECE_BYTES) {
          pieces.push(new Blob(piece));
          piece = [];
          pieceBytes = 0;
        }
        this.#transfers.progress(h.id, done);
      },
      commit: () => {
        pieces.push(new Blob(piece));
        save(new Blob(pieces, { type: 'application/octet-stream' }), t.name);
        this.#transfers.finished(h.id);
        this.#clearLater(h.id);
      },
      abort: (err) => this.#transfers.failed(h.id, reasonOf(err)),
    };
  }

  #clearLater(id) {
    setTimeout(() => this.#transfers.remove(id), CLEAR_AFTER_MS);
  }

  #build(section) {
    this.#section = section;
    const head = document.createElement('div');
    head.className = 'files-head';
    const pick = button('upload', 'Upload files', () => this.#picker.click());
    pick.title = 'Send files to the host’s download folder';
    this.#picker = document.createElement('input');
    this.#picker.type = 'file';
    this.#picker.multiple = true;
    this.#picker.hidden = true;
    this.#picker.id = 'files-input';
    this.#picker.addEventListener('change', () => {
      this.upload(this.#picker.files);
      this.#picker.value = '';
    });
    const hint = document.createElement('span');
    hint.className = 'hint';
    hint.textContent = 'or drop them on the page';
    head.append(pick, this.#picker, hint);
    this.#list = document.createElement('ul');
    this.#list.className = 'transfers';
    section.append(head, this.#list);
  }

  #changed() {
    // Keep the tab out while an offer waits for the user or a transfer
    // runs; once nothing does, it tucks away as usual.
    const pinned = this.#transfers.waiting();
    if (pinned !== this.#section.hasAttribute('data-pinned')) {
      this.#section.toggleAttribute('data-pinned', pinned);
      this.#overlay.reveal();
    }
    this.#frame ||= requestAnimationFrame(() => {
      this.#frame = 0;
      this.#list.replaceChildren(...this.#transfers.list().map((t) => this.#row(t)));
    });
  }

  // row draws one transfer. Names and sizes come from the host or from
  // local files, so they go in as text, never as markup.
  #row(t) {
    const li = document.createElement('li');
    li.className = 'transfer';
    li.dataset.id = t.id;
    li.dataset.dir = t.dir;
    li.dataset.state = t.state;
    const name = document.createElement('span');
    name.className = 'name';
    name.textContent = t.name;
    name.title = t.name;
    const size = document.createElement('span');
    size.className = 'size';
    size.textContent = formatSize(t.size);
    const status = document.createElement('span');
    status.className = 'status';
    status.textContent = statusOf(t);
    li.append(name, size, status);
    if (t.state === SENDING || t.state === FETCHING) {
      const bar = document.createElement('progress');
      bar.max = Math.max(t.size, 1);
      bar.value = t.done;
      li.append(bar);
    }
    const actions = document.createElement('span');
    actions.className = 'actions';
    if (t.state === OFFERED) {
      actions.append(button('save', 'Save', () => {
        if (this.#transfers.fetch(t.id)) this.#send({ t: 'file-fetch', file: { id: t.id } });
      }));
    }
    if (t.state === OFFERED || t.state === FETCHING) {
      actions.append(button('dismiss', 'Dismiss', () => {
        this.#transfers.remove(t.id);
        this.#send({ t: 'file-decline', file: { id: t.id } });
      }));
    }
    if (t.state === QUEUED || t.state === SENDING) {
      actions.append(button('cancel', 'Cancel', () => {
        this.#aborts.get(t.id)?.abort(new Error('cancelled'));
        this.#transfers.remove(t.id);
      }));
    }
    if (t.state === FAILED || t.state === KEPT || t.state === SAVED) {
      actions.append(button('clear', 'Clear', () => this.#transfers.remove(t.id)));
    }
    li.append(actions);
    return li;
  }
}

function button(className, label, onClick) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = className;
  b.textContent = label;
  b.dataset.keys = 'local';
  b.addEventListener('click', () => {
    b.blur(); // the keyboard belongs to the remote again
    onClick();
  });
  return b;
}

function carriesFiles(e) {
  return [...(e.dataTransfer?.types ?? [])].includes('Files');
}

// save hands blob to the browser's downloads, under name.
function save(blob, name) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  a.hidden = true;
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), REVOKE_AFTER_MS);
}

function reasonOf(err) {
  return String(err?.message ?? err).replace(/^bulk transfer refused: /, '');
}
