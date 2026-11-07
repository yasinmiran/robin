import { docHref } from "@/lib/nav";

/** Moved says where a removed page went: a slug, with or without a #fragment. */
export interface Moved {
  to: string;
  /** Old anchors of the removed page and where each now lives. */
  anchors?: Record<string, string>;
}

export const MOVED: Record<string, Moved> = {
  install: {
    to: "getting-started",
    anchors: {
      "what-the-host-needs": "getting-started#what-the-host-needs",
      "where-the-agent-lives-on-the-host": "getting-started#what-the-host-needs",
      "what-the-client-needs": "browsers",
      "two-binaries-two-toolchains": "contributing#build",
    },
  },
  "quick-start": {
    to: "getting-started#your-first-session",
    anchors: {
      "2-if-the-host-is-missing-something": "getting-started#if-the-host-is-missing-something",
      "3-use-it": "getting-started#using-the-desktop",
      "4-leave-it-or-end-it": "getting-started#leave-it-or-end-it",
      "when-something-goes-wrong": "troubleshooting",
    },
  },
  "attach-x11": {
    to: "attach#x11",
    anchors: {
      monitors: "attach#monitors",
      "left-as-found": "attach#left-as-found",
      limits: "attach#limits",
    },
  },
  "attach-wayland": {
    to: "attach#wayland",
    anchors: {
      "the-first-time-the-consent-dialog": "attach#the-consent-dialog",
      "later-sessions-the-restore-token": "attach#the-restore-token",
      "what-the-host-needs": "attach#what-the-host-needs",
      "how-it-differs-from-x11": "attach#how-it-differs-from-x11",
      limits: "attach#limits",
      desktops: "attach#limits",
    },
  },
  "microphone-and-camera": {
    to: "sound#microphone-and-camera",
    anchors: {
      limits: "sound#limits",
      "what-the-host-needs": "sound#what-the-host-needs",
    },
  },
  "hardware-encoding": { to: "using#gpu-encoding" },
  "known-limits": {
    to: "troubleshooting",
    anchors: {
      "sharp-text-and-actual-size": "using#limits",
      "hardware-encoding": "using#limits",
      "kept-sessions": "sessions#limits",
      "attaching-to-x11": "attach#limits",
      "attaching-to-wayland": "attach#limits",
      "private-sessions": "private-session#limits",
      sound: "sound#limits",
      "microphone-and-camera": "sound#limits",
      "files-and-the-clipboard": "files-and-clipboard#limits",
      "keys-and-macros": "macros#limits",
      sharing: "sharing#limits",
      "browsers-and-clients": "browsers#limits",
      "video-and-network": "troubleshooting#the-picture-is-slow-or-blocky",
      coverage: "contributing#tests",
    },
  },
  testing: { to: "contributing#tests" },
  "verifying-a-release": { to: "contributing#reproducible-builds" },
  roadmap: { to: "introduction#what-robin-is-not" },
  "robin-and-horizon": { to: "introduction#what-robin-is-not" },
};

/** slugOf is the page a target such as "attach#limits" names. */
export function slugOf(target: string): string {
  return target.split("#")[0];
}

/** targetHref is the address of a target such as "attach#limits", under the base. */
export function targetHref(target: string): string {
  const [slug, fragment] = target.split("#");
  return fragment === undefined ? docHref(slug) : `${docHref(slug)}#${fragment}`;
}

/** Landing is what a redirect page needs to send a reader on, with every address under the base. */
export interface Landing {
  /** Where the old address lands with no listed anchor. */
  href: string;
  /** Whether that address has a fragment of its own, which an old hash never replaces. */
  ownFragment: boolean;
  /** Each listed old anchor and the address it lands on. */
  anchors: Record<string, string>;
}

/** landingFor resolves an old page's entry in MOVED to addresses. */
export function landingFor(moved: Moved): Landing {
  return {
    href: targetHref(moved.to),
    ownFragment: moved.to.includes("#"),
    anchors: Object.fromEntries(
      Object.entries(moved.anchors ?? {}).map(([anchor, target]) => [anchor, targetHref(target)]),
    ),
  };
}

/**
 * land is where a reader of an old address with hash goes: the anchor map
 * first, then the old hash if the target has no fragment of its own. The
 * redirect page runs the same rule in the browser.
 */
export function land({ href, ownFragment, anchors }: Landing, hash: string): string {
  const anchor = hash.replace(/^#/, "");
  if (Object.hasOwn(anchors, anchor)) return anchors[anchor];
  return anchor === "" || ownFragment ? href : `${href}#${anchor}`;
}

/** movedHref is where an old address lands: the anchor map first, then the old hash if the target has none. */
export function movedHref(old: string, hash: string): string {
  const moved = MOVED[old];
  if (!moved) throw new Error(`no moved page ${old}`);
  return land(landingFor(moved), hash);
}
