import { test, expect, type BrowserContext, type Page } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';
import { Cli, robin } from './helpers/cli';
import { pagePoint } from './helpers/coords';
import { savePageCoverage, startPageCoverage } from './helpers/cover';
import { isNonBlack, sampleFrame } from './helpers/frames';
import * as host from './helpers/host';

// The share scenario: robin's spawned desktop shared with alice, the host's
// second account. The owner's CLI invites alice with --share, alice's own
// CLI joins with the code over her own login, and two pages, the owner's
// and the guest's, watch the one stream. The tests run in order against
// one session.

const OUT = path.resolve('out');
const SCREEN = '#screen';
const GUEST = 'alice';
const INVITE_LINE = /robin: alice may join once, until \S+, with: robin gui --join (\S+) alice@robinhost/;

const robinOf = (p: Page) => p.evaluate(() => (window as any).robin?.state as string | undefined);
const share = (p: Page) => p.evaluate(() => (window as any).robin.share.state);

let owner: Cli | undefined;
let guest: Cli | undefined;
let ownerContext: BrowserContext;
let guestContext: BrowserContext;
let ownerPage: Page;
let guestPage: Page;
let code = '';

async function openPage(context: BrowserContext, name: string): Promise<Page> {
  const p = await context.newPage();
  await startPageCoverage(p);
  const log = fs.createWriteStream(path.join(OUT, `console-${name}.log`), { flags: 'a' });
  p.on('console', (m) => log.write(`${new Date().toISOString()} ${m.type()} ${m.text()}\n`));
  p.on('pageerror', (e) => log.write(`${new Date().toISOString()} pageerror ${e.stack ?? e}\n`));
  return p;
}

async function connected(p: Page, what: string) {
  await expect.poll(() => p.evaluate(() => typeof (window as any).robin), { message: `${what}: window.robin`, timeout: 15_000 }).toBe('object');
  await expect.poll(() => robinOf(p), { message: `${what} is connected`, timeout: 20_000 }).toBe('connected');
  await expect
    .poll(async () => isNonBlack(await sampleFrame(p, SCREEN)), { message: `${what} shows the desktop`, timeout: 20_000 })
    .toBe(true);
}

// typeOnGuest clicks the xterm through the guest's picture and types a
// command that writes word to file, and reports whether file came to hold
// it within waitMs.
async function typeOnGuest(file: string, word: string, waitMs: number): Promise<boolean> {
  await host.ssh(`rm -f ${file}`);
  const t = await host.terminal();
  expect(t, 'an xterm is mapped').not.toBeNull();
  const centre = await pagePoint(guestPage, SCREEN, t!.x + t!.width / 2, t!.y + t!.height / 2);
  await guestPage.mouse.click(centre.x, centre.y);
  // A click without control must not stand in for one with it.
  await host.focusTerminal();
  await guestPage.keyboard.type(`echo ${word} > ${file}`, { delay: 15 });
  await guestPage.keyboard.press('Enter');
  const deadline = Date.now() + waitMs;
  while (Date.now() < deadline) {
    if ((await host.sshRaw(`cat ${file}`)).stdout.trim() === word) return true;
    await new Promise((r) => setTimeout(r, 200));
  }
  return false;
}

// clickGuestAction clicks the owner's button for the guest's action on the
// owner's page, as the user would, wherever the status tab is. Remove
// takes a second click to confirm.
const clickGuestAction = (action: string) =>
  ownerPage.evaluate((a) => {
    const find = () => document.querySelector<HTMLButtonElement>(`.share-guest button[data-action="${a}"]`);
    if (!find()) throw new Error(`no ${a} button`);
    find()!.click();
    if (a === 'remove') find()!.click();
  }, action);

const text = (p: Page, selector: string) => p.$eval(selector, (el) => el.textContent);

// guestProcesses lists what alice runs on the host besides this check.
async function guestProcesses(): Promise<string> {
  const r = await host.sshRaw(`pgrep -u ${GUEST} -fa '[r]obin-agent|[s]shd'`);
  return r.stdout.trim();
}

