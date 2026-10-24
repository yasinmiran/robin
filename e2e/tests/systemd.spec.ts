import { test, expect, type BrowserContext, type Page } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';
import { Cli, robin } from './helpers/cli';
import { savePageCoverage, startPageCoverage } from './helpers/cover';
import { isNonBlack, sampleFrame } from './helpers/frames';
import * as host from './helpers/host';

// The systemd scenario: the host boots systemd, logind kills what a login
// started once the login ends (KillUserProcesses=yes), and robin lingers.
// The probe sees both, so the CLI has the kept session's daemon started by
// systemd-run, as a unit of robin's user manager. The tests below share one
// session, in order: it must outlive the login that started it, and robin
// ls, a resume and robin end must find it there.

const OUT = path.resolve('out');
const SCREEN = '#screen';

let cli: Cli | undefined;
let context: BrowserContext;
let page: Page;

const state = (p: Page) => p.evaluate(() => (window as any).robin?.state as string | undefined);

// What the first connection found, for the later tests to compare with.
let first: { id: string; pid: number; xvfb: number; unit: string; login: number[]; dir: string } | undefined;

// unitState is what robin's user manager says of a unit.
async function unitState(unit: string): Promise<Record<string, string>> {
  const out = await host.ssh(`systemctl --user show -p LoadState -p ActiveState -p MainPID ${host.shellQuote(unit)}`);
  return Object.fromEntries(out.split('\n').map((l) => l.split('=', 2) as [string, string]));
}

// cgroupOf is the cgroup v2 path of a process, or '' once it is gone.
async function cgroupOf(pid: number): Promise<string> {
  const r = await host.sshRaw(`sed -n 's/^0:://p' /proc/${pid}/cgroup`);
  return r.code === 0 ? r.stdout.trim() : '';
}

const alive = async (pid: number) => (await host.sshRaw(`kill -0 ${pid}`)).code === 0;

// connectPage opens the CLI's page and waits for the desktop to show.
async function connectPage(c: Cli) {
  await page.goto(c.url);
  await expect.poll(() => state(page), { message: 'the page connects', timeout: 60_000 }).toBe('connected');
  await expect
    .poll(async () => isNonBlack(await sampleFrame(page, SCREEN)), { message: 'a non-black frame', timeout: 20_000, intervals: [250] })
    .toBe(true);
}

test.describe.configure({ mode: 'serial' });

test.beforeAll(async ({ browser }) => {
  expect(process.env.SCENARIO, 'SCENARIO').toBe('systemd');
  context = await browser.newContext({
    viewport: { width: 1280, height: 720 },
    recordVideo: { dir: path.join(OUT, 'video'), size: { width: 1280, height: 720 } },
  });
  page = await context.newPage();
  await startPageCoverage(page);
  const consoleLog = fs.createWriteStream(path.join(OUT, 'console.log'), { flags: 'a' });
  page.on('console', (m) => consoleLog.write(`${new Date().toISOString()} ${m.type()} ${m.text()}\n`));
});

test.afterEach(async ({}, testInfo) => {
  if (testInfo.status === testInfo.expectedStatus) return;
  if (cli) await testInfo.attach('cli stderr (tail)', { body: cli.stderrTail(), contentType: 'text/plain' });
  const units = await host.sshRaw('loginctl list-sessions --no-legend; systemctl --user --no-pager list-units --all "robin-*"');
  await testInfo.attach('units', { body: units.stdout + units.stderr, contentType: 'text/plain' });
  await testInfo.attach('host', { body: await host.diagnostics().catch(String), contentType: 'text/plain' });
});

test.afterAll(async () => {
  await cli?.stop();
  await savePageCoverage(page, 'systemd');
  await context?.close();
});

test('host: logind ends what a login started once it logs out, setsid or not', async () => {
  const pid = Number(await host.ssh('setsid sleep 600 </dev/null >/dev/null 2>&1 & echo $!'));
  expect(pid, 'pid of the setsid sleep').toBeGreaterThan(0);
  await expect.poll(() => alive(pid), { message: `pid ${pid} after its login ended`, timeout: 15_000 }).toBe(false);
});

