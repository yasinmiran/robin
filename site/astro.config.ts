import { satteri } from "@astrojs/markdown-satteri";
import mdx from "@astrojs/mdx";
import react from "@astrojs/react";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "astro/config";
import type { ThemeRegistration } from "shiki";

import { siteAddress } from "./scripts/site-url.ts";
import { baseLinks } from "./src/lib/base.ts";

// Code is coloured from the --code-* tokens, the same in both page themes.
// Only comments and quoted strings stand out: a shell argument is a string
// to the grammar, and painting every argument amber would drown the strings.
const codeTheme: ThemeRegistration = {
  name: "robin-code",
  type: "dark",
  colors: {
    "editor.foreground": "var(--code-foreground)",
    "editor.background": "var(--code)",
  },
  tokenColors: [
    {
      scope: ["comment", "punctuation.definition.comment"],
      settings: { foreground: "var(--code-muted)" },
    },
    {
      scope: ["string.quoted", "punctuation.definition.string"],
      settings: { foreground: "var(--code-string)" },
    },
  ],
};

// SITE_URL names the address the build is for: it adds canonical and og:url
// tags, and its path is the base every page is served under.
const { site, base } = siteAddress(process.env.SITE_URL);

export default defineConfig({
  site,
  base,
  trailingSlash: "never",
  build: { format: "file" },
  compressHTML: true,
  markdown: {
    // Double hyphens are flags here (--attach), never dashes.
    processor: satteri({
      features: { smartPunctuation: { dashes: false } },
      hastPlugins: [baseLinks(base)],
    }),
    shikiConfig: { theme: codeTheme },
  },
  integrations: [react(), mdx()],
  vite: {
    plugins: [tailwindcss()],
  },
});
