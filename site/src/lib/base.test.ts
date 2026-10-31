import { markdownToHtml } from "satteri";
import { afterEach, describe, expect, it, vi } from "vitest";

import { baseLinks, isRootRelative, joinBase, withBase } from "./base";

afterEach(() => {
  vi.unstubAllEnvs();
});

describe("joinBase", () => {
  it.each([
    ["/", "/install", "/install"],
    ["/", "/", "/"],
    ["/robin", "/install#build", "/robin/install#build"],
    ["/robin/", "/install", "/robin/install"],
    ["/robin", "/", "/robin/"],
  ])("(%s, %s) is %s", (base, path, joined) => {
    expect(joinBase(base, path)).toBe(joined);
  });
});

describe("withBase", () => {
  it("leaves paths alone at the root", () => {
    expect(withBase("/flags")).toBe("/flags");
  });

  it("follows the base the build serves from", () => {
    vi.stubEnv("BASE_URL", "/robin");
    expect(withBase("/flags")).toBe("/robin/flags");
    expect(withBase("/")).toBe("/robin/");
  });
});

describe("isRootRelative", () => {
  it.each([
    ["/install", true],
    ["/", true],
    ["//cdn.example/x.js", false],
    ["https://example.com/", false],
    ["#top", false],
    ["install", false],
    ["../img/a.png", false],
  ])("%s: %s", (link, rooted) => {
    expect(isRootRelative(link)).toBe(rooted);
  });
});

describe("baseLinks", () => {
  const render = async (markdown: string, base: string) =>
    (await markdownToHtml(markdown, { hastPlugins: [baseLinks(base)] })).html;

  it("moves root-relative links and images under the base", async () => {
    const html = await render("[Sound](/sound) [Limits](/known-limits#sound)\n\n![A session](/img/session.png)", "/robin");
    expect(html).toContain('href="/robin/sound"');
    expect(html).toContain('href="/robin/known-limits#sound"');
    expect(html).toContain('src="/robin/img/session.png"');
  });

  it("leaves anchors, relative and external links alone", async () => {
    const html = await render("[a](#top) [b](sound) [c](https://example.com/x) [d](//cdn.example/y)", "/robin");
    expect(html).toContain('href="#top"');
    expect(html).toContain('href="sound"');
    expect(html).toContain('href="https://example.com/x"');
    expect(html).toContain('href="//cdn.example/y"');
  });

  it("changes nothing at the root", async () => {
    expect(await render("[Sound](/sound)", "/")).toContain('href="/sound"');
  });
});
