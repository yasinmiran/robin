import { existsSync, readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

import { MOVED, landingFor, movedHref, slugOf, targetHref } from "./redirects";

// The path is passed in rather than written in new URL(), where Vite would
// rewrite it into an asset URL.
const fromHere = (path: string) => fileURLToPath(new URL(path, import.meta.url));
const src = fromHere("..");
const docs = join(src, "content", "docs");
const pages = join(src, "pages");

const hasPage = (slug: string) => existsSync(join(docs, `${slug}.mdx`));

describe("MOVED", () => {
  it("names no page that still exists, so no two routes build one path", () => {
    expect(Object.keys(MOVED).filter(hasPage)).toEqual([]);
  });

  it("sends every old page and old anchor to a page that exists", () => {
    const targets = Object.values(MOVED).flatMap((moved) => [moved.to, ...Object.values(moved.anchors ?? {})]);
    expect(targets.map(slugOf).filter((slug) => !hasPage(slug))).toEqual([]);
  });

  it("is linked from no page, since a redirect stub would hide the stale link from check-links", () => {
    const sources = [
      ...readdirSync(docs).filter((name) => name.endsWith(".mdx")).map((name) => join(docs, name)),
      ...readdirSync(pages).filter((name) => name.endsWith(".astro")).map((name) => join(pages, name)),
    ];
    const stale = sources.flatMap((file) => {
      const text = readFileSync(file, "utf8");
      return Object.keys(MOVED)
        .filter((old) => new RegExp(`\\]\\(/${old}[)#]|docHref\\("${old}"\\)|href="/${old}[#"]`).test(text))
        .map((old) => `${file}: /${old}`);
    });
    expect(stale).toEqual([]);
  });
});

describe("movedHref", () => {
  it("maps a listed anchor to its new home", () => {
    expect(movedHref("known-limits", "#sound")).toBe("/sound#limits");
    expect(movedHref("install", "what-the-client-needs")).toBe("/browsers");
  });

  it("passes an unlisted hash through to a target without a fragment", () => {
    expect(movedHref("install", "#build")).toBe("/getting-started#build");
    expect(movedHref("install", "")).toBe("/getting-started");
  });

  it("keeps the target's own fragment over an unlisted hash", () => {
    expect(movedHref("attach-wayland", "")).toBe("/attach#wayland");
    expect(movedHref("attach-wayland", "#somewhere")).toBe("/attach#wayland");
    expect(movedHref("hardware-encoding", "#at-1080p")).toBe("/using#gpu-encoding");
  });

  it("takes no anchor from the object prototype", () => {
    expect(movedHref("install", "#constructor")).toBe("/getting-started#constructor");
  });

  it("refuses a page that did not move", () => {
    expect(() => movedHref("sound", "")).toThrow(/sound/);
  });
});

describe("landingFor", () => {
  it("resolves every address under the base", () => {
    expect(landingFor(MOVED["attach-x11"])).toEqual({
      href: "/attach#x11",
      ownFragment: true,
      anchors: { monitors: "/attach#monitors", "left-as-found": "/attach#left-as-found", limits: "/attach#limits" },
    });
    expect(landingFor(MOVED.testing).anchors).toEqual({});
  });

  it("builds a target with or without a fragment", () => {
    expect(targetHref("troubleshooting")).toBe("/troubleshooting");
    expect(targetHref("sound#limits")).toBe("/sound#limits");
  });
});
