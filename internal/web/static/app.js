import { Input } from './input.js';
import { Overlay } from './overlay.js';
import { StatsSampler } from './stats.js';
import { RemoteCursor } from './cursor.js';
import { dispatchControl, onControl } from './control.js';
import { acceptBulk, onBulk, sendBulk } from './bulk.js';
import { BULK_CHANNEL, registerKind } from './bulkframe.js';

const CONTROL_CHANNEL = 'control';
const ICE_GRACE_MS = 2000;
// The agent sends a heartbeat every second. Silence this long means media is
// not getting through even if ICE still looks connected, as when only the
// agent-to-browser direction of UDP dies; a restart moves to a working path.
const HEARTBEAT_STALL_MS = 3500;
const STATS_EVERY_MS = 1000;
const TOKEN_KEY = 'robin.token';

const video = document.getElementById('screen');
const typing = document.getElementById('typing');
const overlay = new Overlay();
const sampler = new StatsSampler();
const input = new Input(video, typing, document.getElementById('preedit'), sendControl, () => channel?.readyState === 'open');
const cursor = new RemoteCursor(video);

let ws = null;
let pc = null;
let channel = null;
let state = 'connecting';
let lastRemoteClipboard = null;
let wsOpened = false;
let iceGraceTimer = 0;
let heartbeatTimer = 0;
let lastControlAt = 0;
let statsTimer = 0;
let signals = Promise.resolve();
let earlyCandidates = [];
let monitors = null;

window.robin = {
  get pc() { return pc; },
  get state() { return state; },
  video,
  stats: () => sampler.sample(pc),
  sendClipboard: (text) => input.sendClipboard(text),
  get lastRemoteClipboard() { return lastRemoteClipboard; },
  get muted() { return video.muted; },
  get monitors() { return monitors; },
  showMonitor: (name) => showMonitor(name),
  // The picture's local zoom: scale, and the shift of its top-left corner.
  get zoom() {
    const v = input.view;
    return { scale: v.scale, x: v.x, y: v.y };
  },
  addSection: (name) => overlay.addSection(name),
  bulk: {
    registerKind,
    onBulk,
    send: (header, blob, options) => sendBulk(pc, header, blob, options),
  },
};

onControl('cursor', (c) => {
  if (c.cursor) cursor.apply(c.cursor);
});
onControl('monitors', (c) => {
  monitors = c.monitors ?? null;
  overlay.setMonitors(monitors, showMonitor);
});
onControl('clipboard', (c) => {
  lastRemoteClipboard = c.text ?? '';
  navigator.clipboard?.writeText(lastRemoteClipboard).catch(() => {
    // Needs focus and permission; the text is kept either way.
  });
});

input.attach();
// A touch screen has no keys of its own: the button raises the soft one.
if (navigator.maxTouchPoints > 0) overlay.showKeyboard(typing, () => input.toggleKeyboard());
connect(takeToken());

// takeToken reads the token from the fragment and moves it out of the address
// bar. sessionStorage keeps it for this tab only, so a reload still connects.
function takeToken() {
  let token = location.hash.slice(1);
  try {
    if (token) sessionStorage.setItem(TOKEN_KEY, token);
    else token = sessionStorage.getItem(TOKEN_KEY) ?? '';
  } catch {
    // Storage disabled: the session works, a reload will not.
  }
  if (location.hash) history.replaceState(null, '', location.pathname);
  return token;
}

function connect(token) {
  if (!token) {
    end('no-token');
    return;
  }
  try {
    ws = new WebSocket(`ws://${location.host}/ws`, ['robin', `token.${token}`]);
  } catch {
    end('refused');
    return;
  }
  ws.onopen = () => { wsOpened = true; };
  ws.onclose = () => end(wsOpened ? 'closed' : 'refused');
  ws.onmessage = (e) => {
    const m = JSON.parse(e.data);
    // Signals apply in arrival order: a candidate must not race its answer.
    signals = signals.then(() => onSignal(m)).catch((err) => console.warn('robin: signal', m.type, err));
  };
}

function sendSignal(m) {
  if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify(m));
}

function sendControl(c) {
  if (channel?.readyState === 'open') channel.send(JSON.stringify(c));
}

async function onSignal(m) {
  switch (m.type) {
    case 'config':
      input.scale = m.config?.hidpi ? window.devicePixelRatio || 1 : 1;
      if (!pc) createPeer(m.config ?? {});
      break;
    case 'answer': {
      await pc.setRemoteDescription({ type: 'answer', sdp: m.sdp });
      const early = earlyCandidates;
      earlyCandidates = [];
      for (const c of early) await addCandidate(c);
      break;
    }
    case 'candidate':
      // A candidate can overtake the answer it belongs to; hold it until then.
      if (pc.signalingState === 'have-local-offer') earlyCandidates.push(m.candidate);
      else await addCandidate(m.candidate);
      break;
  }
}

