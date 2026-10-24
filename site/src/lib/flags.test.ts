import { describe, expect, it } from "vitest";

import { codeSegments, flagId } from "./flags";

describe("flagId", () => {
  it.each([
    ["--max-size", "flag-max-size"],
    ["--no-browser", "flag-no-browser"],
    ["-v", "flag-v"],
    ["--audio=false", "flag-audio-false"],
    ["--", "flag-ssh-args"],
  ])("%s anchors at #%s", (name, id) => {
    expect(flagId(name)).toBe(id);
  });
});

describe("codeSegments", () => {
  it("splits text on backticks into code and prose", () => {
    expect(codeSegments("`1920x1080`, or `1280x720` on small hosts")).toEqual([
      { code: true, text: "1920x1080" },
      { code: false, text: ", or " },
      { code: true, text: "1280x720" },
      { code: false, text: " on small hosts" },
    ]);
  });

  it("returns plain text as one segment", () => {
    expect(codeSegments("off")).toEqual([{ code: false, text: "off" }]);
  });
});
