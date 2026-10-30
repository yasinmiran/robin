import { hasTouch } from './device.js';
import { Input } from './input.js';
import { Overlay } from './overlay.js';
import { StatsSampler } from './stats.js';
import { RemoteCursor } from './cursor.js';
import { dispatchControl, onControl } from './control.js';
import { acceptBulk, onBulk, sendBulk } from './bulk.js';
import { BULK_CHANNEL, registerKind } from './bulkframe.js';
import { Session } from './session.js';
import { Files } from './files.js';
import { Clipboard } from './clipboard.js';
import { offerDevices } from './devicebar.js';
import { constrainedBaselineFirst } from './devices.js';
import { PrivateSwitches } from './private.js';
import { Keys } from './keys.js';
import { SharpText } from './sharp.js';
import { Sharing } from './share.js';

const CONTROL_CHANNEL = 'control';
const ICE_GRACE_MS = 2000;
// The agent sends a heartbeat every second. Silence this long means media is
// not getting through even if ICE still looks connected, as when only the
// agent-to-browser direction of UDP dies; a restart moves to a working path.
const HEARTBEAT_STALL_MS = 3500;
const STATS_EVERY_MS = 1000;
const TOKEN_KEY = 'robin.token';
// A guest's page shares the owner's stream, whose tiles follow the owner's.
const GUEST_SHARP = 'Not available to a guest: sharp text follows the page of the desktop’s owner.';

const video = document.getElementById('screen');
const typing = document.getElementById('typing');
const overlay = new Overlay();
const sampler = new StatsSampler();
const toAgent = (header, blob, options) => sendBulk(pc, header, blob, options);
const files = new Files({ overlay, send: sendControl, bulk: toAgent });
const clipboard = new Clipboard({ send: sendControl, bulk: toAgent, propose: (list) => files.propose(list) });
const input = new Input(video, typing, document.getElementById('preedit'), sendControl, () => channel?.readyState === 'open', {
  paste: (data) => clipboard.paste(data),
});
const keys = new Keys({
  overlay, toggle: document.getElementById('keys'), send: sendControl, live: () => channel?.readyState === 'open', signal: sendSignal, input,
});
const cursor = new RemoteCursor(video);
const session = new Session();
const sessionControls = sessionSection(overlay.addSection('session', { last: true }));
const privateSwitches = new PrivateSwitches(overlay, sendControl);
const sharp = new SharpText({ video, overlay, send: sendControl, input });
const sharing = new Sharing(overlay, sendControl);

let ws = null;
let pc = null;
let channel = null;
let state = 'connecting';
let wsOpened = false;
let iceGraceTimer = 0;
let heartbeatTimer = 0;
let lastControlAt = 0;
let statsTimer = 0;
let signals = Promise.resolve();
let earlyCandidates = [];
let monitors = null;
let devices = null;

window.robin = {
  get pc() { return pc; },
  get state() { return state; },
  video,
  stats: () => sampler.sample(pc),
  sendClipboard: (text) => input.sendClipboard(text),
  // paste takes a DataTransfer as a paste event would carry it. Firefox
  // keeps none of a script's DataTransfer in a ClipboardEvent it builds.
  paste: (data) => clipboard.paste(data),
  get lastRemoteClipboard() { return clipboard.last?.text ?? null; },
  get lastRemoteClip() { return clipboard.last; },
  files: {
    list: () => files.list(),
    upload: (list) => files.upload(list),
  },
  get muted() { return video.muted; },
  get monitors() { return monitors; },
  showMonitor: (name) => showMonitor(name),
  // The picture's local zoom: scale, and the shift of its top-left corner.
  get zoom() {
    const v = input.view;
    return { scale: v.scale, x: v.x, y: v.y };
  },
  get gesture() { return input.gesture; },
  get private() { return privateSwitches.state; },
  share: {
    get state() { return sharing.state; },
    get guest() { return sharing.guest; },
    invite: (user) => sharing.invite(user),
    change: (id, action) => sharing.change(id, action),
  },
  sharp: {
    get state() { return sharp.state; },
    trace: () => sharp.trace(),
    stopTrace: () => sharp.stopTrace(),
  },
  addSection: (name) => overlay.addSection(name),
  detach: () => leave(session.detach()),
  endSession: () => leave(session.end()),
  device: (name) => devices?.switches[name]?.state ?? null,
  bulk: {
    registerKind,
    onBulk,
    send: toAgent,
  },
};

onControl('cursor', (c) => {
  if (c.cursor) cursor.apply(c.cursor);
});
onControl('monitors', (c) => {
  monitors = c.monitors ?? null;
  overlay.setMonitors(monitors, showMonitor);
});

