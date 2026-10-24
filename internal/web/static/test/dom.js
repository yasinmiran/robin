// Just enough of a DOM for files.js, clipboard.js, devicebar.js and keys.js
// under node: elements that hold children, attributes and click listeners, a
// window and a body, and animation frames on timers. installDom puts it on
// globalThis.

export class FakeElement extends EventTarget {
  children = [];
  dataset = {};
  classList = new Set(); // add and has, as a DOMTokenList has
  #attrs = new Set();

  constructor(tag) {
    super();
    this.tagName = tag.toUpperCase();
  }

  append(...nodes) {
    this.children.push(...nodes);
  }

  replaceChildren(...nodes) {
    this.children = nodes;
  }

  remove() {}

  blur() {}

  click() {
    this.dispatchEvent(new Event('click'));
  }

  hasAttribute(name) {
    return this.#attrs.has(name);
  }

  setAttribute(name) {
    this.#attrs.add(name);
  }

  toggleAttribute(name, force = !this.#attrs.has(name)) {
    if (force) this.#attrs.add(name);
    else this.#attrs.delete(name);
    return force;
  }

  // find is the first element at or under this one that matches.
  find(match) {
    if (match(this)) return this;
    for (const child of this.children) {
      const found = child.find?.(match);
      if (found) return found;
    }
    return null;
  }
}

// installDom returns the anchors the page clicked to save a download.
export function installDom() {
  const saved = [];
  const body = new FakeElement('body');
  globalThis.window = new EventTarget();
  globalThis.document = {
    body,
    createElement(tag) {
      const el = new FakeElement(tag);
      if (tag === 'a') el.addEventListener('click', () => saved.push({ name: el.download, href: el.href }));
      return el;
    },
    createElementNS: (_, tag) => new FakeElement(tag),
    createTextNode: (textContent) => ({ textContent }),
  };
  globalThis.requestAnimationFrame = (fn) => setTimeout(fn, 0);
  // The page's long timers, clearing rows and revoking URLs, must not keep
  // a finished test alive.
  const setTimer = globalThis.setTimeout;
  globalThis.setTimeout = (fn, ms, ...args) => {
    const timer = setTimer(fn, ms, ...args);
    if (ms >= 1000) timer.unref();
    return timer;
  };
  return saved;
}

// fakeOverlay hands out one section, as Overlay.addSection does.
export function fakeOverlay() {
  const section = new FakeElement('section');
  return { section, addSection: () => section, reveal() {} };
}
