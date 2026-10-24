import { execFile } from 'node:child_process';

// Host-side checks run over the test's own ssh connections, never the CLI's
// master, so a broken session cannot hide behind (or break) the checks.
const SSH = ['-o', 'ControlMaster=no', '-o', 'ControlPath=none', '-o', 'ConnectTimeout=10'];

export type Result = { code: number; stdout: string; stderr: string };

export function sshRaw(command: string, timeoutMs = 20_000): Promise<Result> {
  return new Promise((resolve) => {
    execFile('ssh', [...SSH, 'robinhost', command], { timeout: timeoutMs, maxBuffer: 16 << 20 }, (err, stdout, stderr) => {
      const code = err ? (typeof err.code === 'number' ? err.code : 255) : 0;
      resolve({ code, stdout: String(stdout), stderr: String(stderr) });
    });
  });
}

export async function ssh(command: string, timeoutMs?: number): Promise<string> {
  const r = await sshRaw(command, timeoutMs);
  if (r.code !== 0) throw new Error(`ssh robinhost ${command}: exit ${r.code}: ${r.stderr.trim()}`);
  return r.stdout.trim();
}

export const shellQuote = (s: string) => `'${s.replace(/'/g, `'\\''`)}'`;

// onDisplay runs a command with DISPLAY and XAUTHORITY taken from the agent's
// run dir (see host/robin-x).
export const onDisplay = (command: string) => ssh(`robin-x ${command}`);

export const runDir = () => ssh('robin-rundir');

// fileSha256 is the SHA-256 of a file on the host, or '' while there is
// none. path may start with ~ and is otherwise quoted.
export async function fileSha256(path: string): Promise<string> {
  const p = path.startsWith('~/') ? `~/${shellQuote(path.slice(2))}` : shellQuote(path);
  const r = await sshRaw(`sha256sum ${p}`);
  return r.code === 0 ? r.stdout.split(' ')[0] : '';
}

export async function exists(path: string): Promise<boolean> {
  return (await sshRaw(`test -e ${shellQuote(path)}`)).code === 0;
}

function shellVars(out: string): Record<string, number> {
  const vars: Record<string, number> = {};
  for (const line of out.split('\n')) {
    const m = /^(\w+)=(-?\d+)$/.exec(line.trim());
    if (m) vars[m[1]] = Number(m[2]);
  }
  return vars;
}

export async function displayGeometry(): Promise<{ width: number; height: number }> {
  const [width, height] = (await onDisplay('xdotool getdisplaygeometry')).split(/\s+/).map(Number);
  return { width, height };
}

export async function mouseLocation(): Promise<{ x: number; y: number }> {
  const v = shellVars(await onDisplay('xdotool getmouselocation --shell'));
  return { x: v.X, y: v.Y };
}

export type Rect = { x: number; y: number; width: number; height: number };

const FIRST_XTERM = 'w=$(xdotool search --onlyvisible --class xterm | head -n1); [ -n "$w" ]';

// terminal returns the first visible xterm's geometry in display pixels, or
// null while there is none.
export async function terminal(): Promise<Rect | null> {
  const r = await sshRaw(`robin-x sh -c '${FIRST_XTERM} && xdotool getwindowgeometry --shell "$w"'`);
  if (r.code !== 0) return null;
  const v = shellVars(r.stdout);
  return { x: v.X, y: v.Y, width: v.WIDTH, height: v.HEIGHT };
}

// focusTerminal gives the xterm the keyboard focus from the host side, which
// a click through the picture does otherwise.
export async function focusTerminal(): Promise<void> {
  await ssh(`robin-x sh -c '${FIRST_XTERM} && xdotool windowfocus --sync "$w"'`);
}

// retitleTerminal changes the xterm's title, so the window manager redraws the
// title bar: damage that does not go through robin's input path.
export async function retitleTerminal(): Promise<void> {
  await ssh(`robin-x sh -c '${FIRST_XTERM} && xdotool set_window --name "robin-e2e-$(date +%s%N)" "$w"'`);
}

export async function clipboard(): Promise<string> {
  const r = await sshRaw('robin-x xclip -o -selection clipboard');
  return r.code === 0 ? r.stdout : '';
}

// setClipboard leaves a forked xclip owning the selection; its output goes to
// /dev/null so the ssh channel can close.
export async function setClipboard(text: string): Promise<void> {
  await ssh(`printf %s ${shellQuote(text)} | robin-x xclip -selection clipboard -i >/dev/null 2>&1`);
}

// leftovers lists session processes still running as robin. The bracketed
// patterns keep pgrep from matching the remote shell that carries them.
export async function leftovers(): Promise<string> {
  const r = await sshRaw(`pgrep -u robin -fa '[X]vfb|[r]obin-agent|[o]penbox|[x]term|[p]ulseaudio'`);
  if (r.code > 1) throw new Error(`pgrep: exit ${r.code}: ${r.stderr.trim()}`);
  return r.stdout.trim();
}

export const blockUdp = () => ssh('robin-block-udp');

// liveState describes robin's own desktop (host/robin-live): its processes,
// autorepeat, keymaps, gamma and sinks. liveBefore is the same, saved before the CLI ran.
export const liveState = () => ssh('robin-live state');
export const liveBefore = () => ssh('robin-live before');

// agentLeftovers lists agents still running as robin, and run dirs they left.
export async function agentLeftovers(): Promise<string> {
  const r = await sshRaw(`pgrep -u robin -fa '[r]obin-agent'; find ~/.cache/robin/run -mindepth 1 -maxdepth 1 2>/dev/null`);
  return r.stdout.trim();
}

// Session is what the daemon records in its run dir (protocol.Session).
export type Session = {
  id: string;
  pid: number;
  agent: string;
  mode: string;
  display: string;
  keep: number;
  connected: boolean;
  until?: string;
};

// session reads the kept session's record from the agent's run dir.
export async function session(): Promise<Session> {
  return JSON.parse(await ssh('cat "$(robin-rundir)/session.json"')) as Session;
}

// xvfbPid is the pid of the session's Xvfb, the one whose cookie file is in
// the agent's run dir, or 0.
export async function xvfbPid(): Promise<number> {
  const r = await sshRaw(`pgrep -u robin -f "[X]vfb .*-auth $(robin-rundir)/Xauthority"`);
  return Number(r.stdout.trim().split('\n')[0]) || 0;
}

// terminalWindow is the X window id of the first visible xterm, or ''.
export async function terminalWindow(): Promise<string> {
  const r = await sshRaw(`robin-x sh -c 'xdotool search --onlyvisible --class xterm | head -n1'`);
  return r.code === 0 ? r.stdout.trim() : '';
}

// cutSSH kills every sshd process the robin user owns: the post-auth half of
// each connection, which drops them all at once, the CLI's master included.
// The connection carrying the kill goes with them, so its exit code says
// nothing.
export async function cutSSH(): Promise<void> {
  await sshRaw('pkill -u robin -x sshd');
}

// cutSSHFor drops every connection as cutSSH does, and refuses new ones
// for seconds, so the CLI's redials fail fast until then: a window a test
// can act in while the page reconnects. The rule takes itself away; host
// checks fail meanwhile too.
export async function cutSSHFor(seconds: number): Promise<void> {
  const rule = 'INPUT -p tcp --dport 22 --syn -j REJECT --reject-with tcp-reset';
  await sshRaw(
    `sudo -n iptables -w -I ${rule} && setsid -f sh -c 'sleep ${seconds}; sudo -n iptables -w -D ${rule}' </dev/null >/dev/null 2>&1; pkill -u robin -x sshd`,
  );
}

// keptSession reads the record of the one session the agent keeps, in its
// run dir under ~/.cache/robin/run; an attached session has no display
// file for robin-rundir to find it by.
export async function keptSession(): Promise<Session> {
  return JSON.parse(await ssh('cat ~/.cache/robin/run/*/session.json')) as Session;
}

// roomBrightness is the brightness of robin's own desktop's screen gamma as
// xrandr reads it (host/robin-room): 0 is black, about 1 is lit.
export const roomBrightness = async () => Number(await ssh('robin-room brightness'));

// roomLoudness is the RMS of a second of what robin's own desktop's
// speakers play.
export const roomLoudness = async () => Number(await ssh('robin-room loudness'));

// playNoise plays seconds of white noise into the session's sound server,
// as a program on the remote desktop would, and resolves when it ends.
export const playNoise = (seconds: number) =>
  ssh(`head -c ${seconds * 48000 * 4} /dev/urandom | robin-x paplay --raw --format=s16le --rate=48000 --channels=2`, (seconds + 20) * 1000);

// recordMicrophone records seconds of the session's default source, the
// browser's microphone on a spawned desktop, as a program there would: 48 kHz
// mono, base64.
export const recordMicrophone = (seconds: number) =>
  ssh(
    `robin-x timeout -s INT ${seconds} parec -d @DEFAULT_SOURCE@ --raw --format=s16le --rate=48000 --channels=1 --latency-msec=20 | base64 -w0`,
    (seconds + 20) * 1000,
  );

// diagnostics describes the host side of a session for a failure report: the
// run dir, the agent's processes and what is mapped on its display.
export async function diagnostics(): Promise<string> {
  const r = await sshRaw(String.raw`
    d=$(robin-rundir 2>&1); echo "run dir: $d"; ls -la "$d" 2>&1
    echo; echo "## processes"; ps -u robin -o pid,ppid,etime,args
    echo; echo "## display"; robin-x xdotool getdisplaygeometry 2>&1
    robin-x xwininfo -root -tree 2>&1 | head -n 40
    for w in $(robin-x xdotool search --class xterm 2>/dev/null); do
      echo "xterm $w: $(robin-x xwininfo -id "$w" | grep -E 'Map State|geometry')"
    done`);
  return r.stdout + r.stderr;
}