input.attach();
// A touch screen has no keys of its own: the button raises the soft one.
if (hasTouch) overlay.showKeyboard(typing, () => input.toggleKeyboard());
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
  ws.onclose = () => end(session.closeReason(wsOpened));
  ws.onmessage = (e) => {
    const m = JSON.parse(e.data);
    // Signals apply in arrival order: a candidate must not race its answer.
    signals = signals.then(() => onSignal(m)).catch((err) => console.warn('robin: signal', m.type, err));
  };
}

// sendSignal sends a message to the CLI, and says whether it went.
function sendSignal(m) {
  if (ws?.readyState !== WebSocket.OPEN) return false;
  ws.send(JSON.stringify(m));
  return true;
}

function sendControl(c) {
  if (channel?.readyState === 'open') channel.send(JSON.stringify(c));
}

async function onSignal(m) {
  switch (m.type) {
    case 'config':
      input.scale = m.config?.hidpi ? window.devicePixelRatio || 1 : 1;
      session.configure(m.config);
      sharing.configure(m.config);
      if (sharing.guest) guestControls();
      sessionControls.keep.hidden = !session.canKeep;
      keys.configure(m.config?.macros);
      // After a reconnect there is no peer; the CLI's new config asks for one.
      if (!pc) createPeer(m.config ?? {});
      break;
    case 'macros':
      keys.changed(m.macros);
      break;
    case 'reconnecting':
      // The CLI lost the host and is redialing; the peer connection it
      // relayed is dead, and the next config brings a new one.
      dropPeer();
      setState('reconnecting');
      break;
    case 'ended':
      // The host confirmed the end; the CLI closes the socket next. A
      // guest's ending says why: it left, or the owner let it go.
      session.ended();
      if (sharing.guest) end('left', m.error);
      else end('ended');
      break;
    case 'answer': {
      if (!pc) break;
      await pc.setRemoteDescription({ type: 'answer', sdp: m.sdp });
      const early = earlyCandidates;
      earlyCandidates = [];
      for (const c of early) await addCandidate(c);
      break;
    }
    case 'candidate':
      if (!pc) break;
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
  const codecs = constrainedBaselineFirst(RTCRtpReceiver.getCapabilities('video')?.codecs ?? []);
  if (codecs) transceiver.setCodecPreferences(codecs);
  // Sound shares the video's stream, so the one element plays both. With the
  // microphone on offer the line goes both ways from the start, as does the
  // camera's own, so switching either is a replaceTrack and never a new offer.
  const sound = pc.addTransceiver('audio', { direction: config.mic ? 'sendrecv' : 'recvonly' });
  devices = offerDevices(pc, sound, config, overlay);
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
    privateSwitches.reopened();
    sharp.open();
  };
  channel.onclose = () => {
    input.release();
    keys.stop();
  };
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

// dropPeer closes the peer connection and forgets it, keeping the page and
// its socket for the next one. The old connection's events must not reach
// handlers that now expect none.
function dropPeer() {
  clearTimeout(iceGraceTimer);
  clearInterval(statsTimer);
  clearInterval(heartbeatTimer);
  input.release();
  keys.stop();
  devices?.stop();
  sharp.reset();
  earlyCandidates = [];
  const old = pc;
  pc = null;
  channel = null;
  if (!old) return;
  old.oniceconnectionstatechange = null;
  old.onicecandidate = null;
  old.onnegotiationneeded = null;
  old.ontrack = null;
  old.ondatachannel = null;
  old.close();
}

// leave sends the page's last word to the CLI and waits for the socket to
// close, which ends the page with the matching notice.
function leave(signal) {
  if (state === 'ended') return;
  sendSignal(signal);
  sessionControls.keep.disabled = sessionControls.end.disabled = true;
}

// sessionSection fills the status tab's session part: a button that leaves
// the desktop running for a later robin gui, shown when the host keeps it,
// and one that ends the session.
function sessionSection(section) {
  const keep = document.createElement('button');
  keep.type = 'button';
  keep.id = 'detach';
  keep.dataset.keys = 'local';
  keep.textContent = 'Disconnect, keep desktop';
  keep.title = 'Close this page and Robin, leaving the desktop running on the host for robin gui to pick up again';
  keep.hidden = true;
  keep.addEventListener('click', () => leave(session.detach()));
  const end = document.createElement('button');
  end.type = 'button';
  end.id = 'end';
  end.dataset.keys = 'local';
  end.textContent = 'End session';
  end.title = 'End the session and the desktop on the host';
  end.addEventListener('click', () => leave(session.end()));
  section.append(keep, end);
  return { keep, end };
}

// guestControls turns the session's buttons into a guest's: a guest
// leaves, and the desktop stays with its owner.
function guestControls() {
  sharp.withhold(GUEST_SHARP);
  sessionControls.end.textContent = 'Leave';
  sessionControls.end.title = 'Stop watching; the desktop stays with its owner';
}

function end(reason, detail = '') {
  if (state === 'ended') return;
  dropPeer();
  setState('ended');
  overlay.showNotice(reason, detail);
}
