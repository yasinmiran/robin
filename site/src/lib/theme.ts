export type Theme = "light" | "dark";

export const THEME_KEY = "robin-theme";

/**
 * themeScript runs inline in <head>, before first paint: it applies the
 * stored theme, or the system's when the visitor has not picked one, and
 * marks the page as scripted so controls that need JavaScript show.
 */
export const themeScript = `(function () {
  var root = document.documentElement;
  root.classList.add("js");
  var stored = null;
  try { stored = localStorage.getItem("${THEME_KEY}"); } catch (e) {}
  var dark = stored === "dark" || (stored !== "light" && matchMedia("(prefers-color-scheme: dark)").matches);
  root.classList.toggle("dark", dark);
})();`;

export function isTheme(value: unknown): value is Theme {
  return value === "light" || value === "dark";
}

export function readStoredTheme(storage: Pick<Storage, "getItem"> | undefined): Theme | null {
  try {
    const value = storage?.getItem(THEME_KEY);
    return isTheme(value) ? value : null;
  } catch {
    return null;
  }
}

export function storeTheme(storage: Pick<Storage, "setItem"> | undefined, theme: Theme): void {
  try {
    storage?.setItem(THEME_KEY, theme);
  } catch {
    // Storage can be disabled; the choice then lasts until the page closes.
  }
}

export function resolveTheme(stored: Theme | null, systemDark: boolean): Theme {
  return stored ?? (systemDark ? "dark" : "light");
}

export function applyTheme(theme: Theme, root: HTMLElement = document.documentElement): void {
  root.classList.toggle("dark", theme === "dark");
}

export function currentTheme(root: HTMLElement = document.documentElement): Theme {
  return root.classList.contains("dark") ? "dark" : "light";
}