test.beforeAll(async ({ browser }, testInfo) => {
  testInfo.setTimeout(150_000);
  ownerContext = await browser.newContext({ viewport: { width: 1280, height: 720 } });
  guestContext = await browser.newContext({ viewport: { width: 1280, height: 720 } });
  ownerPage = await openPage(ownerContext, 'owner');
  guestPage = await openPage(guestContext, 'guest');
  owner = await Cli.start(OUT, ['--share', GUEST]);
  await ownerPage.goto(owner.url);
});

test.afterAll(async () => {
  await guest?.stop();
  await owner?.stop();
  await savePageCoverage(ownerPage, 'share-owner');
  await savePageCoverage(guestPage, 'share-guest');
  await ownerContext?.close();
  await guestContext?.close();
});

test('the owner shares the desktop: the CLI prints the join command and the page lists it', async () => {
  await connected(ownerPage, 'the owner page');
  const m = INVITE_LINE.exec(owner!.said());
  expect(m, `the invite line in:\n${owner!.said()}`).not.toBeNull();
  code = m![1];
  await expect
    .poll(async () => (await share(ownerPage)).guests?.invitations?.map((i: any) => `${i.user} ${i.code}`), { message: 'the page lists the invitation' })
    .toEqual([`${GUEST} ${code}`]);
  const shown = await ownerPage.$eval('.share-command', (el) => el.textContent);
  expect(shown).toBe(`robin gui --join ${code} ${GUEST}@robinhost`);
});

test('the code admits nobody but the account it was made for', async () => {
  const r = await robin(['gui', '--no-browser', '--join', code, 'robinhost'], 60_000);
  expect(r.code, `robin's own join:\n${r.stderr}`).not.toBe(0);
  expect(r.stdout).toBe('');
  // Refused before a byte was read, the invitation is still alice's.
  expect((await share(ownerPage)).guests.invitations.map((i: any) => i.user)).toEqual([GUEST]);
});

test("the guest joins over its own login and watches the owner's stream", async () => {
  guest = await Cli.start(OUT, ['--join', code], `${GUEST}@robinhost`);
  await expect.poll(() => guest!.said(), { message: 'the guest CLI says what it joined' }).toContain("joined robin's desktop :");
  await guestPage.goto(guest.url);
  await connected(guestPage, 'the guest page');
  const s = await share(guestPage);
  expect(s.share).toEqual({ owner: 'robin' });
  expect(await guestPage.$eval('.share-status', (el) => el.textContent)).toBe('View only until robin gives you control');
  expect(await text(guestPage, '#state-label'), "the guest's row says whose desktop it watches").toBe('Watching robin’s desktop');
  await expect
    .poll(async () => (await share(ownerPage)).guests?.list?.map((g: any) => `${g.user} ${Boolean(g.control)}`), { message: 'the owner page lists the guest' })
    .toEqual([`${GUEST} false`]);
  expect(await text(ownerPage, '#guests .label'), "the owner's row says who watches").toBe(`${GUEST} watching`);
  expect((await share(ownerPage)).guests.invitations, 'the spent invitation is gone').toEqual([]);
  expect(await guestProcesses(), "alice's agent runs as alice").toMatch(/robin-agent-\S+ join -session \S+ -owner 1000/);
  // Both pages keep decoding.
  const decoded = (p: Page) => p.evaluate(() => (window as any).robin.stats().then((s: any) => s.framesDecoded as number));
  const before = [await decoded(ownerPage), await decoded(guestPage)];
  await host.retitleTerminal();
  await expect.poll(async () => (await decoded(ownerPage)) > before[0], { message: 'the owner page decodes on' }).toBe(true);
  await expect.poll(async () => (await decoded(guestPage)) > before[1], { message: 'the guest page decodes on' }).toBe(true);
});

test('a guest without control types nothing into the desktop', async () => {
  expect(await typeOnGuest('/tmp/robin-e2e-share-0', 'nocontrol', 3_000)).toBe(false);
});

