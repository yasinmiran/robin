import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { LOCKUP_WIDTH, OPENING } from "@/lib/logo";

import { Lockup, Mark } from "./Logo";

describe("Mark", () => {
  it("is decorative by default and draws in the theme's colours", () => {
    const { container } = render(<Mark size={16} />);
    const svg = container.querySelector("svg")!;
    expect(svg).toHaveAttribute("aria-hidden", "true");
    expect(svg).toHaveAttribute("width", "16");
    const fills = [...svg.querySelectorAll("path")].map((path) => path.getAttribute("fill"));
    expect(fills).toEqual(["var(--tile)", "var(--mint)", "var(--tile)"]);
    expect(svg.querySelector(`path[d="${OPENING}"]`)).not.toBeNull();
  });

  it("is an image with a name when given a title", () => {
    render(<Mark title="Robin" />);
    expect(screen.getByRole("img", { name: "Robin" })).toBeInTheDocument();
  });
});

describe("Lockup", () => {
  it("keeps the lockup's proportions at any height", () => {
    const { container } = render(<Lockup size={44} className="hero" />);
    const svg = container.querySelector("svg")!;
    expect(svg).toHaveAttribute("height", "44");
    expect(svg).toHaveAttribute("width", String((44 * LOCKUP_WIDTH) / 32));
    expect(svg).toHaveClass("hero");
  });

  it("sets the wordmark in currentColor", () => {
    const { container } = render(<Lockup title="Robin" />);
    expect(container.querySelector('path[fill="currentColor"]')).not.toBeNull();
    expect(screen.getByRole("img", { name: "Robin" })).toBeInTheDocument();
  });
});
