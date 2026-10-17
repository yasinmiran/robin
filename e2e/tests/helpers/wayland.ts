import { ssh, sshRaw } from './host';

// Host-side checks for the wayland scenario, through wlhost/rj-wayland on the
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
  const out = await ssh('rj-wayland events');
  return out
    .split('\n')
    .filter((l) => l.startsWith('{'))
    .map((l) => JSON.parse(l) as ProbeEvent);
}

// consent answers the portal's dialog as the person at the host's screen.
export const consent = (viewOnly = false) => ssh(`rj-wayland consent${viewOnly ? ' view-only' : ''}`, 60_000);

// redraw moves the probe window's picture on by one frame: damage that does
// not come from remotejump's input.
export const redraw = () => ssh('rj-wayland redraw');

// leftovers lists agent and gst-launch-1.0 processes, and portal or mutter
// sessions, that outlive a remotejump session.
export const leftovers = () => ssh('rj-wayland leftovers');

export async function sessionAlive(): Promise<boolean> {
  return (await sshRaw('pgrep -u rj -x mutter')).code === 0;
}

// tokenMode is the permission bits of the portal restore token the agent
// keeps beside itself, or '' when there is none.
export async function tokenMode(): Promise<string> {
  const r = await sshRaw(String.raw`
    for d in ~/.cache/remotejump /tmp/remotejump-$(id -u) /run/user/$(id -u)/remotejump; do
      [ -f "$d/wayland-restore-token" ] && exec stat -c %a "$d/wayland-restore-token"
    done; exit 1`);
  return r.code === 0 ? r.stdout.trim() : '';
}

// playNoise plays seconds of white noise into the session's PipeWire, as a
// program on the desktop would, and resolves when it ends.
export const playNoise = (seconds: number) =>
  ssh(
    `head -c ${seconds * 48000 * 4} /dev/urandom | rj-wayland run paplay --raw --format=s16le --rate=48000 --channels=2`,
    (seconds + 20) * 1000,
  );

export const diagnostics = () =>
  sshRaw('rj-wayland leftovers; ps -u rj -o pid,etime,args; tail -n 20 /tmp/probe.log').then((r) => r.stdout + r.stderr);
