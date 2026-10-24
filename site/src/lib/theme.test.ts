import { describe, expect, it, vi } from "vitest";

import {
  THEME_KEY,
  applyTheme,
  currentTheme,
  isTheme,
  readStoredTheme,
  resolveTheme,
  storeTheme,
  themeScript,
} from "./theme";

function runThemeScript(systemDark: boolean) {
  vi.spyOn(window, "matchMedia").mockImplementation(
    (query: string) => ({ matches: systemDark && query.includes("dark") }) as MediaQueryList,
  );
  new Function(themeScript)();
}

describe("themeScript", () => {
  it("marks the page as scripted", () => {
    runThemeScript(false);
    expect(document.documentElement).toHaveClass("js");
  });

  it("follows the system when nothing is stored", () => {
    runThemeScript(true);
    expect(document.documentElement).toHaveClass("dark");
    document.documentElement.className = "";
    runThemeScript(false);
    expect(document.documentElement).not.toHaveClass("dark");
  });

  it("prefers the stored choice over the system", () => {
    localStorage.setItem(THEME_KEY, "light");
    runThemeScript(true);
    expect(document.documentElement).not.toHaveClass("dark");
    localStorage.setItem(THEME_KEY, "dark");
    runThemeScript(false);
    expect(document.documentElement).toHaveClass("dark");
  });

  it("survives storage that throws", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    runThemeScript(true);
    expect(document.documentElement).toHaveClass("dark");
  });
});

describe("stored theme", () => {
  it("round-trips a choice", () => {
    storeTheme(localStorage, "dark");
    expect(readStoredTheme(localStorage)).toBe("dark");
  });

  it("ignores values that are not themes", () => {
    localStorage.setItem(THEME_KEY, "sepia");
    expect(readStoredTheme(localStorage)).toBeNull();
    expect(isTheme("sepia")).toBe(false);
    expect(isTheme("light")).toBe(true);
  });

  it("treats missing or throwing storage as no choice", () => {
    expect(readStoredTheme(undefined)).toBeNull();
    const throwing = {
      getItem: () => {
        throw new Error("blocked");
      },
      setItem: () => {
        throw new Error("blocked");
      },
    };
    expect(readStoredTheme(throwing)).toBeNull();
    expect(() => storeTheme(throwing, "dark")).not.toThrow();
  });
});

describe("resolveTheme", () => {
  it("uses the stored choice, else the system", () => {
    expect(resolveTheme("light", true)).toBe("light");
    expect(resolveTheme(null, true)).toBe("dark");
    expect(resolveTheme(null, false)).toBe("light");
  });
});

describe("applyTheme", () => {
  it("sets and clears the dark class", () => {
    applyTheme("dark");
    expect(currentTheme()).toBe("dark");
    applyTheme("light");
    expect(currentTheme()).toBe("light");
  });
});
