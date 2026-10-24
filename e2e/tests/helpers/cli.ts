import { execFile, spawn, type ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import readline from 'node:readline';

// sshProcesses lists the command lines of the ssh processes the CLI started
// on this machine: a ControlMaster, and the sessions and forwards, which all
// run with -e none. The tests' own ssh to the host runs with neither.
export function sshProcesses(): string[] {
  const found: string[] = [];
  for (const pid of fs.readdirSync('/proc')) {
    if (!/^\d+$/.test(pid)) continue;
    let argv: string[];
    try {
      argv = fs.readFileSync(`/proc/${pid}/cmdline`, 'utf8').split('\0').filter(Boolean);
    } catch {
      continue; // gone meanwhile
    }
    if (path.basename(argv[0] ?? '') !== 'ssh') continue;
    const mine = argv.includes('ControlMaster=yes') || argv.some((a, i) => a === '-e' && argv[i + 1] === 'none');
    if (mine) found.push(argv.join(' '));
  }
  return found;
}

export type Exit = { code: number | null; signal: NodeJS.Signals | null };
export type Stop = Exit & { afterMs: number; killed: boolean };

const URL_LINE = /^http:\/\/127\.0\.0\.1:\d+\/#\S+$/;

// SIGINT goes to the CLI's whole process group, as Ctrl-C in a terminal does,
// so ssh children the CLI did not move out of its group get it too. Set
// ROBIN_SIGINT_PID_ONLY=1 to signal the CLI process alone.
const signalGroup = process.env.ROBIN_SIGINT_PID_ONLY !== '1';

// Cli runs `robin gui --no-browser <host>`, with ROBIN_CLI_ARGS and then
// args: one URL line on stdout, logs on stderr (appended to
// <dir>/cli.stderr.log), SIGINT to end the session.
export class Cli {
  readonly extraStdout: string[] = [];
  private exit?: Exit;
  private stopping?: Promise<Stop>;

  private constructor(
    private readonly proc: ChildProcess,
    private readonly exited: Promise<Exit>,
    readonly url: string,
    readonly stderrPath: string,
  ) {
    exited.then((e) => (this.exit = e));
  }

  static start(dir: string, args: string[] = [], host = 'robinhost', timeoutMs = 90_000): Promise<Cli> {
    const stderrPath = path.join(dir, 'cli.stderr.log');
    const stderr = fs.createWriteStream(stderrPath, { flags: 'a' });
    const stdoutLog = fs.createWriteStream(path.join(dir, 'cli.stdout.log'), { flags: 'a' });
    // Verbose, so the saved stderr carries the agent's log (ICE states, resizes).
    const argv = ['gui', '--no-browser', '-v', ...(process.env.ROBIN_CLI_ARGS ?? '').split(' ').filter(Boolean), ...args, host];
    const header = `--- ${new Date().toISOString()} robin ${argv.join(' ')}\n`;
    stderr.write(header);
    stdoutLog.write(header);

    const proc = spawn('robin', argv, {
      stdio: ['ignore', 'pipe', 'pipe'],
      detached: true, // own process group, like a foreground job
    });
    proc.stderr!.pipe(stderr);
    const exited = new Promise<Exit>((resolve) => proc.on('exit', (code, signal) => resolve({ code, signal })));

    return new Promise<Cli>((resolve, reject) => {
      let cli: Cli | undefined;
      const fail = (err: Error) => {
        clearTimeout(timer);
        killGroup(proc, 'SIGKILL');
        reject(err);
      };
      const timer = setTimeout(() => fail(new Error(`no URL on stdout within ${timeoutMs} ms; see ${stderrPath}`)), timeoutMs);
      proc.on('error', fail);
      exited.then((e) => {
        if (!cli) fail(new Error(`CLI exited before printing a URL (code ${e.code}, signal ${e.signal}); see ${stderrPath}`));
      });
      readline.createInterface({ input: proc.stdout! }).on('line', (line) => {
        stdoutLog.write(line + '\n');
        if (cli) {
          cli.extraStdout.push(line);
          return;
        }
        if (!URL_LINE.test(line)) {
          fail(new Error(`first stdout line is not http://127.0.0.1:PORT/#TOKEN: ${JSON.stringify(line)}`));
          return;
        }
        clearTimeout(timer);
        cli = new Cli(proc, exited, line, stderrPath);
        resolve(cli);
      });
    });
  }

  get running() {
    return !this.exit;
  }

  // kill ends the CLI the way a crash or a closed laptop lid does: SIGKILL
  // to its process group, ssh master included, with no chance to say bye.
  kill(): Promise<Exit> {
    killGroup(this.proc, 'SIGKILL');
    return this.exited;
  }

  // waitExit waits up to timeoutMs for the CLI to exit on its own.
  async waitExit(timeoutMs = 15_000): Promise<Exit | null> {
    const timedOut = new Promise<null>((r) => setTimeout(() => r(null), timeoutMs));
    return Promise.race([this.exited, timedOut]);
  }

  // stop sends SIGINT and waits up to timeoutMs for the CLI to exit, then
  // SIGKILLs its process group. Safe to call more than once.
  stop(timeoutMs = 10_000): Promise<Stop> {
    this.stopping ??= (async () => {
      const t0 = Date.now();
      if (this.exit) return { ...this.exit, afterMs: 0, killed: false };
      if (signalGroup) killGroup(this.proc, 'SIGINT');
      else this.proc.kill('SIGINT');
      const timedOut = new Promise<null>((r) => setTimeout(() => r(null), timeoutMs));
      const exit = await Promise.race([this.exited, timedOut]);
      if (exit) return { ...exit, afterMs: Date.now() - t0, killed: false };
      killGroup(this.proc, 'SIGKILL');
      return { ...(await this.exited), afterMs: Date.now() - t0, killed: true };
    })();
    return this.stopping;
  }

  stderrTail(lines = 60): string {
    try {
      return fs.readFileSync(this.stderrPath, 'utf8').split('\n').slice(-lines).join('\n');
    } catch {
      return '';
    }
  }
}

// robin runs another robin command, such as ls or end, to completion.
export function robin(args: string[], timeoutMs = 60_000): Promise<{ code: number; stdout: string; stderr: string }> {
  return new Promise((resolve) => {
    execFile('robin', args, { timeout: timeoutMs }, (err, stdout, stderr) => {
      const code = err ? (typeof err.code === 'number' ? err.code : 255) : 0;
      resolve({ code, stdout: String(stdout), stderr: String(stderr) });
    });
  });
}

function killGroup(proc: ChildProcess, signal: NodeJS.Signals) {
  try {
    process.kill(-proc.pid!, signal);
  } catch {
    // already gone
  }
}
