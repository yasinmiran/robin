import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { checkLinks, fileFor, idsIn, isExternal, linksIn, main, sitePathOf, underBase } from "./check-links";

let dist: string;

function page(path: string, html: string) {
  const file = join(dist, path);
  mkdirSync(join(file, ".."), { recursive: true });
  writeFileSync(file, html);
}

beforeEach(() => {
  dist = mkdtempSync(join(tmpdir(), "robin-links-"));
});

afterEach(() => {
  rmSync(dist, { recursive: true, force: true });
});

describe("linksIn", () => {
  it("reads href, src and island URLs in any quoting", () => {
    const html = `<a href="/a">x</a><img src='/b.png'><link href=/c.css><astro-island component-url="/_astro/d.js" renderer-url="/_astro/e.js"></astro-island>`;
    expect(linksIn(html)).toEqual(["/a", "/b.png", "/c.css", "/_astro/d.js", "/_astro/e.js"]);
  });

  it("decodes entities and ignores escaped markup in text", () => {
    expect(linksIn(`<a href="/a?x=1&amp;y=2">x</a><code>href=&quot;/nope&quot;</code>`)).toEqual(["/a?x=1&y=2"]);
  });
});

describe("idsIn", () => {
  it("collects ids and anchor names", () => {
    expect([...idsIn(`<h2 id="one">1</h2><a name='two'></a><div data-id="no"></div>`)]).toEqual(["one", "two"]);
  });
});

describe("isExternal", () => {
  it.each([
    ["https://example.com", true],
    ["mailto:a@b.c", true],
    ["//cdn.example.com/x.js", true],
    ["data:image/png;base64,AA", true],
    ["/install", false],
    ["#top", false],
    ["../img/a.png", false],
  ])("%s is external: %s", (link, external) => {
    expect(isExternal(link)).toBe(external);
  });
});

describe("fileFor", () => {
  it("resolves pages the way a static host does", () => {
    page("index.html", "");
    page("install.html", "");
    page("guide/index.html", "");
    page("favicon.svg", "");
    expect(fileFor(dist, "/")).toBe(join(dist, "index.html"));
    expect(fileFor(dist, "/install")).toBe(join(dist, "install.html"));
    expect(fileFor(dist, "/install.html")).toBe(join(dist, "install.html"));
    expect(fileFor(dist, "/guide")).toBe(join(dist, "guide/index.html"));
    expect(fileFor(dist, "/guide/")).toBe(join(dist, "guide/index.html"));
    expect(fileFor(dist, "/favicon.svg")).toBe(join(dist, "favicon.svg"));
    expect(fileFor(dist, "/missing")).toBeNull();
  });

  it("maps a file back to its address", () => {
    expect(sitePathOf(dist, join(dist, "guide/index.html"))).toBe("/guide/index.html");
    expect(sitePathOf(dist, join(dist, "guide/index.html"), "/robin")).toBe("/robin/guide/index.html");
  });
});

describe("checkLinks", () => {
  it("passes a site whose links and anchors all exist", () => {
    page("index.html", `<a href="/install#build">x</a><a href="#top">t</a><span id="top"></span><a href="https://x.y/">e</a><a href="">e</a>`);
    page("install.html", `<h2 id="build">Build</h2><img src="img/a.png"><a href="./?q=1">home</a>`);
    page("img/a.png", "png");
    expect(checkLinks(dist)).toEqual([]);
  });

  it("reports a missing page, a missing file and a missing anchor", () => {
    page("index.html", `<a href="/nope">x</a><img src="/gone.png"><a href="/install#missing">y</a><a href="#here">z</a>`);
    page("install.html", `<h2 id="build">Build</h2>`);
    expect(checkLinks(dist)).toEqual([
      { page: "/index.html", link: "/nope", reason: "no such file" },
      { page: "/index.html", link: "/gone.png", reason: "no such file" },
      { page: "/index.html", link: "/install#missing", reason: "no #missing on /install.html" },
      { page: "/index.html", link: "#here", reason: "no #here on /index.html" },
    ]);
  });

  it("resolves relative links against the page's directory", () => {
    page("guide/index.html", `<a href="../install">x</a><a href="part">y</a>`);
    page("install.html", "");
    page("guide/part.html", "");
    expect(checkLinks(dist)).toEqual([]);
  });

  it("does not look for anchors in files that are not pages", () => {
    page("index.html", `<a href="/doc.svg#frag">x</a>`);
    page("doc.svg", "<svg/>");
    expect(checkLinks(dist)).toEqual([]);
  });

  it("finds pages under the base the site is served from", () => {
    page("index.html", `<a href="/robin/install#build">x</a><a href="/robin/">home</a><a href="/robin">home</a>`);
    page("install.html", `<h2 id="build">Build</h2><a href="sound">s</a><a href="../robin/sound">s</a>`);
    page("sound.html", "");
    expect(checkLinks(dist, "/robin")).toEqual([]);
  });

  it("reports a link that leaves the base, as a hard-coded root link does", () => {
    page("index.html", `<a href="/install">x</a><a href="/robinson">y</a><a href="../install">z</a><a href="/robin/install#nope">w</a>`);
    page("install.html", "");
    expect(checkLinks(dist, "/robin/")).toEqual([
      { page: "/robin/index.html", link: "/install", reason: "outside /robin/" },
      { page: "/robin/index.html", link: "/robinson", reason: "outside /robin/" },
      { page: "/robin/index.html", link: "../install", reason: "outside /robin/" },
      { page: "/robin/index.html", link: "/robin/install#nope", reason: "no #nope on /robin/install.html" },
    ]);
  });
});

describe("underBase", () => {
  it.each([
    ["/", "/install", "/install"],
    ["/", "/", "/"],
    ["/robin", "/robin/install", "/install"],
    ["/robin", "/robin", "/"],
    ["/robin/", "/robin/", "/"],
    ["/robin", "/robinson", null],
    ["/robin", "/install", null],
  ])("(%s, %s) is %s", (base, address, path) => {
    expect(underBase(base, address)).toBe(path);
  });
});

describe("main", () => {
  it("exits 0 when every link resolves", () => {
    const log = vi.spyOn(console, "log").mockImplementation(() => {});
    page("index.html", `<a href="/">home</a>`);
    expect(main(dist)).toBe(0);
    expect(log).toHaveBeenCalledWith("check-links: every internal link resolves");
  });

  it("exits 1 and names each broken link", () => {
    vi.spyOn(console, "log").mockImplementation(() => {});
    const error = vi.spyOn(console, "error").mockImplementation(() => {});
    page("index.html", `<a href="/nope">x</a>`);
    expect(main(dist)).toBe(1);
    expect(error).toHaveBeenCalledWith("/index.html: /nope (no such file)");
  });

  it("checks against the base it is given", () => {
    vi.spyOn(console, "log").mockImplementation(() => {});
    const error = vi.spyOn(console, "error").mockImplementation(() => {});
    page("index.html", `<a href="/robin/">home</a>`);
    expect(main(dist, "/robin")).toBe(0);
    expect(main(dist)).toBe(1);
    expect(error).toHaveBeenCalledWith("/index.html: /robin/ (no such file)");
  });

  it("exits 2 without a build to check", () => {
    const error = vi.spyOn(console, "error").mockImplementation(() => {});
    expect(main(undefined)).toBe(2);
    expect(main(join(dist, "absent"))).toBe(2);
    expect(error).toHaveBeenCalledTimes(2);
  });
});
