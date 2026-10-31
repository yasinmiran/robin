import { describe, expect, it, vi } from "vitest";

import {
  type DocLink,
  HEADER_LINKS,
  currentHeaderLink,
  docHref,
  groupDocs,
  neighbours,
  sortDocs,
  toDocLinks,
} from "./nav";

const docs: DocLink[] = [
  { slug: "roadmap", title: "Roadmap", section: "Project", order: 2 },
  { slug: "install", title: "Install and build", section: "Start", order: 3 },
  { slug: "introduction", title: "Introduction", section: "Start", order: 1 },
  { slug: "using", title: "Using Robin", section: "Use", order: 1 },
  { slug: "known-limits", title: "Known limits", section: "Reference", order: 2 },
];

describe("sortDocs", () => {
  it("orders by section, then by order within it", () => {
    expect(sortDocs(docs).map((doc) => doc.slug)).toEqual([
      "introduction",
      "install",
      "using",
      "known-limits",
      "roadmap",
    ]);
  });

  it("breaks a tie in order by title and leaves its input alone", () => {
    const tied: DocLink[] = [
      { slug: "b", title: "Beta", section: "Use", order: 1 },
      { slug: "a", title: "Alpha", section: "Use", order: 1 },
    ];
    expect(sortDocs(tied).map((doc) => doc.slug)).toEqual(["a", "b"]);
    expect(tied[0].slug).toBe("b");
  });
});

describe("groupDocs", () => {
  it("makes one group per section with pages, in section order", () => {
    const groups = groupDocs(docs);
    expect(groups.map((group) => group.label)).toEqual(["Start", "Use", "Reference", "Project"]);
    expect(groups[0].items.map((doc) => doc.slug)).toEqual(["introduction", "install"]);
  });
});

describe("neighbours", () => {
  it("finds the pages either side in sidebar order", () => {
    expect(neighbours(docs, "using")).toEqual({
      prev: expect.objectContaining({ slug: "install" }),
      next: expect.objectContaining({ slug: "known-limits" }),
    });
  });

  it("has no previous page at the start and no next page at the end", () => {
    expect(neighbours(docs, "introduction").prev).toBeUndefined();
    expect(neighbours(docs, "roadmap").next).toBeUndefined();
  });

  it("returns nothing for a page it does not know", () => {
    expect(neighbours(docs, "nope")).toEqual({});
  });
});

describe("toDocLinks", () => {
  it("reads the sidebar fields from collection entries", () => {
    expect(
      toDocLinks([{ id: "sound", data: { title: "Sound", section: "Use", order: 4 } }]),
    ).toEqual([{ slug: "sound", title: "Sound", section: "Use", order: 4 }]);
  });
});

describe("header links", () => {
  it("links each page by its slug", () => {
    expect(docHref("install")).toBe("/install");
  });

  it("links each page under the base the site is served from", () => {
    vi.stubEnv("BASE_URL", "/robin");
    try {
      expect(docHref("install")).toBe("/robin/install");
    } finally {
      vi.unstubAllEnvs();
    }
  });

  it("marks the link for the section a page sits in", () => {
    expect(currentHeaderLink("install", "Start")).toBe("Guide");
    expect(currentHeaderLink("sound", "Use")).toBe("Guide");
    expect(currentHeaderLink("testing", "Design")).toBe("How it works");
    expect(currentHeaderLink("robin-and-horizon", "Project")).toBe("Roadmap");
    expect(currentHeaderLink(undefined, undefined)).toBeUndefined();
  });

  it("marks Flags on the flags page only, not on the rest of its section", () => {
    expect(currentHeaderLink("flags", "Reference")).toBe("Flags");
    expect(currentHeaderLink("security", "Reference")).toBeUndefined();
  });

  it("points Flags at its own page", () => {
    expect(HEADER_LINKS.find((link) => link.label === "Flags")?.href).toBe("/flags");
  });
});
