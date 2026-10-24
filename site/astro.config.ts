import { satteri } from "@astrojs/markdown-satteri";
import mdx from "@astrojs/mdx";
import react from "@astrojs/react";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "astro/config";
import type { ThemeRegistration } from "shiki";

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

export default defineConfig({
  // Set SITE_URL at build time to emit canonical and og:url tags.
  site: process.env.SITE_URL || undefined,
  trailingSlash: "never",
  build: { format: "file" },
  compressHTML: true,
  markdown: {
    // Double hyphens are flags here (--attach), never dashes.
    processor: satteri({ features: { smartPunctuation: { dashes: false } } }),
    shikiConfig: { theme: codeTheme },
  },
  integrations: [react(), mdx()],
  vite: {
    plugins: [tailwindcss()],
  },
});
