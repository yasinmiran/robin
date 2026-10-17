import { execFile } from 'node:child_process';

// Host-side checks run over the test's own ssh connections, never the CLI's
// master, so a broken session cannot hide behind (or break) the checks.
const SSH = ['-o', 'ControlMaster=no', '-o', 'ControlPath=none', '-o', 'ConnectTimeout=10'];

export type Result = { code: number; stdout: string; stderr: string };

export function sshRaw(command: string, timeoutMs = 20_000): Promise<Result> {
  return new Promise((resolve) => {
    execFile('ssh', [...SSH, 'rjhost', command], { timeout: timeoutMs, maxBuffer: 16 << 20 }, (err, stdout, stderr) => {
      const code = err ? (typeof err.code === 'number' ? err.code : 255) : 0;
      resolve({ code, stdout: String(stdout), stderr: String(stderr) });
    });
  });
}

export async function ssh(command: string, timeoutMs?: number): Promise<string> {
  const r = await sshRaw(command, timeoutMs);
  if (r.code !== 0) throw new Error(`ssh rjhost ${command}: exit ${r.code}: ${r.stderr.trim()}`);
  return r.stdout.trim();
}

export const shellQuote = (s: string) => `'${s.replace(/'/g, `'\\''`)}'`;

// onDisplay runs a command with DISPLAY and XAUTHORITY taken from the agent's
// run dir (see host/rj-x).
export const onDisplay = (command: string) => ssh(`rj-x ${command}`);

export const runDir = () => ssh('rj-rundir');

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
  const r = await sshRaw(`rj-x sh -c '${FIRST_XTERM} && xdotool getwindowgeometry --shell "$w"'`);
  if (r.code !== 0) return null;
  const v = shellVars(r.stdout);
  return { x: v.X, y: v.Y, width: v.WIDTH, height: v.HEIGHT };
}

// retitleTerminal changes the xterm's title, so the window manager redraws the
// title bar: damage that does not go through remotejump's input path.
export async function retitleTerminal(): Promise<void> {
  await ssh(`rj-x sh -c '${FIRST_XTERM} && xdotool set_window --name "rj-e2e-$(date +%s%N)" "$w"'`);
}

export async function clipboard(): Promise<string> {
  const r = await sshRaw('rj-x xclip -o -selection clipboard');
  return r.code === 0 ? r.stdout : '';
}

// setClipboard leaves a forked xclip owning the selection; its output goes to
// /dev/null so the ssh channel can close.
export async function setClipboard(text: string): Promise<void> {
  await ssh(`printf %s ${shellQuote(text)} | rj-x xclip -selection clipboard -i >/dev/null 2>&1`);
}

// leftovers lists session processes still running as rj. The bracketed
// patterns keep pgrep from matching the remote shell that carries them.
export async function leftovers(): Promise<string> {
  const r = await sshRaw(`pgrep -u rj -fa '[X]vfb|[r]emotejump-agent|[o]penbox|[x]term'`);
  if (r.code > 1) throw new Error(`pgrep: exit ${r.code}: ${r.stderr.trim()}`);
  return r.stdout.trim();
}

export const blockUdp = () => ssh('rj-block-udp');

// diagnostics describes the host side of a session for a failure report: the
// run dir, the agent's processes and what is mapped on its display.
export async function diagnostics(): Promise<string> {
  const r = await sshRaw(String.raw`
    d=$(rj-rundir 2>&1); echo "run dir: $d"; ls -la "$d" 2>&1
    echo; echo "## processes"; ps -u rj -o pid,ppid,etime,args
    echo; echo "## display"; rj-x xdotool getdisplaygeometry 2>&1
    rj-x xwininfo -root -tree 2>&1 | head -n 40
    for w in $(rj-x xdotool search --class xterm 2>/dev/null); do
      echo "xterm $w: $(rj-x xwininfo -id "$w" | grep -E 'Map State|geometry')"
    done`);
  return r.stdout + r.stderr;
}
