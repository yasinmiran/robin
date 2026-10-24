import { describe, expect, it } from "vitest";

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
  { slug: "roadmap", title: "Roadmap", section: "Project", order: 3 },
  { slug: "install", title: "Install and build", section: "Getting started", order: 3 },
  { slug: "introduction", title: "Introduction", section: "Getting started", order: 1 },
  { slug: "using", title: "Using Robin", section: "Guides", order: 1 },
  { slug: "known-limits", title: "Known limits", section: "Project", order: 1 },
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
      { slug: "b", title: "Beta", section: "Guides", order: 1 },
      { slug: "a", title: "Alpha", section: "Guides", order: 1 },
    ];
    expect(sortDocs(tied).map((doc) => doc.slug)).toEqual(["a", "b"]);
    expect(tied[0].slug).toBe("b");
  });
});

describe("groupDocs", () => {
  it("makes one group per section with pages, in section order", () => {
    const groups = groupDocs(docs);
    expect(groups.map((group) => group.label)).toEqual(["Getting started", "Guides", "Project"]);
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
      toDocLinks([{ id: "sound", data: { title: "Sound", section: "Guides", order: 4 } }]),
    ).toEqual([{ slug: "sound", title: "Sound", section: "Guides", order: 4 }]);
  });
});

describe("header links", () => {
  it("links each page by its slug", () => {
    expect(docHref("install")).toBe("/install");
  });

  it("marks the link for the section a page sits in", () => {
    expect(currentHeaderLink("Getting started")).toBe("Guide");
    expect(currentHeaderLink("Guides")).toBe("Guide");
    expect(currentHeaderLink("Internals")).toBe("How it works");
    expect(currentHeaderLink("Project")).toBe("Roadmap");
    expect(currentHeaderLink(undefined)).toBeUndefined();
  });

  it("points Flags at the flag reference", () => {
    expect(HEADER_LINKS.find((link) => link.label === "Flags")?.href).toBe("/using#flag-reference");
  });
});
