import { describe, expect, it } from "vitest";

import { siteAddress } from "./site-url";

describe("siteAddress", () => {
  it("serves from the root without SITE_URL", () => {
    expect(siteAddress(undefined)).toEqual({ base: "/" });
    expect(siteAddress("")).toEqual({ base: "/" });
  });

  it.each([
    ["https://robin.example", { site: "https://robin.example", base: "/" }],
    ["https://robin.example/", { site: "https://robin.example", base: "/" }],
    ["https://yasinmiran.github.io/robin", { site: "https://yasinmiran.github.io", base: "/robin" }],
    ["https://yasinmiran.github.io/robin/", { site: "https://yasinmiran.github.io", base: "/robin" }],
    ["http://127.0.0.1:4817/docs/robin//", { site: "http://127.0.0.1:4817", base: "/docs/robin" }],
  ])("%s", (url, address) => {
    expect(siteAddress(url)).toEqual(address);
  });

  it("refuses an address that is not a URL", () => {
    expect(() => siteAddress("robin.example")).toThrow();
  });
});
