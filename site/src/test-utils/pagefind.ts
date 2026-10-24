import { vi } from "vitest";

import type { Pagefind, PagefindData } from "@/lib/search";

/** fakePagefind answers every search with the given pages. */
export function fakePagefind(pages: PagefindData[]): Pagefind {
  return {
    search: vi.fn(async () => ({ results: pages.map((page) => ({ data: async () => page })) })),
  };
}
