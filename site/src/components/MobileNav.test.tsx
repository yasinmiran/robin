import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { groups, stayOnPage } from "@/test-utils/nav";

import { MobileNav } from "./MobileNav";

beforeEach(() => document.addEventListener("click", stayOnPage));
afterEach(() => document.removeEventListener("click", stayOnPage));

describe("MobileNav", () => {
  it("opens the sidebar in a sheet from the menu button", async () => {
    render(<MobileNav groups={groups} current="sound" />);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Open navigation" }));
    const sheet = screen.getByRole("dialog", { name: "Navigation" });
    const nav = within(sheet).getByRole("navigation", { name: "Docs" });
    expect(within(nav).getByRole("link", { name: "Sound" })).toHaveAttribute("aria-current", "page");
    expect(within(sheet).getByRole("link", { name: "Robin home" })).toHaveAttribute("href", "/");
  });

  it("closes when a page is picked", async () => {
    render(<MobileNav groups={groups} />);
    await userEvent.click(screen.getByRole("button", { name: "Open navigation" }));
    await userEvent.click(screen.getByRole("link", { name: "Install and build" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("closes from the home link and on Escape, returning focus to the button", async () => {
    render(<MobileNav groups={groups} />);
    const button = screen.getByRole("button", { name: "Open navigation" });

    await userEvent.click(button);
    await userEvent.click(screen.getByRole("link", { name: "Robin home" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();

    await userEvent.click(button);
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(button).toHaveFocus();
  });
});
