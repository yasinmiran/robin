/** flagId is the anchor of a flag in the reference: --max-size is #flag-max-size. */
export function flagId(name: string): string {
  const bare = name
    .replace(/^-+/, "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "");
  return `flag-${bare || "ssh-args"}`;
}

export type Segment = { code: boolean; text: string };

/** codeSegments splits text on backticks, so `1920x1080` renders as code. */
export function codeSegments(text: string): Segment[] {
  return text
    .split("`")
    .map((part, index) => ({ code: index % 2 === 1, text: part }))
    .filter((segment) => segment.text !== "");
}
