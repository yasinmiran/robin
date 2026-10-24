const ENTITIES: Record<string, string> = {
  amp: "&",
  lt: "<",
  gt: ">",
  quot: '"',
  apos: "'",
  nbsp: " ",
};

export function decodeEntities(text: string): string {
  return text.replace(/&(#x[0-9a-f]+|#[0-9]+|[a-z]+);/gi, (match, name: string) => {
    if (name[0] === "#") {
      const code = name[1].toLowerCase() === "x" ? parseInt(name.slice(2), 16) : parseInt(name.slice(1), 10);
      return Number.isFinite(code) ? String.fromCodePoint(code) : match;
    }
    return ENTITIES[name.toLowerCase()] ?? match;
  });
}

export function escapeHtml(text: string): string {
  return text
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

/** codeText turns highlighted code markup back into the text a reader copies. */
export function codeText(html: string): string {
  const withoutPrompts = html.replace(/<span class="code-prompt"[^>]*>[^<]*<\/span>/g, "");
  return decodeEntities(withoutPrompts.replace(/<[^>]+>/g, "")).replace(/\n+$/, "");
}

/**
 * withLead puts a bold run-in lead at the start of a callout's first
 * paragraph, so the lead reads as part of the sentence that follows it.
 */
export function withLead(html: string, lead: string | undefined): string {
  if (!lead) return html;
  const bold = `<b class="callout-lead">${escapeHtml(lead)}</b>`;
  const opening = html.match(/^\s*<p(\s[^>]*)?>/);
  if (!opening) return `<p>${bold}</p>${html}`;
  return `${opening[0]}${bold} ${html.slice(opening[0].length)}`;
}
