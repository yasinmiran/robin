import { type Locator, type Page, expect, test } from "@playwright/test";

import { siteAddress } from "../../scripts/site-url.ts";
import { joinBase } from "../../src/lib/base.ts";

const { base } = siteAddress(process.env.SITE_URL);

/** at is where a site path is served: /install is /robin/install in the Pages build. */
const at = (path: string) => joinBase(base, path);

/** failOnErrors fails the test on any uncaught error or console error. */
function failOnErrors(page: Page) {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (message) => {
    if (message.type() === "error") errors.push(message.text());
  });
  return () => expect(errors).toEqual([]);
}

/**
 * hydrated waits until the island holding the control has hydrated: Astro
 * drops the ssr attribute then. Before that the control is inert HTML.
 */
async function hydrated(page: Page, control: Locator) {
  const island = page.locator("astro-island").filter({ has: control });
  await expect(island.first()).not.toHaveAttribute("ssr");
}

async function horizontalOverflow(page: Page) {
  return page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
}

test("the homepage shows what Robin is", async ({ page }) => {
  const noErrors = failOnErrors(page);
  await page.goto(at("/"));
  await expect(page).toHaveTitle(/^Robin/);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(
    "Robin: Your eyes and hands on any Linux box you can ssh to.",
  );
  const stage = page.getByRole("figure", { name: /^Your terminal runs robin gui alice@build-box/ });
  await expect(stage.getByRole("img", { name: /A Robin session in the browser/ })).toBeVisible();
  await expect(page.getByText("Connected", { exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: "Install" })).toHaveAttribute("href", at("/install"));
  await expect(page.locator('link[rel="icon"]')).toHaveAttribute("href", at("/favicon.svg"));
  noErrors();
});

for (const [path, title, heading] of [
  ["/install", "Install and build", "What the host needs"],
  ["/using", "Using Robin", "The page"],
  ["/flags", "Flags", "Picture"],
] as const) {
  test(`${path} reads as a docs page`, async ({ page }) => {
    const noErrors = failOnErrors(page);
    await page.goto(at(path));
    await expect(page).toHaveTitle(`${title} | Robin`);
    await expect(page.getByRole("heading", { level: 1 })).toHaveText(title);
    await expect(page.locator('meta[name="description"]')).toHaveAttribute("content", /.+/);

    const sidebar = page.getByRole("navigation", { name: "Docs" });
    await expect(sidebar.getByRole("link", { name: title })).toHaveAttribute("aria-current", "page");
    const toc = page.getByRole("complementary").getByRole("navigation", { name: "On this page" });
    await expect(toc.getByRole("link", { name: heading })).toBeVisible();
    await expect(page.getByRole("button", { name: /^Copy/ }).first()).toBeVisible();
    await expect(page.getByRole("navigation", { name: "Previous and next" })).toBeVisible();
    noErrors();
  });
}

test("the flags page has its own header link", async ({ page }) => {
  await page.goto(at("/flags"));
  const header = page.getByRole("navigation", { name: "Main" });
  await expect(header.getByRole("link", { name: "Flags" })).toHaveAttribute("aria-current", "true");
  await expect(header.getByRole("link", { name: "Guide" })).not.toHaveAttribute("aria-current");
});

test("each flag has an anchor", async ({ page }) => {
  await page.goto(at("/flags#flag-max-size"));
  await expect(page.locator("#flag-max-size")).toBeInViewport();
  await expect(page.locator("#flag-max-size")).toContainText("default 2560x1440");
});

test("flag defaults read without stray spaces", async ({ page }) => {
  await page.goto(at("/flags"));
  // Raw textContent: toHaveText would collapse the whitespace this looks for.
  const defaults = await page.locator(".flag-default").evaluateAll((spans) => spans.map((span) => span.textContent));
  expect(defaults.length).toBeGreaterThan(10);
  expect(defaults).toContain("default 2560x1440, 1920x1080 on hosts with 4 cores or fewer, 1280x720 with 2 or fewer");
  for (const text of defaults) expect(text).not.toMatch(/^\s|\s$|\s[,.;)]|\s{2}/);
});