test('kept: the daemon starts as a unit of the user manager, outside the login', async ({}, testInfo) => {
  testInfo.setTimeout(150_000);
  cli = await Cli.start(OUT);
  await connectPage(cli);
  expect(cli.stderrTail(), 'linger is on, so no warning').not.toContain('logind kills user processes');

  const session = await host.session();
  const unit = `robin-${session.id}.service`;
  const u = await unitState(unit);
  testInfo.annotations.push({ type: unit, description: JSON.stringify(u) });
  expect(u.ActiveState, `${unit} ActiveState`).toBe('active');
  expect(Number(u.MainPID), `${unit} MainPID is the daemon`).toBe(session.pid);
  const daemonCgroup = await cgroupOf(session.pid);
  expect(daemonCgroup, "the daemon's cgroup").toMatch(new RegExp(`/user@1000\\.service/(.+/)?${unit.replace('.', '\\.')}$`));

  // The proxy on the CLI's exec channel lives in the login's own scope,
  // with the sshd that serves the login.
  const proxy = Number((await host.ssh(`pgrep -u robin -f '[r]obin-agent-[0-9a-f]* serve' | head -n1`)) || 0);
  expect(proxy, 'the proxy pid').toBeGreaterThan(0);
  const scope = await cgroupOf(proxy);
  expect(scope, "the proxy's cgroup").toMatch(/\/session-[0-9]+\.scope$/);
  const sshd = Number(await host.ssh(`ps -o ppid= -p ${proxy}`));
  expect(await cgroupOf(sshd), "the proxy's sshd").toBe(scope);

  first = { id: session.id, pid: session.pid, xvfb: await host.xvfbPid(), unit, login: [proxy, sshd], dir: await host.runDir() };
  testInfo.annotations.push({ type: 'cgroups', description: `daemon ${daemonCgroup}, proxy ${proxy} and sshd ${sshd} in ${scope}` });
  expect(first.xvfb, 'Xvfb pid').toBeGreaterThan(0);
});

test('kept: the CLI dies, logind ends its login, and the desktop stays in its unit', async ({}, testInfo) => {
  testInfo.setTimeout(90_000);
  expect(first, 'the first test found the session').toBeDefined();
  const f = first!;
  await cli!.kill();
  await expect.poll(() => state(page), { message: 'the page ends with its CLI', timeout: 15_000 }).toBe('ended');

  // The login is over. Its scope's name says nothing here: a rootless
  // container cannot set a login's audit session, so logind names every
  // login after the one podman runs in, and the next login gets the same
  // scope back.
  for (const pid of f.login) {
    await expect.poll(() => alive(pid), { message: `pid ${pid} of the CLI's login`, timeout: 30_000 }).toBe(false);
  }
  await expect
    .poll(async () => (await host.session()).connected, { message: 'session.json says kept', timeout: 30_000 })
    .toBe(false);
  expect((await host.session()).until, 'the keep window has an end').toBeTruthy();
  expect(await alive(f.pid), 'the daemon outlives the login').toBe(true);
  expect(await host.xvfbPid(), 'Xvfb outlives the login').toBe(f.xvfb);
  const u = await unitState(f.unit);
  expect(u.ActiveState, `${f.unit} ActiveState`).toBe('active');
  expect(Number(u.MainPID), `${f.unit} MainPID`).toBe(f.pid);
});

test('kept: robin ls lists it, and a new robin gui resumes it in the same unit', async ({}, testInfo) => {
  testInfo.setTimeout(150_000);
  const f = first!;
  const ls = await robin(['ls', 'robinhost']);
  testInfo.annotations.push({ type: 'robin ls', description: ls.stdout + ls.stderr });
  expect(ls.code, 'robin ls exit code').toBe(0);
  expect(ls.stdout).toContain(f.id);
  expect(ls.stdout).toContain('kept until');

  cli = await Cli.start(OUT);
  const resumed = cli;
  await expect.poll(() => resumed.stderrTail(), { message: 'the CLI said it resumed' }).toContain(`resumed session ${f.id}`);
  await connectPage(cli);
  const session = await host.session();
  expect(session.id, 'session id').toBe(f.id);
  expect(session.pid, 'daemon pid').toBe(f.pid);
  expect(session.connected, 'session.json says connected').toBe(true);
  expect(await host.xvfbPid(), 'Xvfb pid').toBe(f.xvfb);
  expect(Number((await unitState(f.unit)).MainPID), `${f.unit} MainPID`).toBe(f.pid);
});

test('kept: robin end stops the unit and removes the run directory, and the connected CLI hears so', async ({}, testInfo) => {
  testInfo.setTimeout(90_000);
  const f = first!;
  const end = await robin(['end', 'robinhost', f.id]);
  testInfo.annotations.push({ type: 'robin end', description: end.stdout + end.stderr });
  expect(end.code, 'robin end exit code').toBe(0);
  expect(end.stderr).toContain(`ended session ${f.id}`);

  const left = await cli!.waitExit(15_000);
  testInfo.annotations.push({ type: 'CLI exit', description: JSON.stringify(left) });
  expect(left, 'the connected CLI exited once the session ended').not.toBeNull();
  expect(left!.code, 'CLI exit code').toBe(0);
  await expect.poll(() => state(page), { message: 'the page is told', timeout: 15_000 }).toBe('ended');

  await expect.poll(() => host.exists(f.dir), { message: `run dir ${f.dir}`, timeout: 10_000 }).toBe(false);
  await expect.poll(async () => (await unitState(f.unit)).ActiveState, { message: `${f.unit} ActiveState`, timeout: 10_000 }).toBe('inactive');
  await expect.poll(host.leftovers, { message: 'session processes on the host', timeout: 10_000 }).toBe('');
  expect(await host.agentLeftovers(), 'agents and run dirs').toBe('');
  expect(cli!.extraStdout, 'stdout carries only the URL').toEqual([]);
});
