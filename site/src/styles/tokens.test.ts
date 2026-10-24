import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

type Theme = Map<string, { oklch: [number, number, number]; hex: string }>;

// The path is passed in rather than written in new URL(), where Vite would
// rewrite it into an asset URL.
function read(name: string) {
  return readFileSync(fileURLToPath(new URL(name, import.meta.url)), "utf8");
}

/** theme reads the colour tokens of one block: --name: oklch(L C H); with its hex in a comment. */
function theme(selector: string): Theme {
  const css = read("./tokens.css");
  const blocks = [...css.matchAll(/^(\S[^{\n]*) \{\n([^}]*)\}/gm)].filter(
    ([, sel, body]) => sel === selector && body.includes("--background"),
  );
  expect(blocks).toHaveLength(1);
  const tokens: Theme = new Map();
  for (const [, name, l, c, h, hex] of blocks[0][2].matchAll(
    /--([\w-]+): oklch\(([\d.]+) ([\d.]+) ([\d.]+)\); \/\* (#[0-9a-f]{6}) \*\//g,
  )) {
    tokens.set(name, { oklch: [Number(l), Number(c), Number(h)], hex });
  }
  return tokens;
}

function channels(hex: string): number[] {
  return [1, 3, 5].map((at) => parseInt(hex.slice(at, at + 2), 16));
}

function toLinear(channel: number): number {
  const c = channel / 255;
  return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
}

function fromLinear(c: number): number {
  const clamped = Math.min(1, Math.max(0, c));
  const srgb = clamped <= 0.0031308 ? 12.92 * clamped : 1.055 * clamped ** (1 / 2.4) - 0.055;
  return srgb * 255;
}

/** oklchToRgb is the CSS Color 4 conversion, to 0..255 sRGB channels. */
function oklchToRgb([lightness, chroma, hue]: [number, number, number]): number[] {
  const a = chroma * Math.cos((hue * Math.PI) / 180);
  const b = chroma * Math.sin((hue * Math.PI) / 180);
  const l = (lightness + 0.3963377774 * a + 0.2158037573 * b) ** 3;
  const m = (lightness - 0.1055613458 * a - 0.0638541728 * b) ** 3;
  const s = (lightness - 0.0894841775 * a - 1.291485548 * b) ** 3;
  return [
    4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s,
    -1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s,
    -0.0041960863 * l - 0.7034186147 * m + 1.707614701 * s,
  ].map(fromLinear);
}

function luminance(hex: string): number {
  const [r, g, b] = channels(hex).map(toLinear);
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function contrast(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

// [foreground, background, minimum]: 4.5 for text, 7 for body text, 3 for
// edges and focus rings, 1.5 for the logo tile against the page.
const PAIRS: [string, string, number][] = [
  ["foreground", "background", 7],
  ["muted-foreground", "background", 4.5],
  ["muted-foreground", "muted", 4.5],
  ["muted-foreground", "card", 4.5],
  ["muted-foreground", "accent", 4.5],
  ["foreground", "accent", 7],
  ["primary", "background", 4.5],
  ["primary", "card", 4.5],
  ["primary-foreground", "primary", 4.5],
  ["primary-foreground", "primary-hover", 4.5],
  ["destructive", "background", 4.5],
  ["destructive", "destructive-surface", 4.5],
  ["warning", "warning-surface", 4.5],
  ["warning", "background", 4.5],
  ["foreground", "warning-surface", 7],
  ["foreground", "destructive-surface", 7],
  ["code-foreground", "code", 7],
  ["code-muted", "code", 4.5],
  ["code-prompt", "code", 4.5],
  ["code-string", "code", 4.5],
  ["input", "background", 3],
  ["ring", "background", 3],
  ["ring", "card", 3],
  ["warning-edge", "background", 3],
  ["note-edge", "background", 3],
  ["mint", "tile", 3],
  ["tile", "background", 1.5],
];

describe.each([
  ["light", ":root"],
  ["dark", ".dark"],
])("tokens.css, %s", (_, selector) => {
  // Three decimals of oklch land within a channel step and a bit of the hex.
  it("renders each oklch value as the hex beside it", () => {
    const tokens = theme(selector);
    expect(tokens.size).toBeGreaterThan(30);
    for (const [name, { oklch, hex }] of tokens) {
      const rendered = oklchToRgb(oklch);
      channels(hex).forEach((channel, at) => {
        expect(Math.abs(rendered[at] - channel), `--${name} ${hex}`).toBeLessThanOrEqual(1.5);
      });
    }
  });

  it.each(PAIRS)("keeps --%s on --%s at %s:1 or more", (fg, bg, minimum) => {
    const tokens = theme(selector);
    const [front, back] = [tokens.get(fg), tokens.get(bg)];
    expect(front && back, `--${fg} and --${bg} are defined`).toBeTruthy();
    expect(contrast(front!.hex, back!.hex)).toBeGreaterThanOrEqual(minimum);
  });
});
