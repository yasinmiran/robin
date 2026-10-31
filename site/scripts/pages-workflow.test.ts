import { readFileSync } from "node:fs";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

// The workflow that builds and publishes this site. Its trust boundary is in
// how it is laid out, so the layout is checked here.
const workflow = readFileSync(join(import.meta.dirname, "..", "..", ".github", "workflows", "pages.yml"), "utf8");

// The lines of a job, by its two-space-indented name under jobs:.
function job(name: string): string {
  const lines = workflow.split("\n");
  const start = lines.indexOf(`  ${name}:`);
  expect(start, `job ${name}`).toBeGreaterThan(-1);
  const end = lines.findIndex((line, i) => i > start && /^ {2}\S/.test(line));
  return lines.slice(start, end === -1 ? undefined : end).join("\n");
}

// A job's steps, each from its "- " line to the next.
function steps(jobText: string): string[] {
  return jobText.split(/\n(?= {6}- )/).slice(1);
}

describe("pages.yml", () => {
  const build = job("build");
  const publish = job("publish");

  it("builds with no write access, no token and no install scripts", () => {
    expect(workflow).toMatch(/^permissions:\n {2}contents: read\n/m);
    expect(build).not.toMatch(/permissions:|token|secrets\./i);
    expect(build).toContain("persist-credentials: false");
    expect(build).toContain("run: npm ci --ignore-scripts\n");
    expect(build).toContain("include-hidden-files: false");
  });

  it("publishes only from main, through the docs environment", () => {
    expect(publish).toContain("    if: github.ref == 'refs/heads/main'\n");
    expect(publish).toMatch(/\n {4}environment:\n {6}name: docs\n/);
  });

  it("gives the token to the push step alone, after the site is committed", () => {
    expect(publish).not.toMatch(/^ {4}env:/m);
    const all = steps(publish);
    const withToken = all.filter((s) => /github\.token|secrets\./.test(s));
    expect(withToken).toHaveLength(1);
    expect(withToken[0]).toContain("run: sh site/scripts/publish-pages.sh push ");
    expect(all.at(-1)).toBe(withToken[0]);

    const order = all.map((s) => (s.match(/uses: actions\/([\w-]+)@|publish-pages\.sh (\w+)/) ?? []).slice(1).join(""));
    expect(order).toEqual(["checkout", "download-artifact", "commit", "push"]);
    expect(all[0]).toContain("persist-credentials: false");
  });

  it("keeps the artifact out of the checkout and the git dir out of the artifact", () => {
    expect(publish).toContain("path: ${{ runner.temp }}/site\n");
    expect(publish).toContain('publish-pages.sh commit "$RUNNER_TEMP/site" "$RUNNER_TEMP/pages.git" ');
  });
});