test('given control, the guest types; taken back, it stops at once', async () => {
  await clickGuestAction('grant');
  await expect.poll(async () => Boolean((await share(guestPage)).share.control), { message: 'the guest page has control' }).toBe(true);
  expect(await guestPage.$eval('.share-status', (el) => el.textContent)).toBe('You have control');
  expect(await typeOnGuest('/tmp/robin-e2e-share', 'robin-e2e-share', 10_000)).toBe(true);
  expect(await host.ssh('stat -c %U /tmp/robin-e2e-share'), "the guest typed into robin's own xterm").toBe('robin');

  await clickGuestAction('revoke');
  await expect.poll(async () => Boolean((await share(guestPage)).share.control), { message: 'the guest page lost control' }).toBe(false);
  expect(await typeOnGuest('/tmp/robin-e2e-share-1', 'revoked', 3_000)).toBe(false);
});

test("reloading the owner's page takes control from the guest, who keeps watching", async () => {
  await clickGuestAction('grant');
  await expect.poll(async () => Boolean((await share(guestPage)).share.control), { message: 'the guest page has control' }).toBe(true);
  await ownerPage.reload();
  await expect.poll(async () => Boolean((await share(guestPage)).share.control), { message: 'the guest lost control with the page' }).toBe(false);
  await connected(ownerPage, 'the reloaded owner page');
  await expect
    .poll(async () => (await share(ownerPage)).guests?.list?.map((g: any) => `${g.user} ${Boolean(g.control)}`), { message: 'the reloaded page lists the guest' })
    .toEqual([`${GUEST} false`]);
  expect(await robinOf(guestPage)).toBe('connected');
});

test('the owner page invites by name, and says why it cannot', async () => {
  await ownerPage.evaluate(() => (window as any).robin.share.invite('nosuchuser'));
  await expect.poll(async () => (await share(ownerPage)).error, { message: 'the page says why' }).toContain('no user nosuchuser');
  await ownerPage.evaluate(() => (window as any).robin.share.invite('robin'));
  await expect.poll(async () => (await share(ownerPage)).error, { message: 'the owner cannot invite itself' }).toContain('own user');
  await ownerPage.evaluate(() => (window as any).robin.share.invite('alice'));
  await expect
    .poll(async () => (await share(ownerPage)).guests.invitations.map((i: any) => i.user), { message: 'a second invitation' })
    .toEqual([GUEST]);
});

test("removing the guest closes its page at once and ends its CLI; the owner's goes on", async () => {
  const t0 = Date.now();
  await clickGuestAction('remove');
  await expect.poll(() => robinOf(guestPage), { message: 'the guest page ended', timeout: 5_000 }).toBe('ended');
  const closedMs = Date.now() - t0;
  test.info().annotations.push({ type: 'guest page closed after', description: `${closedMs} ms` });
  expect(closedMs).toBeLessThan(3_000);
  expect(await guestPage.$eval('.notice', (el) => el.textContent)).toContain('The owner removed you from the session.');
  const exit = await guest!.waitExit(15_000);
  expect(exit, `the guest CLI exits:\n${guest!.said()}`).toEqual({ code: 0, signal: null });
  expect(guest!.said()).toContain('the owner removed you from the session');
  await expect.poll(guestProcesses, { message: 'nothing of alice runs on the host', timeout: 15_000 }).toBe('');
  expect(await robinOf(ownerPage)).toBe('connected');
  expect((await share(ownerPage)).guests.list).toEqual([]);
});

test('the owner ending the session leaves nothing behind', async () => {
  const stop = await owner!.stop();
  expect(stop.code, owner!.said()).toBe(0);
  await expect.poll(host.agentLeftovers, { message: 'no agent or run dir is left', timeout: 15_000 }).toBe('');
  await expect.poll(guestProcesses, { message: 'nothing of alice runs on the host', timeout: 15_000 }).toBe('');
});
