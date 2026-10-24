import { describe, expect, it } from "vitest";

import { activeSlug, tocItems } from "./toc";

describe("tocItems", () => {
  it("keeps h2 and h3 only", () => {
    const headings = [
      { depth: 1, slug: "title", text: "Title" },
      { depth: 2, slug: "a", text: "A" },
      { depth: 3, slug: "b", text: "B" },
      { depth: 4, slug: "c", text: "C" },
    ];
    expect(tocItems(headings).map((heading) => heading.slug)).toEqual(["a", "b"]);
  });
});

describe("activeSlug", () => {
  const positions = [
    { slug: "a", top: -400 },
    { slug: "b", top: 80 },
    { slug: "c", top: 600 },
  ];

  it("picks the last heading above the reading line", () => {
    expect(activeSlug(positions, 96)).toBe("b");
    expect(activeSlug(positions, 40)).toBe("a");
  });

  it("falls back to the first heading before any has scrolled past", () => {
    expect(activeSlug([{ slug: "a", top: 300 }], 96)).toBe("a");
  });

  it("has nothing to mark without headings", () => {
    expect(activeSlug([], 96)).toBeUndefined();
  });
});
