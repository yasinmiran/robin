export const SECTIONS = ["Start", "Use", "Reference", "Design", "Project"] as const;

export type Section = (typeof SECTIONS)[number];

export interface DocLink {
  slug: string;
  title: string;
  section: Section;
  order: number;
}

export interface NavGroup {
  label: Section;
  items: DocLink[];
}

export interface HeaderLink {
  label: string;
  href: string;
  /** Sections whose pages mark this link as the current one, besides the page it opens. */
  sections: readonly Section[];
}

export const HEADER_LINKS: readonly HeaderLink[] = [
  { label: "Guide", href: "/introduction", sections: ["Start", "Use"] },
  { label: "Flags", href: "/flags", sections: [] },
  { label: "How it works", href: "/how-it-works", sections: ["Design"] },
  { label: "Roadmap", href: "/roadmap", sections: ["Project"] },
];

/** toDocLinks reads each page's place in the sidebar from its collection entry. */
export function toDocLinks(
  entries: readonly { id: string; data: { title: string; section: Section; order: number } }[],
): DocLink[] {
  return entries.map(({ id, data }) => ({
    slug: id,
    title: data.title,
    section: data.section,
    order: data.order,
  }));
}

export function docHref(slug: string): string {
  return `/${slug}`;
}

/** sortDocs orders pages by section, then by their order within it. */
export function sortDocs<T extends DocLink>(docs: readonly T[]): T[] {
  return [...docs].sort(
    (a, b) =>
      SECTIONS.indexOf(a.section) - SECTIONS.indexOf(b.section) ||
      a.order - b.order ||
      a.title.localeCompare(b.title),
  );
}

/** groupDocs builds the sidebar: one group per section that has pages. */
export function groupDocs(docs: readonly DocLink[]): NavGroup[] {
  const sorted = sortDocs(docs);
  return SECTIONS.map((label) => ({
    label,
    items: sorted.filter((doc) => doc.section === label),
  })).filter((group) => group.items.length > 0);
}

/** neighbours returns the pages before and after slug in sidebar order. */
export function neighbours(
  docs: readonly DocLink[],
  slug: string,
): { prev?: DocLink; next?: DocLink } {
  const sorted = sortDocs(docs);
  const at = sorted.findIndex((doc) => doc.slug === slug);
  if (at < 0) return {};
  return { prev: sorted[at - 1], next: sorted[at + 1] };
}

/**
 * currentHeaderLink names the header link a page marks as current: the link
 * that opens it, else the one for its section.
 */
export function currentHeaderLink(slug: string | undefined, section: Section | undefined): string | undefined {
  const opens = slug === undefined ? undefined : HEADER_LINKS.find((link) => link.href === docHref(slug));
  if (opens) return opens.label;
  if (!section) return undefined;
  return HEADER_LINKS.find((link) => link.sections.includes(section))?.label;
}
