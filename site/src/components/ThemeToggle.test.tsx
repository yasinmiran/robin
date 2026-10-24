import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { THEME_KEY } from "@/lib/theme";

import { ThemeToggle } from "./ThemeToggle";

/** systemTheme stands in for matchMedia and lets a test flip the system's preference. */
function systemTheme(dark: boolean) {
  const listeners = new Set<(event: MediaQueryListEvent) => void>();
  vi.spyOn(window, "matchMedia").mockImplementation(
    () =>
      ({
        matches: dark,
        addEventListener: (_: string, listener: (event: MediaQueryListEvent) => void) => listeners.add(listener),
        removeEventListener: (_: string, listener: (event: MediaQueryListEvent) => void) => listeners.delete(listener),
      }) as unknown as MediaQueryList,
  );
  return {
    change(matches: boolean) {
      act(() => listeners.forEach((listener) => listener({ matches } as MediaQueryListEvent)));
    },
    listeners,
  };
}

describe("ThemeToggle", () => {
  it("offers the other theme and remembers the switch", async () => {
    systemTheme(false);
    render(<ThemeToggle />);
    const button = screen.getByRole("button", { name: "Switch to dark theme" });

    await userEvent.click(button);
    expect(document.documentElement).toHaveClass("dark");
    expect(localStorage.getItem(THEME_KEY)).toBe("dark");
    expect(screen.getByRole("button", { name: "Switch to light theme" })).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Switch to light theme" }));
    expect(document.documentElement).not.toHaveClass("dark");
    expect(localStorage.getItem(THEME_KEY)).toBe("light");
  });

  it("starts from the theme the head script applied", () => {
    systemTheme(false);
    document.documentElement.classList.add("dark");
    render(<ThemeToggle />);
    expect(screen.getByRole("button", { name: "Switch to light theme" })).toBeInTheDocument();
  });

  it("follows the system until the visitor picks a theme", async () => {
    const system = systemTheme(false);
    render(<ThemeToggle />);

    system.change(true);
    expect(document.documentElement).toHaveClass("dark");
    expect(screen.getByRole("button", { name: "Switch to light theme" })).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Switch to light theme" }));
    system.change(true);
    expect(document.documentElement).not.toHaveClass("dark");
  });

  it("stops listening when it unmounts", () => {
    const system = systemTheme(false);
    const { unmount } = render(<ThemeToggle />);
    expect(system.listeners.size).toBe(1);
    unmount();
    expect(system.listeners.size).toBe(0);
  });

  it("still switches when storage is blocked", async () => {
    systemTheme(false);
    vi.spyOn(window, "localStorage", "get").mockImplementation(() => {
      throw new Error("blocked");
    });
    render(<ThemeToggle />);
    await userEvent.click(screen.getByRole("button", { name: "Switch to dark theme" }));
    expect(document.documentElement).toHaveClass("dark");
  });

  it("is hidden when scripts do not run", () => {
    systemTheme(false);
    render(<ThemeToggle />);
    expect(screen.getByRole("button")).toHaveClass("js-only");
  });
});
