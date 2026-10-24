// The switches for the microphone and camera, in a section of the status
// tab, and the senders they drive. Only devices the host took get a switch.

import {
  CAMERA_CONSTRAINTS,
  CAMERA_MAX_BITRATE,
  DeviceSwitch,
  MIC_CONSTRAINTS,
  constrainedBaselineFirst,
} from './devices.js';

const SVG = 'http://www.w3.org/2000/svg';

const DEVICES = {
  mic: {
    label: 'Microphone',
    heard: 'hear you',
    // A capsule on a stand.
    glyph: ['M8 1.75a2.25 2.25 0 0 1 2.25 2.25v3.5a2.25 2.25 0 0 1-4.5 0V4A2.25 2.25 0 0 1 8 1.75Z', 'M3.75 7.25a4.25 4.25 0 0 0 8.5 0M8 11.5v2.75'],
    constraints: MIC_CONSTRAINTS,
    track: (stream) => stream.getAudioTracks()[0],
  },
  camera: {
    label: 'Camera',
    heard: 'see you',
    // A body and its lens.
    glyph: ['M2.75 4.25h6.5a1.25 1.25 0 0 1 1.25 1.25v5a1.25 1.25 0 0 1-1.25 1.25h-6.5A1.25 1.25 0 0 1 1.5 10.5v-5a1.25 1.25 0 0 1 1.25-1.25Z', 'M10.5 6.75 14.5 4.5v7l-4-2.25'],
    constraints: CAMERA_CONSTRAINTS,
    track: (stream) => stream.getVideoTracks()[0],
  },
};

const TITLES = {
  off: (d) => `${d.label} off. Send it to the remote desktop`,
  starting: (d) => `${d.label}: waiting for the browser`,
  on: (d) => `${d.label} on: the remote desktop can ${d.heard}. Turn it off`,
  blocked: (d, why) => `${d.label} unavailable: ${why}. Try again`,
};

// offerDevices readies the senders for what config says the host takes,
// before the page makes its offer: the microphone on the sound line, which
// the caller made sendrecv for it, and the camera on a sendonly video line of
// its own. It returns the switches by name, and stop, which turns them off
// and takes them away with their section: they drive this peer
// connection's senders, and the one built after a reconnect brings its own.
export function offerDevices(pc, soundLine, config, overlay) {
  const switches = {};
  const senders = {};
  if (config.mic) senders.mic = soundLine.sender;
  if (config.camera) senders.camera = cameraLine(pc).sender;
  const names = Object.keys(senders);
  let section = null;
  if (names.length) {
    section = overlay.addSection('devices');
    section.classList.add('devices');
    for (const name of names) switches[name] = deviceSwitch(name, senders[name], section);
  }
  return {
    switches,
    stop: () => {
      section?.remove();
      return Promise.all(Object.values(switches).map((s) => s.off()));
    },
  };
}

// cameraLine adds the camera's video line, Constrained Baseline H.264 first:
// the agent decodes nothing else. Browsers judge the order by what they send
// on a sendonly line, older ones by what they receive.
function cameraLine(pc) {
  const line = pc.addTransceiver('video', { direction: 'sendonly', sendEncodings: [{ maxBitrate: CAMERA_MAX_BITRATE }] });
  for (const caps of [RTCRtpSender.getCapabilities?.('video'), RTCRtpReceiver.getCapabilities?.('video')]) {
    const codecs = constrainedBaselineFirst(caps?.codecs ?? []);
    if (!codecs) continue;
    try {
      line.setCodecPreferences(codecs);
      break;
    } catch {
      // This browser checks against the other list.
    }
  }
  return line;
}

function deviceSwitch(name, sender, section) {
  const d = DEVICES[name];
  const button = document.createElement('button');
  button.type = 'button';
  button.className = 'device';
  button.dataset.device = name;
  button.dataset.keys = 'local';
  button.append(glyph(d.glyph), text('label', d.label), text('live', ''));
  section.append(button);

  const show = (state, why) => {
    button.dataset.state = state;
    button.setAttribute('aria-pressed', String(state === 'on'));
    button.title = TITLES[state](d, why);
  };
  const sw = new DeviceSwitch({
    acquire: async () => d.track(await navigator.mediaDevices.getUserMedia(d.constraints)),
    send: (track) => sender.replaceTrack(track),
    changed: show,
  });
  show('off', '');
  button.addEventListener('click', () => {
    button.blur(); // keys belong to the remote, not to this button
    sw.toggle();
  });
  return sw;
}

function glyph(paths) {
  const svg = document.createElementNS(SVG, 'svg');
  svg.setAttribute('viewBox', '0 0 16 16');
  svg.setAttribute('aria-hidden', 'true');
  for (const d of paths) {
    const p = document.createElementNS(SVG, 'path');
    p.setAttribute('d', d);
    svg.append(p);
  }
  return svg;
}

function text(className, content) {
  const span = document.createElement('span');
  span.className = className;
  span.textContent = content;
  return span;
}
