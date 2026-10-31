export interface SiteAddress {
  /** Origin the canonical and og:url tags are built on; none without SITE_URL. */
  site?: string;
  /** Path every page is served under: "/" or, say, "/robin", with no trailing slash. */
  base: string;
}

/**
 * siteAddress reads the address the build is for out of SITE_URL:
 * https://robin.example serves the site from the root and
 * https://yasinmiran.github.io/robin from /robin.
 */
export function siteAddress(url: string | undefined): SiteAddress {
  if (!url) return { base: "/" };
  const { origin, pathname } = new URL(url);
  return { site: origin, base: pathname.replace(/\/+$/, "") || "/" };
}
