import type { NavGroup } from "@/lib/nav";

export const groups: NavGroup[] = [
  {
    label: "Start",
    items: [
      { slug: "introduction", title: "Introduction", section: "Start", order: 1 },
      { slug: "getting-started", title: "Getting started", section: "Start", order: 2 },
    ],
  },
  {
    label: "Use",
    items: [{ slug: "sound", title: "Sound", section: "Use", order: 4 }],
  },
];

/** stayOnPage stops link clicks from navigating the test document away. */
export function stayOnPage(event: MouseEvent) {
  if ((event.target as HTMLElement).closest("a")) event.preventDefault();
}
