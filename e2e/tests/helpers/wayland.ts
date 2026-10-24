import { shellQuote, ssh, sshRaw } from './host';

// Host-side checks for the wayland scenario, through wlhost/robin-wayland on the
// Wayland host image (e2e/wlhost.Containerfile).

// ProbeEvent is one line the probe window logged: input it received, in
// monitor pixels, or clipboard text it read.
export type ProbeEvent = {
  ev: string;
  x?: number;
  y?: number;
  button?: number;
  keyval?: string;
  text?: string;
  w?: number;
  h?: number;
};

export async function events(): Promise<ProbeEvent[]> {
  const out = await ssh('robin-wayland events');
  return out
    .split('\n')
    .filter((l) => l.startsWith('{'))
    .map((l) => JSON.parse(l) as ProbeEvent);
}

// consent answers the portal's dialog as the person at the host's screen.
export const consent = (viewOnly = false) => ssh(`robin-wayland consent${viewOnly ? ' view-only' : ''}`, 60_000);

// redraw moves the probe window's picture on by one frame: damage that does
// not come from robin's input.
export const redraw = () => ssh('robin-wayland redraw');

// leftovers lists agent and gst-launch-1.0 processes, and portal or mutter
// sessions, that outlive a robin session.
export const leftovers = () => ssh('robin-wayland leftovers');

export async function sessionAlive(): Promise<boolean> {
  return (await sshRaw('pgrep -u robin -x mutter')).code === 0;
}

// tokenMode is the permission bits of the portal restore token the agent
// keeps beside itself, or '' when there is none.
export async function tokenMode(): Promise<string> {
  const r = await sshRaw(String.raw`
    for d in ~/.cache/robin /tmp/robin-$(id -u) /run/user/$(id -u)/robin; do
      [ -f "$d/wayland-restore-token" ] && exec stat -c %a "$d/wayland-restore-token"
    done; exit 1`);
  return r.code === 0 ? r.stdout.trim() : '';
}

// playNoise plays seconds of white noise into the session's PipeWire, as a
// program on the desktop would, and resolves when it ends.
export const playNoise = (seconds: number) =>
  ssh(
    `head -c ${seconds * 48000 * 4} /dev/urandom | robin-wayland run paplay --raw --format=s16le --rate=48000 --channels=2`,
    (seconds + 20) * 1000,
  );

// deviceNodes names the PipeWire nodes robin's agent made for the
// browser's microphone and camera, as pw-cli lists them.
export async function deviceNodes(): Promise<{ mic: string; camera: string }> {
  const out = await ssh(`robin-wayland run pw-cli ls Node | grep -o 'robin-\\(mic\\|camera\\)-[0-9]*' | sort -u || true`);
  const names = out.split('\n');
  return { mic: names.find((n) => n.startsWith('robin-mic-')) ?? '', camera: names.find((n) => n.startsWith('robin-camera-')) ?? '' };
}

// recordSource records seconds of a source as a program on the desktop would,
// through pipewire-pulse: 48 kHz mono, base64.
export const recordSource = (source: string, seconds: number) =>
  ssh(
    `robin-wayland run timeout -s INT ${seconds} parec -d ${shellQuote(source)} --raw --format=s16le --rate=48000 --channels=1 --latency-msec=20 | base64 -w0`,
    (seconds + 20) * 1000,
  );

// cameraFrame reads one frame of a video node through pipewiresrc, as
// I420 at the camera's 1280x720, base64.
export const cameraFrame = (node: string) =>
  ssh(
    `robin-wayland run timeout 15 gst-launch-1.0 -q pipewiresrc target-object=${shellQuote(node)} num-buffers=1 ! videoconvert ! ` +
      `video/x-raw,format=I420,width=1280,height=720 ! fdsink fd=1 | base64 -w0`,
    30_000,
  );

export const diagnostics = () =>
  sshRaw('robin-wayland leftovers; ps -u robin -o pid,etime,args; tail -n 20 /tmp/probe.log').then((r) => r.stdout + r.stderr);
