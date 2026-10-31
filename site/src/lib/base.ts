import type { HastPluginDefinition } from "satteri";

/**
 * joinBase puts a root-relative site path under base, the path the site is
 * served from: ("/robin", "/install") is /robin/install and ("/robin", "/")
 * is /robin/, the homepage, which a host would otherwise redirect to.
 */
export function joinBase(base: string, path: string): string {
  return `${base.replace(/\/+$/, "")}${path}`;
}

/** withBase is joinBase for the base this build serves from. */
export function withBase(path: string): string {
  return joinBase(import.meta.env.BASE_URL, path);
}

/** isRootRelative is true for /install, false for //cdn.example, #top, a URL or a relative path. */
export function isRootRelative(link: string): boolean {
  return link.startsWith("/") && !link.startsWith("//");
}

/**
 * baseLinks moves the root-relative links and images the docs write in
 * Markdown, [Sound](/sound), under base. Astro does that only for the links
 * its own components build.
 */
export function baseLinks(base: string): HastPluginDefinition {
  return {
    name: "base-links",
    element: {
      filter: ["a", "img"],
      visit(node, ctx) {
        const key = node.tagName === "a" ? "href" : "src";
        const link = node.properties?.[key];
        if (typeof link === "string" && isRootRelative(link)) ctx.setProperty(node, key, joinBase(base, link));
      },
    },
  };
}
