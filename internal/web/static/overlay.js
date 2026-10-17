// The HUD: a tab hanging from the top edge that tucks itself away while the
// session is healthy and comes back when the pointer nears the top.

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
    'The connection to remotejump closed. Reload this page to reconnect if remotejump is still running, or run it again.',
  ],
  refused: [
    'Could not connect',
    'This link has expired or is open in another tab. Use the link remotejump printed most recently.',
  ],
  'no-token': [
    'This link is incomplete',
    'Open the full link remotejump printed, including the part after the #.',
  ],
};

export class Overlay {
  #hud = document.getElementById('hud');
  #label = document.getElementById('state-label');
  #transport = document.getElementById('transport');
  #picture = this.#hud.querySelector('.picture');
  #resolution = document.getElementById('resolution');
  #fps = document.getElementById('fps');
  #bitrate = document.getElementById('bitrate');
  #notice = this.#hud.querySelector('.notice');
  #scrim = document.getElementById('scrim');
  #fullscreen = document.getElementById('fullscreen');
  #sound = document.getElementById('sound');
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
  }

  setState(state) {
    this.#hud.dataset.state = state;
    this.#label.textContent = STATE_LABELS[state];
    this.#reveal();
  }

  showNotice(reason) {
    const [title, body] = NOTICES[reason];
    this.#label.textContent = title;
    this.#notice.textContent = body;
    this.#notice.hidden = false;
    for (const el of [this.#transport, this.#picture, this.#fullscreen, this.#sound]) el.hidden = true;
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

  #reveal() {
    this.#hud.classList.remove('tucked');
    this.#scheduleTuck();
  }

  #scheduleTuck() {
    clearTimeout(this.#tuckTimer);
    if (this.#hovered || this.#hud.dataset.state !== 'connected') return;
    this.#tuckTimer = setTimeout(() => this.#hud.classList.add('tucked'), TUCK_AFTER_MS);
  }
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
