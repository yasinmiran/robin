import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

function read(name: string) {
  return readFileSync(fileURLToPath(new URL(name, import.meta.url)), "utf8");
}

function declarations(block: string): string[] {
  return [...block.matchAll(/(--[\w-]+):\s*([^;]+);/g)].map(([, name, value]) => `${name}: ${value.trim()}`);
}

describe("no-script.css", () => {
  it("repeats the dark tokens exactly, so the page without JavaScript matches .dark", () => {
    const dark = read("./tokens.css").match(/\n\.dark \{([^}]*)\}/)?.[1] ?? "";
    const noScript = read("./no-script.css").match(/:root:not\(\.js\) \{([^}]*)\}/)?.[1] ?? "";
    expect(declarations(dark).length).toBeGreaterThan(30);
    expect(declarations(noScript)).toEqual(declarations(dark));
  });
});
