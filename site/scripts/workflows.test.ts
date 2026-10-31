import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

// Every workflow in the repository runs on GitHub's machines with a token,
// so the rules that keep that token from untrusted code hold for all of them.
const dir = join(import.meta.dirname, "..", "..", ".github", "workflows");
const workflows = readdirSync(dir)
  .filter((name) => /\.ya?ml$/.test(name))
  .sort()
  .map((name) => ({ name, text: readFileSync(join(dir, name), "utf8") }));

// A workflow's steps, each from its "- " line to the next at the same depth.
function steps(text: string): string[] {
  return text.split(/\n(?= {6}- )/).slice(1);
}

// The script a step runs: its run: line, and the block under it for a "|".
function script(step: string): string {
  const lines = step.split("\n");
  const at = lines.findIndex((line) => /^( {6}- | {8})run: /.test(line));
  if (at === -1) return "";
  const block = lines.slice(at + 1).findIndex((line) => line !== "" && !line.startsWith(" ".repeat(10)));
  return lines.slice(at, block === -1 ? undefined : at + 1 + block).join("\n");
}

describe("workflows", () => {
  it("finds the CI and the docs publish", () => {
    expect(workflows.map((w) => w.name)).toEqual(expect.arrayContaining(["ci.yml", "pages.yml"]));
  });

  describe.each(workflows)("$name", ({ text }) => {
    it("pins every action to a commit", () => {
      const uses = [...text.matchAll(/uses: (\S+)/g)].map((m) => m[1]);
      expect(uses.length).toBeGreaterThan(0);
      for (const ref of uses) expect(ref).toMatch(/^[\w.-]+\/[\w.-]+@[0-9a-f]{40}$/);
    });

    it("reads by default and never runs for a fork with the base's token", () => {
      expect(text).toMatch(/^permissions:\n {2}contents: read\n/m);
      expect(text).not.toMatch(/pull_request_target|workflow_run/);
    });

    it("keeps no credentials in a checkout", () => {
      const checkouts = steps(text).filter((s) => s.includes("uses: actions/checkout@"));
      expect(checkouts.length).toBeGreaterThan(0);
      for (const step of checkouts) expect(step).toContain("persist-credentials: false");
    });

    it("installs npm packages without their install scripts", () => {
      const installs = steps(text)
        .map(script)
        .flatMap((run) => [...run.matchAll(/npm ci\b[^\n)]*/g)].map((m) => m[0]));
      expect(installs.length).toBeGreaterThan(0);
      for (const install of installs) expect(install).toContain("--ignore-scripts");
    });

    it("puts no event data into a script", () => {
      const scripts = steps(text).map(script).filter(Boolean);
      expect(scripts.length).toBeGreaterThan(0);
      for (const run of scripts) expect(run).not.toContain("${{");
    });
  });
});
