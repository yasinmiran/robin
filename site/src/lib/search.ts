import { withBase } from "@/lib/base";

/** The slice of Pagefind's browser API the search dialog uses. */
export interface Pagefind {
  init?: () => Promise<void>;
  search: (term: string) => Promise<{ results: { data: () => Promise<PagefindData> }[] } | null>;
}

export interface PagefindData {
  url: string;
  excerpt: string;
  meta: { title?: string };
  sub_results?: { title: string; url: string; excerpt: string }[];
}

export interface SearchSection {
  title: string;
  url: string;
}

export interface SearchResult {
  url: string;
  title: string;
  /** Pagefind's excerpt: escaped page text with the matches in <mark>. */
  excerpt: string;
  sections: SearchSection[];
}

export type PagefindLoader = () => Promise<Pagefind>;

// Pagefind puts its results under the path it was loaded from, the base.
export const PAGEFIND_URL = withBase("/pagefind/pagefind.js");

export const loadPagefind: PagefindLoader = async () => {
  const pagefind = (await import(/* @vite-ignore */ PAGEFIND_URL)) as Pagefind;
  await pagefind.init?.();
  return pagefind;
};

/** pageUrl maps a built file's URL to the page's address: /install.html is /install. */
export function pageUrl(url: string): string {
  const [path, hash] = url.split("#");
  const page = path.replace(/(^|\/)index\.html$/, "$1").replace(/\.html$/, "") || "/";
  return hash ? `${page}#${hash}` : page;
}

export async function runSearch(pagefind: Pagefind, query: string, limit = 8): Promise<SearchResult[]> {
  const term = query.trim();
  if (!term) return [];
  const found = await pagefind.search(term);
  if (!found) return [];
  const pages = await Promise.all(found.results.slice(0, limit).map((result) => result.data()));
  return pages.map((page) => {
    const url = pageUrl(page.url);
    return {
      url,
      title: page.meta.title ?? url,
      excerpt: page.excerpt,
      sections: (page.sub_results ?? [])
        .map((section) => ({ title: section.title, url: pageUrl(section.url) }))
        .filter((section) => section.url !== url)
        .slice(0, 3),
    };
  });
}

/** isTypingTarget is true where a keystroke belongs to a field, not a shortcut. */
export function isTypingTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  return target.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(target.tagName);
}

/** isSearchShortcut matches Ctrl K, Cmd K, and / outside a text field. */
export function isSearchShortcut(event: KeyboardEvent): boolean {
  if (event.key.toLowerCase() === "k" && (event.ctrlKey || event.metaKey) && !event.altKey) return true;
  return event.key === "/" && !event.ctrlKey && !event.metaKey && !event.altKey && !isTypingTarget(event.target);
}

export function isApplePlatform(platform: string): boolean {
  return /mac|iphone|ipad|ipod/i.test(platform);
}
