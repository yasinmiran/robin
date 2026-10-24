import { act, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { Toc } from "./Toc";

const items = [
  { depth: 2, slug: "ssh", text: "SSH" },
  { depth: 2, slug: "the-page", text: "The page" },
  { depth: 3, slug: "keyboard", text: "Keyboard, pointer and clipboard" },
];

/** headingsAt puts the page's headings in the document at the given tops. */
function headingsAt(tops: number[]) {
  document.body.insertAdjacentHTML(
    "beforeend",
    items.map((item) => `<h2 id="${item.slug}">${item.text}</h2>`).join(""),
  );
  items.forEach((item, index) => {
    vi.spyOn(document.getElementById(item.slug)!, "getBoundingClientRect").mockImplementation(
      () => ({ top: tops[index] }) as DOMRect,
    );
  });
}

describe("Toc", () => {
  it("lists the sections under a labelled navigation landmark", () => {
    render(<Toc items={items} spy={false} />);
    const nav = screen.getByRole("navigation", { name: "On this page" });
    expect(nav).toHaveTextContent("On this page");
    expect(screen.getByRole("link", { name: "SSH" })).toHaveAttribute("href", "#ssh");
    expect(screen.getByRole("link", { name: "Keyboard, pointer and clipboard" })).toHaveClass("pl-6");
    expect(screen.getByRole("link", { name: "SSH" })).toHaveClass("pl-3.5");
  });

  it("can leave the label to a surrounding summary", () => {
    render(<Toc items={items} spy={false} showLabel={false} />);
    expect(screen.getByRole("navigation")).not.toHaveTextContent("On this page");
  });

  it("renders nothing for a page without sections", () => {
    const { container } = render(<Toc items={[]} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("marks the section being read and follows the scroll", async () => {
    headingsAt([40, 500, 900]);
    render(<Toc items={items} />);
    expect(screen.getByRole("link", { name: "SSH" })).toHaveAttribute("aria-current", "true");

    headingsAt([-900, -300, 60]);
    await act(async () => {
      window.dispatchEvent(new Event("scroll"));
      await new Promise((resolve) => requestAnimationFrame(resolve));
    });
    expect(screen.getByRole("link", { name: "Keyboard, pointer and clipboard" })).toHaveAttribute(
      "aria-current",
      "true",
    );
    expect(screen.getByRole("link", { name: "SSH" })).not.toHaveAttribute("aria-current");
    document.body.innerHTML = "";
  });

  it("stops following when it unmounts", () => {
    const remove = vi.spyOn(window, "removeEventListener");
    const { unmount } = render(<Toc items={items} />);
    unmount();
    expect(remove).toHaveBeenCalledWith("scroll", expect.any(Function));
    expect(remove).toHaveBeenCalledWith("resize", expect.any(Function));
  });
});
