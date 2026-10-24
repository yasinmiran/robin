import { describe, expect, it } from "vitest";

import { fakePagefind } from "@/test-utils/pagefind";

import {
  type PagefindData,
  isApplePlatform,
  isSearchShortcut,
  isTypingTarget,
  loadPagefind,
  pageUrl,
  runSearch,
} from "./search";

describe("pageUrl", () => {
  it.each([
    ["/install.html", "/install"],
    ["/install.html#what-the-host-needs", "/install#what-the-host-needs"],
    ["/index.html", "/"],
    ["/guide/index.html", "/guide/"],
    ["/install", "/install"],
  ])("%s is %s", (url, page) => {
    expect(pageUrl(url)).toBe(page);
  });
});

describe("runSearch", () => {
  const install: PagefindData = {
    url: "/install.html",
    excerpt: "the <mark>host</mark> needs",
    meta: { title: "Install and build" },
    sub_results: [
      { title: "Install and build", url: "/install.html", excerpt: "" },
      { title: "What the host needs", url: "/install.html#what-the-host-needs", excerpt: "" },
    ],
  };

  it("maps Pagefind results to page addresses and sections", async () => {
    const results = await runSearch(fakePagefind([install]), "host");
    expect(results).toEqual([
      {
        url: "/install",
        title: "Install and build",
        excerpt: "the <mark>host</mark> needs",
        sections: [{ title: "What the host needs", url: "/install#what-the-host-needs" }],
      },
    ]);
  });

  it("falls back to the address when a page has no title", async () => {
    const [result] = await runSearch(fakePagefind([{ url: "/x.html", excerpt: "", meta: {} }]), "x");
    expect(result.title).toBe("/x");
    expect(result.sections).toEqual([]);
  });

  it("caps the number of results", async () => {
    const pages = Array.from({ length: 12 }, (_, i) => ({ url: `/p${i}.html`, excerpt: "", meta: {} }));
    expect(await runSearch(fakePagefind(pages), "p", 5)).toHaveLength(5);
  });

  it("does not search for blank input", async () => {
    const pagefind = fakePagefind([install]);
    expect(await runSearch(pagefind, "   ")).toEqual([]);
    expect(pagefind.search).not.toHaveBeenCalled();
  });

  it("treats a cancelled search as no results", async () => {
    expect(await runSearch({ search: async () => null }, "host")).toEqual([]);
  });
});

describe("loadPagefind", () => {
  it("rejects when the index is not there, as in development", async () => {
    await expect(loadPagefind()).rejects.toThrow();
  });
});

function key(init: KeyboardEventInit, target?: HTMLElement) {
  const event = new KeyboardEvent("keydown", init);
  if (target) Object.defineProperty(event, "target", { value: target });
  return event;
}

describe("isSearchShortcut", () => {
  it("opens on Ctrl K and Cmd K", () => {
    expect(isSearchShortcut(key({ key: "k", ctrlKey: true }))).toBe(true);
    expect(isSearchShortcut(key({ key: "K", metaKey: true }))).toBe(true);
    expect(isSearchShortcut(key({ key: "k", ctrlKey: true, altKey: true }))).toBe(false);
    expect(isSearchShortcut(key({ key: "k" }))).toBe(false);
  });

  it("opens on / unless the reader is typing", () => {
    expect(isSearchShortcut(key({ key: "/" }, document.body))).toBe(true);
    expect(isSearchShortcut(key({ key: "/" }, document.createElement("input")))).toBe(false);
    expect(isSearchShortcut(key({ key: "/", ctrlKey: true }))).toBe(false);
  });
});

describe("isTypingTarget", () => {
  it("knows fields from everything else", () => {
    const editable = document.createElement("div");
    editable.contentEditable = "true";
    Object.defineProperty(editable, "isContentEditable", { value: true });
    expect(isTypingTarget(document.createElement("textarea"))).toBe(true);
    expect(isTypingTarget(document.createElement("select"))).toBe(true);
    expect(isTypingTarget(editable)).toBe(true);
    expect(isTypingTarget(document.createElement("a"))).toBe(false);
    expect(isTypingTarget(null)).toBe(false);
  });
});

describe("isApplePlatform", () => {
  it("recognises Apple platforms", () => {
    expect(isApplePlatform("MacIntel")).toBe(true);
    expect(isApplePlatform("macOS")).toBe(true);
    expect(isApplePlatform("iPhone")).toBe(true);
    expect(isApplePlatform("Linux x86_64")).toBe(false);
  });
});
