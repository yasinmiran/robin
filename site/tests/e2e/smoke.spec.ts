import { type Page, expect, test } from "@playwright/test";

/** failOnErrors fails the test on any uncaught error or console error. */
function failOnErrors(page: Page) {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (message) => {
    if (message.type() === "error") errors.push(message.text());
  });
  return () => expect(errors).toEqual([]);
}

async function horizontalOverflow(page: Page) {
  return page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
}

test("the homepage shows what Robin is", async ({ page }) => {
  const noErrors = failOnErrors(page);
  await page.goto("/");
  await expect(page).toHaveTitle(/^Robin/);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(
    "Robin: Your eyes and hands on any Linux box you can ssh to.",
  );
  await expect(page.getByRole("img", { name: /A Robin session in the browser/ })).toBeVisible();
  await expect(page.getByText("Connected", { exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: "Install" })).toHaveAttribute("href", "/install");
  await expect(page.locator('link[rel="icon"]')).toHaveAttribute("href", "/favicon.svg");
  noErrors();
});

for (const [path, title, heading] of [
  ["/install", "Install and build", "What the host needs"],
  ["/using", "Using Robin", "Flag reference"],
] as const) {
  test(`${path} reads as a docs page`, async ({ page }) => {
    const noErrors = failOnErrors(page);
    await page.goto(path);
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

test("each flag has an anchor", async ({ page }) => {
  await page.goto("/using#flag-max-size");
  await expect(page.locator("#flag-max-size")).toBeInViewport();
  await expect(page.locator("#flag-max-size")).toContainText("default 1920x1080");
});

test("the theme toggle switches and remembers the theme", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto("/install");
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
  await page.goto("/");
  await expect(page.locator("html")).toHaveClass(/\bdark\b/);
});

test("search finds a page and opens it", async ({ page }) => {
  await page.goto("/install");
  await page.locator("body").press("Control+k");
  const dialog = page.getByRole("dialog", { name: "Search the docs" });
  await dialog.getByRole("combobox").fill("restore token");
  await dialog.getByRole("option", { name: /Attaching to Wayland/ }).first().click();
  await expect(page).toHaveURL(/\/attach-wayland/);
});

test.describe("at phone width", () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true });

  test("the sidebar opens in a sheet and leads to a page", async ({ page }) => {
    await page.goto("/install");
    await expect(page.getByRole("complementary").getByRole("navigation", { name: "Docs" })).toBeHidden();

    await page.getByRole("button", { name: "Open navigation" }).click();
    const sheet = page.getByRole("dialog", { name: "Navigation" });
    await expect(sheet).toBeVisible();
    await sheet.getByRole("link", { name: "Sound" }).click();
    await expect(page).toHaveURL(/\/sound$/);
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("Sound");
  });

  for (const path of ["/", "/using", "/how-it-works"]) {
    test(`${path} does not scroll sideways`, async ({ page }) => {
      await page.goto(path);
      expect(await horizontalOverflow(page)).toBeLessThanOrEqual(0);
    });
  }
});

test("an unknown address gets the 404 page", async ({ page }) => {
  const response = await page.goto("/no-such-page");
  expect(response?.status()).toBe(404);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("No page at this address");
  await expect(page.locator(".not-found-path")).toHaveText("/no-such-page");
  await expect(page.getByRole("combobox")).toBeFocused();
});
