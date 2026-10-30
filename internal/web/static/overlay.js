// The HUD: a tab hanging from the top edge that tucks itself away while the
// session is healthy and comes back when the pointer nears the top. Where
// the pointer is a finger, which hovers nowhere, it stays out.

import { touchFirst } from './device.js';
import { leftNotice, sentence } from './sharestate.js';

const TUCK_AFTER_MS = 2500;
const REVEAL_ZONE_PX = 48;

const STATE_LABELS = {
  connecting: 'Connecting',
  connected: 'Connected',
  reconnecting: 'Reconnecting',
  ended: 'Session ended',
};

const TRANSPORT_LABELS = {
  udp: 'Direct UDP',
  tcp: 'SSH tunnel',
};

const NOTICES = {
  closed: [
    'Session ended',
    'The connection to Robin closed. Reload this page to reconnect if Robin is still running, or run it again.',
  ],
  refused: [
    'Could not connect',
    'This link has expired or is open in another tab. Use the link Robin printed most recently.',
  ],
  'no-token': [
    'This link is incomplete',
    'Open the full link Robin printed, including the part after the #.',
  ],
  kept: [
    'Desktop kept',
    'The desktop is still running on the host. Run robin gui there again to pick it up, or robin end to end it.',
  ],
  ended: [
    'Session ended',
    'The session ended; the desktop on the host is gone.',
  ],
  unconfirmed: [
    'Session ended',
    'Robin could not confirm that the host ended the session. Run robin end for the host to make sure.',
  ],
};

export class Overlay {
  #hud = document.getElementById('hud');
  #label = document.getElementById('state-label');
  #transport = document.getElementById('transport');
  #guests = document.getElementById('guests');
  #role = ''; // what a guest's page says for Connected
  #picture = this.#hud.querySelector('.picture');
  #resolution = document.getElementById('resolution');
  #fps = document.getElementById('fps');
  #bitrate = document.getElementById('bitrate');
  #notice = this.#hud.querySelector('.notice');
  #scrim = document.getElementById('scrim');
  #fullscreen = document.getElementById('fullscreen');
  #sound = document.getElementById('sound');
  #keyboard = document.getElementById('keyboard');
  #keys = document.getElementById('keys');
  #monitors = this.#hud.querySelector('.monitors');
  #monitorButton = document.getElementById('monitor');
  #monitorMenu = document.getElementById('monitor-menu');
  #sections = null;
  #lastSection = null;
  #pickMonitor = null;
  #tuckTimer = 0;
  #hovered = false;

