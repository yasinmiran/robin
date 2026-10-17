import { spawn, type ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import readline from 'node:readline';

export type Exit = { code: number | null; signal: NodeJS.Signals | null };
export type Stop = Exit & { afterMs: number; killed: boolean };

const URL_LINE = /^http:\/\/127\.0\.0\.1:\d+\/#\S+$/;

// SIGINT goes to the CLI's whole process group, as Ctrl-C in a terminal does,
// so ssh children the CLI did not move out of its group get it too. Set
// RJ_SIGINT_PID_ONLY=1 to signal the CLI process alone.
const signalGroup = process.env.RJ_SIGINT_PID_ONLY !== '1';

// Cli runs `remotejump gui --no-browser <host>`: one URL line on stdout, logs on
// stderr (appended to <dir>/cli.stderr.log), SIGINT to end the session.
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

  static start(dir: string, host = 'rjhost', timeoutMs = 90_000): Promise<Cli> {
    const stderrPath = path.join(dir, 'cli.stderr.log');
    const stderr = fs.createWriteStream(stderrPath, { flags: 'a' });
    const stdoutLog = fs.createWriteStream(path.join(dir, 'cli.stdout.log'), { flags: 'a' });
    // Verbose, so the saved stderr carries the agent's log (ICE states, resizes).
    const args = ['gui', '--no-browser', '-v', ...(process.env.RJ_CLI_ARGS ?? '').split(' ').filter(Boolean), host];
    const header = `--- ${new Date().toISOString()} remotejump ${args.join(' ')}\n`;
    stderr.write(header);
    stdoutLog.write(header);

    const proc = spawn('remotejump', args, {
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

function killGroup(proc: ChildProcess, signal: NodeJS.Signals) {
  try {
    process.kill(-proc.pid!, signal);
  } catch {
    // already gone
  }
}
