import { spawnSync } from "node:child_process";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, beforeEach, describe, expect, it } from "vitest";

const script = join(import.meta.dirname, "publish-pages.sh");

let root: string;
let site: string;
let gitDir: string;
let marker: string;
// git config entries that run a command which leaves the marker behind.
let hostileConfig: string;

// A clean environment: HOME is the test's own, and nothing of the
// developer's git settings leaks in.
function env(extra: Record<string, string> = {}) {
  return { PATH: process.env.PATH ?? "/usr/bin:/bin", HOME: join(root, "home"), ...extra };
}

function run(args: string[], extra: Record<string, string> = {}) {
  return spawnSync("sh", [script, ...args], { env: env(extra), encoding: "utf8" });
}

function git(args: string[], gitDirOf = gitDir) {
  const r = spawnSync("git", args, { env: env({ GIT_DIR: gitDirOf }), encoding: "utf8" });
  expect(r.status, r.stderr).toBe(0);
  return r.stdout.trim();
}

function file(path: string, body = "x") {
  mkdirSync(join(path, ".."), { recursive: true });
  writeFileSync(path, body);
}

function hostileHooks(dir: string) {
  file(join(dir, "pre-commit"), `#!/bin/sh\ntouch '${marker}'\n`);
  chmodSync(join(dir, "pre-commit"), 0o755);
}

beforeEach(() => {
  root = mkdtempSync(join(tmpdir(), "robin-pages-"));
  site = join(root, "site");
  gitDir = join(root, "pages.git");
  marker = join(root, "ran");
  hostileConfig = `[core]\n\tfsmonitor = touch '${marker}' #\n\thooksPath = ${join(root, "hooks")}\n`;
  mkdirSync(join(root, "home"));
  file(join(site, "index.html"), "<h1>Robin</h1>");
  file(join(site, "_astro", "app.js"), "1");
});

afterEach(() => {
  rmSync(root, { recursive: true, force: true });
});

describe("commit", () => {
  it("commits the site with a .nojekyll as one orphan commit on gh-pages", () => {
    const r = run(["commit", site, gitDir, "abc123"]);
    expect(r.status, r.stderr).toBe(0);
    expect(git(["rev-list", "--count", "HEAD"])).toBe("1");
    expect(git(["symbolic-ref", "HEAD"])).toBe("refs/heads/gh-pages");
    expect(git(["log", "-1", "--format=%s|%an"])).toBe("docs: publish abc123|github-actions[bot]");
    expect(git(["ls-tree", "-r", "--name-only", "HEAD"]).split("\n")).toEqual([
      ".nojekyll",
      "_astro/app.js",
      "index.html",
    ]);
    expect(existsSync(join(site, ".git"))).toBe(false);
  });

  it.each([
    ["a .git dir at the top", ".git/config"],
    ["a .git file at the top", ".git"],
    ["a nested .git", "docs/.git/config"],
    ["a .GIT in another case", "x/.GIT/config"],
  ])("refuses a site holding %s and runs nothing from it", (_, path) => {
    file(join(site, path), hostileConfig);
    hostileHooks(join(root, "hooks"));
    if (path.endsWith("/config")) hostileHooks(join(site, path, "..", "hooks"));
    const r = run(["commit", site, gitDir, "abc123"]);
    expect(r.status).not.toBe(0);
    expect(r.stderr).toMatch(/refusing a site that holds .*\.git/i);
    expect(existsSync(gitDir)).toBe(false);
    expect(existsSync(marker)).toBe(false);
  });

  it("ignores the global and system git config", () => {
    writeFileSync(join(root, "home", ".gitconfig"), hostileConfig);
    hostileHooks(join(root, "hooks"));
    const r = run(["commit", site, gitDir, "abc123"]);
    expect(r.status, r.stderr).toBe(0);
    expect(git(["rev-list", "--count", "HEAD"])).toBe("1");
    expect(existsSync(marker)).toBe(false);
  });

  it("refuses a git dir that exists or sits inside the site", () => {
    mkdirSync(gitDir);
    expect(run(["commit", site, gitDir, "abc123"]).stderr).toMatch(/already exists/);
    expect(run(["commit", site, join(site, "pages.git"), "abc123"]).stderr).toMatch(/inside the site/);
    symlinkSync(site, join(root, "alias"));
    expect(run(["commit", site, join(root, "alias", "pages.git"), "abc123"]).stderr).toMatch(/inside the site/);
    expect(existsSync(join(site, "pages.git"))).toBe(false);
  });

  it("checks its arguments", () => {
    expect(run(["commit", site, gitDir]).stderr).toMatch(/usage: commit/);
    expect(run(["commit", join(root, "none"), gitDir, "abc123"]).stderr).toMatch(/no site dir/);
    expect(run(["commit", site, join(root, "none", "pages.git"), "abc123"]).stderr).toMatch(/no parent dir/);
    expect(run([]).stderr).toMatch(/usage: publish-pages.sh/);
    expect(run(["deploy"]).stderr).toMatch(/unknown command deploy/);
  });
});

describe("push", () => {
  let remote: string;

  beforeEach(() => {
    remote = join(root, "remote.git");
    expect(spawnSync("git", ["init", "-q", "--bare", remote], { env: env() }).status).toBe(0);
    expect(run(["commit", site, gitDir, "abc123"]).status).toBe(0);
  });

  it("replaces gh-pages with the one commit and masks the token's header", () => {
    const old = join(root, "old.git");
    file(join(root, "old", "stale.html"));
    expect(run(["commit", join(root, "old"), old, "old"]).status).toBe(0);
    expect(run(["push", old, remote], { TOKEN: "t0ken" }).status).toBe(0);

    const r = run(["push", gitDir, remote], { TOKEN: "t0ken", GITHUB_ACTIONS: "true" });
    expect(r.status, r.stderr).toBe(0);
    const header = Buffer.from("x-access-token:t0ken").toString("base64");
    expect(r.stdout).toBe(`::add-mask::${header}\n`);
    expect(r.stdout + r.stderr).not.toContain("t0ken");
    expect(git(["rev-list", "--count", "gh-pages"], remote)).toBe("1");
    expect(git(["log", "-1", "--format=%s", "gh-pages"], remote)).toBe("docs: publish abc123");
  });

  it("prints nothing outside Actions and runs no hooks", () => {
    writeFileSync(join(root, "home", ".gitconfig"), hostileConfig);
    file(join(root, "hooks", "pre-push"), `#!/bin/sh\ntouch '${marker}'\n`);
    chmodSync(join(root, "hooks", "pre-push"), 0o755);
    const r = run(["push", gitDir, remote], { TOKEN: "t0ken" });
    expect(r.status, r.stderr).toBe(0);
    expect(r.stdout).toBe("");
    expect(existsSync(marker)).toBe(false);
  });

  it("needs a token and a git dir", () => {
    expect(run(["push", gitDir, remote]).stderr).toMatch(/TOKEN is not set/);
    expect(run(["push", join(root, "none"), remote], { TOKEN: "t" }).stderr).toMatch(/no git dir/);
    expect(run(["push", gitDir], { TOKEN: "t" }).stderr).toMatch(/usage: push/);
  });
});