test("the theme toggle switches and remembers the theme", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto(at("/install"));
  const html = page.locator("html");
  await expect(html).not.toHaveClass(/\bdark\b/);

  await page.getByRole("button", { name: "Switch to dark theme" }).click();
  await expect(html).toHaveClass(/\bdark\b/);

  await page.reload();
  await expect(html).toHaveClass(/\bdark\b/);
  await page.getByRole("button", { name: "Switch to light theme" }).click();
  await expect(html).not.toHaveClass(/\bdark\b/);
});

test("the theme follows the system until one is picked", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "dark" });
  await page.goto(at("/"));
  await expect(page.locator("html")).toHaveClass(/\bdark\b/);
});

test("search finds a page and opens it", async ({ page }) => {
  await page.goto(at("/install"));
  await hydrated(page, page.getByRole("button", { name: "Search the docs" }));
  await page.locator("body").press("Control+k");
  const dialog = page.getByRole("dialog", { name: "Search the docs" });
  await dialog.getByRole("combobox").fill("restore token");
  await dialog.getByRole("option", { name: /Attaching to Wayland/ }).first().click();
  await expect(page).toHaveURL(/\/attach-wayland/);
});

test("pages, assets and the search index all load from under the base", async ({ page, baseURL }) => {
  const origin = new URL(baseURL ?? "").origin;
  const strays: string[] = [];
  page.on("response", (response) => {
    const url = new URL(response.url());
    if (url.origin !== origin) return;
    if (!url.pathname.startsWith(at("/")) || response.status() >= 400) strays.push(`${response.status()} ${url.pathname}`);
  });

  await page.goto(at("/introduction"));
  const screenshot = page.getByRole("img", { name: /^A Robin session/ });
  await expect(screenshot).toHaveJSProperty("complete", true);
  expect(await screenshot.evaluate((img: HTMLImageElement) => img.naturalWidth)).toBeGreaterThan(0);

  await hydrated(page, page.getByRole("button", { name: "Search the docs" }));
  await page.locator("body").press("Control+k");
  const dialog = page.getByRole("dialog", { name: "Search the docs" });
  await dialog.getByRole("combobox").fill("microphone");
  await expect(dialog.getByRole("option").first()).toBeVisible();
  await page.keyboard.press("Escape");

  await page.getByRole("main").getByRole("link", { name: "Kept sessions" }).first().click();
  await expect(page).toHaveURL(new RegExp(`${at("/sessions")}$`));
  await page.getByRole("link", { name: "Robin home" }).click();
  await expect(page).toHaveURL(new RegExp(`${at("/")}$`));
  expect(strays).toEqual([]);
});

test.describe("at phone width", () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true });

  test("the sidebar opens in a sheet and leads to a page", async ({ page }) => {
    await page.goto(at("/install"));
    await expect(page.getByRole("complementary").getByRole("navigation", { name: "Docs" })).toBeHidden();

    const menu = page.getByRole("button", { name: "Open navigation" });
    await hydrated(page, menu);
    await menu.click();
    const sheet = page.getByRole("dialog", { name: "Navigation" });
    await expect(sheet).toBeVisible();
    await sheet.getByRole("link", { name: "Sound" }).click();
    await expect(page).toHaveURL(/\/sound$/);
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("Sound");
  });

  for (const path of ["/", "/using", "/how-it-works"]) {
    test(`${path} does not scroll sideways`, async ({ page }) => {
      await page.goto(at(path));
      expect(await horizontalOverflow(page)).toBeLessThanOrEqual(0);
    });
  }
});

test("an unknown address gets the 404 page", async ({ page }) => {
  const response = await page.goto(at("/no-such-page"));
  expect(response?.status()).toBe(404);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("No page at this address");
  await expect(page.locator(".not-found-path")).toHaveText(at("/no-such-page"));
  await expect(page.getByRole("combobox")).toBeFocused();
});
