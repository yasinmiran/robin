// Fails when a built page links to a file or an anchor that does not exist.
// Usage: node scripts/check-links.ts dist
import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import { join, posix, relative, sep } from "node:path";
import { pathToFileURL } from "node:url";

export interface BrokenLink {
  page: string;
  link: string;
  reason: string;
}

/** Attributes that point at something the site has to serve. */
const LINK_ATTRIBUTES = ["href", "src", "component-url", "renderer-url", "before-hydration-url"];

const ATTRIBUTE = new RegExp(
  `\\s(${LINK_ATTRIBUTES.join("|")})\\s*=\\s*(?:"([^"]*)"|'([^']*)'|([^\\s>]+))`,
  "gi",
);

function decode(value: string): string {
  return value.replace(/&amp;/g, "&").replace(/&#x2F;/gi, "/");
}

/** linksIn lists every link-carrying attribute value in a page. */
export function linksIn(html: string): string[] {
  const found: string[] = [];
  for (const match of html.matchAll(ATTRIBUTE)) {
    found.push(decode(match[2] ?? match[3] ?? match[4] ?? ""));
  }
  return found;
}

/** idsIn collects the anchors a page offers: id and a[name]. */
export function idsIn(html: string): Set<string> {
  const ids = new Set<string>();
  for (const match of html.matchAll(/\s(?:id|name)\s*=\s*(?:"([^"]*)"|'([^']*)')/gi)) {
    ids.add(decode(match[1] ?? match[2] ?? ""));
  }
  return ids;
}

export function isExternal(link: string): boolean {
  return /^([a-z][a-z0-9+.-]*:|\/\/)/i.test(link);
}

/**
 * fileFor maps a site path to the built file that serves it, the way a
 * static host does: /install is install.html, /docs/ is docs/index.html.
 */
export function fileFor(dist: string, sitePath: string): string | null {
  const clean = decodeURIComponent(sitePath).replace(/^\/+/, "");
  const candidates = clean === "" || clean.endsWith("/")
    ? [join(dist, clean, "index.html")]
    : [join(dist, clean), join(dist, `${clean}.html`), join(dist, clean, "index.html")];
  return candidates.find((file) => existsSync(file) && statSync(file).isFile()) ?? null;
}

/** sitePathOf is the address a built file is served at. */
export function sitePathOf(dist: string, file: string): string {
  return `/${relative(dist, file).split(sep).join("/")}`;
}

function htmlFiles(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) return htmlFiles(path);
    return entry.name.endsWith(".html") ? [path] : [];
  });
}

export function checkLinks(dist: string): BrokenLink[] {
  const broken: BrokenLink[] = [];
  const idCache = new Map<string, Set<string>>();
  const idsOf = (file: string) => {
    let ids = idCache.get(file);
    if (!ids) {
      ids = idsIn(readFileSync(file, "utf8"));
      idCache.set(file, ids);
    }
    return ids;
  };

  for (const file of htmlFiles(dist)) {
    const page = sitePathOf(dist, file);
    for (const link of linksIn(readFileSync(file, "utf8"))) {
      if (link === "" || isExternal(link)) continue;
      const [pathAndQuery, hash = ""] = link.split("#");
      const path = pathAndQuery.split("?")[0];
      const target = path === "" ? file : fileFor(dist, posix.resolve(posix.dirname(page), path));
      if (!target) {
        broken.push({ page, link, reason: "no such file" });
        continue;
      }
      if (hash && target.endsWith(".html") && !idsOf(target).has(decodeURIComponent(hash))) {
        broken.push({ page, link, reason: `no #${hash} on ${sitePathOf(dist, target)}` });
      }
    }
  }
  return broken;
}

function main(dist: string | undefined): number {
  if (!dist || !existsSync(dist)) {
    console.error(`check-links: build output not found: ${dist ?? "(none given)"}`);
    return 2;
  }
  const broken = checkLinks(dist);
  for (const { page, link, reason } of broken) console.error(`${page}: ${link} (${reason})`);
  console.log(broken.length ? `check-links: ${broken.length} broken` : "check-links: every internal link resolves");
  return broken.length ? 1 : 0;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exitCode = main(process.argv[2]);
}

export { main };
