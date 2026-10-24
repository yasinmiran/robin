export interface Heading {
  depth: number;
  slug: string;
  text: string;
}

/** tocItems keeps the h2 and h3 headings a page's table of contents lists. */
export function tocItems(headings: readonly Heading[]): Heading[] {
  return headings.filter((heading) => heading.depth === 2 || heading.depth === 3);
}

/**
 * activeSlug picks the section being read: the last heading whose top has
 * scrolled above the line just under the sticky header.
 */
export function activeSlug(
  positions: readonly { slug: string; top: number }[],
  line: number,
): string | undefined {
  let active = positions[0]?.slug;
  for (const { slug, top } of positions) {
    if (top <= line) active = slug;
    else break;
  }
  return active;
}
