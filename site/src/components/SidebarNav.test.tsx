import { fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { groups, stayOnPage } from "@/test-utils/nav";

import { SidebarNav } from "./SidebarNav";

beforeEach(() => document.addEventListener("click", stayOnPage));
afterEach(() => document.removeEventListener("click", stayOnPage));

describe("SidebarNav", () => {
  it("lists every page under its section", () => {
    render(<SidebarNav groups={groups} />);
    const nav = screen.getByRole("navigation", { name: "Docs" });
    const started = within(nav).getByRole("list", { name: "Start" });
    expect(within(started).getAllByRole("link").map((link) => link.getAttribute("href"))).toEqual([
      "/introduction",
      "/install",
    ]);
    expect(within(nav).getByRole("list", { name: "Use" })).toHaveTextContent("Sound");
  });

  it("marks the page being read", () => {
    render(<SidebarNav groups={groups} current="install" />);
    expect(screen.getByRole("link", { name: "Install and build" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("link", { name: "Introduction" })).not.toHaveAttribute("aria-current");
  });

  it("tells its container when a link is followed", () => {
    const onNavigate = vi.fn();
    render(<SidebarNav groups={groups} onNavigate={onNavigate} />);
    fireEvent.click(screen.getByRole("link", { name: "Sound" }));
    expect(onNavigate).toHaveBeenCalledOnce();
  });
});