  constructor() {
    this.#hud.addEventListener('pointerenter', () => {
      this.#hovered = true;
      this.#reveal();
    });
    this.#hud.addEventListener('pointerleave', () => {
      this.#hovered = false;
      this.#scheduleTuck();
    });
    window.addEventListener('pointermove', (e) => {
      if (e.clientY <= REVEAL_ZONE_PX) this.#reveal();
    });
    this.#fullscreen.addEventListener('click', () => {
      this.#fullscreen.blur();
      toggleFullscreen();
    });
    document.addEventListener('fullscreenchange', () => {
      const on = Boolean(document.fullscreenElement);
      this.#fullscreen.setAttribute('aria-label', on ? 'Exit full screen' : 'Enter full screen');
      if (!on) navigator.keyboard?.unlock();
    });
    this.#monitorButton.addEventListener('click', () => this.#toggleMonitorMenu());
    this.#monitorMenu.addEventListener('keydown', (e) => this.#onMenuKey(e));
    this.#monitorButton.addEventListener('keydown', (e) => {
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        this.#toggleMonitorMenu(true);
      }
    });
    document.addEventListener('pointerdown', (e) => {
      if (!this.#monitors.contains(e.target)) this.#closeMonitorMenu(false);
    });
  }

  // setMonitors shows the switcher for a desktop of several monitors: the one
  // shown, and a menu of every monitor plus all of them as one picture. pick
  // is called with the name of the one chosen.
  setMonitors(ms, pick) {
    this.#pickMonitor = pick;
    const list = ms?.list ?? [];
    if (list.length < 2 || this.#hud.dataset.state === 'ended') {
      this.#closeMonitorMenu(false);
      this.#monitors.hidden = true;
      return;
    }
    const all = boundsOf(list);
    const current = list.find((m) => m.name === ms.current);
    this.#monitorButton.querySelector('.label').textContent = current ? current.name : 'All monitors';
    this.#monitorButton.title = current
      ? `Showing ${current.name}, ${current.w} × ${current.h}. Choose another monitor`
      : `Showing all monitors, ${all.w} × ${all.h}. Choose one monitor`;
    const items = list.map((m) => menuItem(m.name, m.name, `${m.w} × ${m.h}`, m.primary ? 'primary' : '', m.name === ms.current));
    items.push(menuItem('all', 'All monitors', `${all.w} × ${all.h}`, '', !current));
    for (const item of items) {
      item.addEventListener('click', () => {
        this.#closeMonitorMenu(true);
        if (item.getAttribute('aria-checked') !== 'true') this.#pickMonitor?.(item.dataset.name);
      });
    }
    this.#monitorMenu.replaceChildren(...items);
    this.#monitors.hidden = false;
    this.#reveal();
  }

  #toggleMonitorMenu(focusFirst = false) {
    if (!this.#monitorMenu.hidden) {
      this.#closeMonitorMenu(true);
      return;
    }
    this.#monitorMenu.hidden = false;
    this.#monitorButton.setAttribute('aria-expanded', 'true');
    this.#reveal();
    const items = [...this.#monitorMenu.children];
    const target = focusFirst ? items[0] : items.find((i) => i.getAttribute('aria-checked') === 'true');
    target?.focus();
  }

  // closeMonitorMenu hides the menu. With release set, focus leaves the
  // switcher too, so the remote gets the keyboard again.
  #closeMonitorMenu(release) {
    if (this.#monitorMenu.hidden) return;
    this.#monitorMenu.hidden = true;
    this.#monitorButton.setAttribute('aria-expanded', 'false');
    if (release && this.#monitors.contains(document.activeElement)) document.activeElement.blur();
    this.#scheduleTuck();
  }

  #onMenuKey(e) {
    const items = [...this.#monitorMenu.children];
    const at = items.indexOf(document.activeElement);
    if (e.key === 'Escape' || e.key === 'Tab') {
      e.preventDefault();
      this.#closeMonitorMenu(true);
    } else if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      const step = e.key === 'ArrowDown' ? 1 : -1;
      items[(at + step + items.length) % items.length]?.focus();
    } else if (e.key === 'Home' || e.key === 'End') {
      e.preventDefault();
      items[e.key === 'Home' ? 0 : items.length - 1]?.focus();
    }
  }

  // addSection gives a feature its own part of the tab, below the row of
  // stats, and returns the element for it to fill. Sections stack in the
  // order they were added, except that one added with last stays below
  // every other, as how to leave goes below what one can do. They take no
  // room while empty, and go with the rest of the tab when the session
  // ends.
  addSection(name, { last = false } = {}) {
    if (!this.#sections) {
      this.#sections = document.createElement('div');
      this.#sections.className = 'sections';
      this.#sections.hidden = this.#hud.dataset.state === 'ended';
      this.#notice.before(this.#sections);
    }
    if ([...this.#sections.children].some((s) => s.dataset.section === name)) {
      throw new Error(`overlay: section ${JSON.stringify(name)} added twice`);
    }
    const section = document.createElement('section');
    section.className = 'section';
    section.dataset.section = name;
    this.#sections.insertBefore(section, this.#lastSection);
    if (last) this.#lastSection = section;
    return section;
  }

  setState(state) {
    this.#hud.dataset.state = state;
    this.#label.textContent = state === 'connected' && this.#role ? this.#role : STATE_LABELS[state];
    this.#reveal();
  }

  // setRole says, in Connected's place, what this page does on a desktop
  // that is not its own, as a guest's watches or controls it; viewOnly
  // shows the dot waiting, as for a page that cannot drive yet.
  setRole(label, viewOnly) {
    this.#role = label;
    this.#hud.toggleAttribute('data-view-only', viewOnly);
    if (this.#hud.dataset.state === 'connected') this.#label.textContent = label;
  }

  // setGuests says in the row who watches this desktop, or hides the
  // stat for an empty text: the tab tucks, and the row is what stays read.
  setGuests(text) {
    this.#guests.hidden = !text || this.#hud.dataset.state === 'ended';
    this.#guests.querySelector('.label').textContent = text;
  }

  // showNotice ends the tab with the notice for reason, after detail, a
  // reason the CLI gave, when there is one. A guest's page that left, or
  // was let go, names owner, whose desktop it was.
  showNotice(reason, detail = '', owner = '') {
    const left = reason === 'left';
    const [title, said] = left ? leftNotice(detail, owner) : NOTICES[reason];
    const body = !left && detail ? `${sentence(detail)} ${said}` : said;
    this.#label.textContent = title;
    this.#notice.textContent = body;
    this.#notice.hidden = false;
    this.#closeMonitorMenu(false);
    for (const el of [this.#transport, this.#guests, this.#picture, this.#fullscreen, this.#sound, this.#keyboard, this.#keys, this.#monitors, this.#sections]) {
      if (el) el.hidden = true;
    }
    this.#scrim.hidden = false;
    this.#reveal();
  }

  // showSound offers the speaker toggle for media, which starts muted: a
  // browser only autoplays muted media, and unmuting needs a user gesture.
  showSound(media) {
    if (!this.#sound.hidden || this.#hud.dataset.state === 'ended') return;
    const reflect = () => {
      this.#sound.setAttribute('aria-pressed', String(!media.muted));
      this.#sound.title = media.muted ? 'Turn sound on' : 'Turn sound off';
    };
    this.#sound.addEventListener('click', () => {
      this.#sound.blur(); // keys belong to the remote, not to this button
      media.muted = !media.muted;
    });
    media.addEventListener('volumechange', reflect);
    reflect();
    this.#sound.hidden = false;
    this.#reveal();
  }

  // showKeyboard offers a button that raises the soft keyboard: toggle
  // focuses the text box, or lets go of it, and the button shows whether it
  // has the focus.
  showKeyboard(text, toggle) {
    if (this.#hud.dataset.state === 'ended') return;
    const reflect = () => {
      const on = document.activeElement === text;
      this.#keyboard.setAttribute('aria-pressed', String(on));
      this.#keyboard.title = on ? 'Hide the keyboard' : 'Show the keyboard';
    };
    // Pressing the button must not take the focus from the text box first.
    this.#keyboard.addEventListener('mousedown', (e) => e.preventDefault());
    this.#keyboard.addEventListener('click', toggle);
    text.addEventListener('focus', reflect);
    text.addEventListener('blur', reflect);
    reflect();
    this.#keyboard.hidden = false;
    this.#reveal();
  }

  setStats(s) {
    const transport = TRANSPORT_LABELS[s.transport];
    this.#transport.hidden = !transport;
    this.#transport.dataset.kind = s.transport ?? '';
    this.#transport.querySelector('.label').textContent = transport ?? '';

    this.#picture.hidden = !(s.frameWidth > 0);
    if (this.#picture.hidden) return;
    setMeasure(this.#resolution, `${s.frameWidth} `, '×', ` ${s.frameHeight}`);
    setMeasure(this.#fps, `${s.fps} `, 'fps');
    const [value, unit] = s.kbps >= 1000 ? [`${(s.kbps / 1000).toFixed(1)} `, 'Mb/s'] : [`${s.kbps} `, 'kb/s'];
    setMeasure(this.#bitrate, value, unit);
  }

  // reveal brings the tab out for a feature with news. It tucks away again
  // later, unless a section holds it out with a data-pinned attribute; a
  // feature that drops its pin calls reveal again.
  reveal() {
    this.#reveal();
  }

  #reveal() {
    this.#hud.classList.remove('tucked');
    this.#scheduleTuck();
  }

  #scheduleTuck() {
    clearTimeout(this.#tuckTimer);
    if (touchFirst || this.#hovered || !this.#monitorMenu.hidden || this.#hud.dataset.state !== 'connected') return;
    if (this.#sections?.querySelector(':scope > [data-pinned]')) return;
    this.#tuckTimer = setTimeout(() => this.#hud.classList.add('tucked'), TUCK_AFTER_MS);
  }
}

// menuItem is one choice in the monitor menu.
function menuItem(name, label, size, tag, checked) {
  const item = document.createElement('button');
  item.type = 'button';
  item.setAttribute('role', 'menuitemradio');
  item.setAttribute('aria-checked', String(checked));
  item.dataset.name = name;
  item.dataset.keys = 'local';
  const check = document.createElement('span');
  check.className = 'check';
  check.setAttribute('aria-hidden', 'true');
  check.textContent = '✓';
  const title = document.createElement('span');
  title.textContent = label;
  if (tag) {
    const t = document.createElement('span');
    t.className = 'tag';
    t.textContent = tag;
    title.append(t);
  }
  const dims = document.createElement('span');
  dims.className = 'size';
  dims.textContent = size;
  item.append(check, title, dims);
  return item;
}

// boundsOf is the size of the picture all monitors make together.
function boundsOf(list) {
  const x0 = Math.min(...list.map((m) => m.x));
  const y0 = Math.min(...list.map((m) => m.y));
  return {
    w: Math.max(...list.map((m) => m.x + m.w)) - x0,
    h: Math.max(...list.map((m) => m.y + m.h)) - y0,
  };
}

// setMeasure writes alternating value and unit parts, units dimmed.
function setMeasure(el, ...parts) {
  el.replaceChildren(...parts.map((text, i) => {
    if (i % 2 === 0) return document.createTextNode(text);
    const unit = document.createElement('span');
    unit.className = 'unit';
    unit.textContent = text;
    return unit;
  }));
}

async function toggleFullscreen() {
  if (document.fullscreenElement) {
    await document.exitFullscreen();
    return;
  }
  await document.documentElement.requestFullscreen({ navigationUI: 'hide' });
  // Keyboard lock lets Ctrl+W, Alt+Tab and friends reach the remote instead of
  // the browser. It only takes effect in fullscreen.
  try {
    await navigator.keyboard?.lock();
  } catch {
    // Unsupported or refused: the session works without it.
  }
}