async function addCandidate(c) {
  try {
    await pc.addIceCandidate(c?.candidate ? c : { candidate: '' });
  } catch (err) {
    console.warn('robin: candidate', err);
  }
}

function createPeer(config) {
  pc = new RTCPeerConnection({ iceServers: config.iceServers ?? [] });

  const transceiver = pc.addTransceiver('video', { direction: 'recvonly' });
  preferH264(transceiver);
  // Sound shares the video's stream, so the one element plays both.
  pc.addTransceiver('audio', { direction: 'recvonly' });
  // One stream for every remote track: the agent may answer an audio line
  // it has nothing to send on, and that track must not replace the video.
  const remote = new MediaStream();
  video.srcObject = remote;
  pc.ontrack = (e) => {
    remote.addTrack(e.track);
    if (e.track.kind === 'audio') overlay.showSound(video);
  };

  channel = pc.createDataChannel(CONTROL_CHANNEL, { ordered: true });
  channel.onopen = () => {
    lastControlAt = Date.now();
    input.sendResize();
  };
  channel.onclose = () => input.release();
  channel.onmessage = (e) => {
    lastControlAt = Date.now();
    // A heartbeat-triggered restart may never change iceConnectionState.
    if (state === 'reconnecting' && ['connected', 'completed'].includes(pc.iceConnectionState)) setState('connected');
    dispatchControl(JSON.parse(e.data));
  };
  // The agent opens a channel of its own for each bulk transfer it sends.
  pc.ondatachannel = (e) => {
    if (e.channel.label === BULK_CHANNEL) acceptBulk(e.channel);
    else e.channel.close();
  };
  heartbeatTimer = setInterval(checkHeartbeat, 500);

  pc.onnegotiationneeded = async () => {
    try {
      await pc.setLocalDescription();
      sendSignal({ type: 'offer', sdp: pc.localDescription.sdp });
    } catch (err) {
      console.warn('robin: offer', err);
    }
  };
  pc.onicecandidate = (e) => {
    sendSignal({ type: 'candidate', candidate: e.candidate ? e.candidate.toJSON() : { candidate: '' } });
  };
  pc.oniceconnectionstatechange = onIceState;

  statsTimer = setInterval(async () => overlay.setStats(await sampler.sample(pc)), STATS_EVERY_MS);
}

// preferH264 puts Constrained Baseline H.264 with packetization-mode=1 first,
// which is what the agent encodes, and keeps every other codec after it so
// retransmission and fallbacks still negotiate.
function preferH264(transceiver) {
  const codecs = RTCRtpReceiver.getCapabilities('video')?.codecs ?? [];
  const isWanted = (c) => {
    if (c.mimeType.toLowerCase() !== 'video/h264') return false;
    const fmtp = (c.sdpFmtpLine ?? '').toLowerCase();
    return fmtp.includes('packetization-mode=1') && fmtp.includes('profile-level-id=42e01f');
  };
  const wanted = codecs.filter(isWanted);
  if (!wanted.length) return;
  transceiver.setCodecPreferences([...wanted, ...codecs.filter((c) => !isWanted(c))]);
}

function onIceState() {
  const s = pc.iceConnectionState;
  clearTimeout(iceGraceTimer);
  if (s === 'connected' || s === 'completed') {
    setState('connected');
  } else if (s === 'disconnected') {
    if (state === 'connected') setState('reconnecting');
    iceGraceTimer = setTimeout(restartIce, ICE_GRACE_MS);
  } else if (s === 'failed') {
    if (state === 'connected') setState('reconnecting');
    restartIce();
  }
}

// restartIce renegotiates through negotiationneeded with fresh credentials.
function restartIce() {
  if (state !== 'ended') pc.restartIce();
}

function checkHeartbeat() {
  if (state !== 'connected' || channel?.readyState !== 'open') return;
  if (Date.now() - lastControlAt < HEARTBEAT_STALL_MS) return;
  // Give the restart a full stall period before trying again.
  lastControlAt = Date.now();
  setState('reconnecting');
  restartIce();
}

// showMonitor asks the agent to show another monitor of the desktop, or
// all of them for 'all'. The video changes size when the frames do.
function showMonitor(name) {
  sendControl({ t: 'monitor', monitor: String(name) });
}

function setState(s) {
  if (state === s) return;
  state = s;
  overlay.setState(s);
}

function end(reason) {
  if (state === 'ended') return;
  clearTimeout(iceGraceTimer);
  clearInterval(statsTimer);
  clearInterval(heartbeatTimer);
  input.release();
  setState('ended');
  overlay.showNotice(reason);
  pc?.close();
}
