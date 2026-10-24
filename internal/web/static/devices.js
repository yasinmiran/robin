// The browser's microphone and camera, which the host takes when the CLI ran
// with --mic or --camera. Each starts off; turning it on asks the browser for
// the device and puts its track on a sender negotiated from the start, so a
// switch never renegotiates. Turning it off takes the track away and stops
// it, which puts the browser's own camera light out. Nothing here touches the
// DOM or the browser: what a switch acts on comes in through its io.

// What the page asks the browser for. The host gets one fixed picture size,
// 1280x720, and fits whatever arrives into it.
export const MIC_CONSTRAINTS = {
  audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true, channelCount: 1 },
};
export const CAMERA_CONSTRAINTS = {
  video: { width: { ideal: 1280 }, height: { ideal: 720 }, frameRate: { ideal: 30, max: 30 } },
};
// The camera's share of the link, which it has to leave to the desktop.
export const CAMERA_MAX_BITRATE = 1_500_000;

const REASONS = {
  NotAllowedError: 'The browser was not allowed to use it',
  SecurityError: 'The browser was not allowed to use it',
  NotFoundError: 'This computer has none',
  OverconstrainedError: 'This computer has none',
  NotReadableError: 'Another program is using it',
  AbortError: 'Another program is using it',
};

// reason says why a device could not be had, for the switch's title.
export function reason(err) {
  return REASONS[err?.name] ?? err?.message ?? String(err);
}

// DeviceSwitch is one device's switch. Its state is off, starting (the
// browser is asking the user or opening the device), on, or blocked (it
// could not be had; why says why, and turning it on tries again).
//
// io.acquire resolves to a MediaStreamTrack, io.send puts a track or null on
// the sender, and io.changed hears every change of state.
export class DeviceSwitch {
  #io;
  #state = 'off';
  #why = '';
  #track = null;
  #wanted = false;

  constructor(io) {
    this.#io = io;
  }

  get state() {
    return this.#state;
  }

  get why() {
    return this.#why;
  }

  toggle() {
    return this.#state === 'on' || this.#state === 'starting' ? this.off() : this.on();
  }

  async on() {
    if (this.#state === 'on' || this.#state === 'starting') return;
    this.#wanted = true;
    this.#set('starting');
    let track;
    try {
      track = await this.#io.acquire();
    } catch (err) {
      if (this.#wanted) this.#block(err);
      return;
    }
    // Turned off while the browser asked: the user has changed their mind.
    if (!this.#wanted) {
      track.stop();
      return;
    }
    try {
      await this.#io.send(track);
    } catch (err) {
      track.stop();
      if (this.#wanted) this.#block(err);
      return;
    }
    if (!this.#wanted) {
      track.stop();
      await this.#io.send(null).catch(() => {});
      return;
    }
    this.#track = track;
    // Unplugged, or the permission taken back in the browser's own controls.
    track.addEventListener?.('ended', () => {
      if (this.#track === track) this.off();
    });
    this.#set('on');
  }

  async off() {
    this.#wanted = false;
    const track = this.#track;
    this.#track = null;
    if (this.#state === 'off') return;
    this.#set('off');
    if (!track) return;
    track.stop();
    await this.#io.send(null).catch(() => {});
  }

  #block(err) {
    this.#why = reason(err);
    this.#set('blocked');
  }

  #set(state) {
    this.#state = state;
    if (state !== 'blocked') this.#why = '';
    this.#io.changed?.(state, this.#why);
  }
}

// constrainedBaselineFirst orders codecs with H.264 Constrained Baseline in
// packetization mode 1 first, which is what the agent encodes and decodes,
// keeping every other codec after it so retransmission and fallbacks still
// negotiate. It returns null when the browser has no such codec.
export function constrainedBaselineFirst(codecs) {
  const wanted = (c) => {
    if (c.mimeType.toLowerCase() !== 'video/h264') return false;
    const fmtp = (c.sdpFmtpLine ?? '').toLowerCase();
    return fmtp.includes('packetization-mode=1') && fmtp.includes('profile-level-id=42e01f');
  };
  const first = codecs.filter(wanted);
  if (!first.length) return null;
  return [...first, ...codecs.filter((c) => !wanted(c))];
}
