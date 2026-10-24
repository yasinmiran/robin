import type { NavGroup } from "@/lib/nav";

export const groups: NavGroup[] = [
  {
    label: "Getting started",
    items: [
      { slug: "introduction", title: "Introduction", section: "Getting started", order: 1 },
      { slug: "install", title: "Install and build", section: "Getting started", order: 3 },
    ],
  },
  {
    label: "Guides",
    items: [{ slug: "sound", title: "Sound", section: "Guides", order: 4 }],
  },
];

/** stayOnPage stops link clicks from navigating the test document away. */
export function stayOnPage(event: MouseEvent) {
  if ((event.target as HTMLElement).closest("a")) event.preventDefault();
}
