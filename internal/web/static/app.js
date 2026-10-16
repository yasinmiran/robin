import { Input } from './input.js';
import { Overlay } from './overlay.js';
import { StatsSampler } from './stats.js';

const CONTROL_CHANNEL = 'control';
const ICE_GRACE_MS = 2000;
const STATS_EVERY_MS = 1000;
const TOKEN_KEY = 'remotejump.token';

const video = document.getElementById('screen');
const overlay = new Overlay();
const sampler = new StatsSampler();
const input = new Input(video, sendControl, () => channel?.readyState === 'open');

let ws = null;
let pc = null;
let channel = null;
let state = 'connecting';
let lastRemoteClipboard = null;
let wsOpened = false;
let iceGraceTimer = 0;
let statsTimer = 0;
let signals = Promise.resolve();
let earlyCandidates = [];

window.remotejump = {
  get pc() { return pc; },
  get state() { return state; },
  video,
  stats: () => sampler.sample(pc),
  sendClipboard: (text) => input.sendClipboard(text),
  get lastRemoteClipboard() { return lastRemoteClipboard; },
};

input.attach();
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
    ws = new WebSocket(`ws://${location.host}/ws`, ['remotejump', `token.${token}`]);
  } catch {
    end('refused');
    return;
  }
  ws.onopen = () => { wsOpened = true; };
  ws.onclose = () => end(wsOpened ? 'closed' : 'refused');
  ws.onmessage = (e) => {
    const m = JSON.parse(e.data);
    // Signals apply in arrival order: a candidate must not race its answer.
    signals = signals.then(() => onSignal(m)).catch((err) => console.warn('remotejump: signal', m.type, err));
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
    console.warn('remotejump: candidate', err);
  }
}

function createPeer(config) {
  pc = new RTCPeerConnection({ iceServers: config.iceServers ?? [] });

  const transceiver = pc.addTransceiver('video', { direction: 'recvonly' });
  preferH264(transceiver);
  pc.ontrack = (e) => {
    video.srcObject = e.streams[0] ?? new MediaStream([e.track]);
  };

  channel = pc.createDataChannel(CONTROL_CHANNEL, { ordered: true });
  channel.onopen = () => input.sendResize();
  channel.onclose = () => input.release();
  channel.onmessage = (e) => onControl(JSON.parse(e.data));

  pc.onnegotiationneeded = async () => {
    try {
      await pc.setLocalDescription();
      sendSignal({ type: 'offer', sdp: pc.localDescription.sdp });
    } catch (err) {
      console.warn('remotejump: offer', err);
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

function onControl(c) {
  if (c.t !== 'clipboard') return;
  lastRemoteClipboard = c.text ?? '';
  navigator.clipboard?.writeText(lastRemoteClipboard).catch(() => {
    // Needs focus and permission; the text is kept either way.
  });
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
  input.release();
  setState('ended');
  overlay.showNotice(reason);
  pc?.close();
}
