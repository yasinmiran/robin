import { act, fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { CopyButton } from "./CopyButton";

function clipboard(writeText: (text: string) => Promise<void>) {
  vi.spyOn(navigator, "clipboard", "get").mockReturnValue({ writeText } as unknown as Clipboard);
}

async function click(button: HTMLElement) {
  await act(async () => {
    fireEvent.click(button);
  });
}

describe("CopyButton", () => {
  it("copies the code, says so, and resets after two seconds", async () => {
    vi.useFakeTimers();
    const writeText = vi.fn(async () => {});
    clipboard(writeText);
    render(<CopyButton code="robin gui alice@build-box" label="Copy command" />);

    await click(screen.getByRole("button", { name: "Copy command" }));
    expect(writeText).toHaveBeenCalledWith("robin gui alice@build-box");
    expect(screen.getByRole("button", { name: "Copied" })).toHaveTextContent("Copied");
    expect(screen.getByRole("status")).toHaveTextContent("Copied to the clipboard");

    act(() => vi.advanceTimersByTime(2000));
    expect(screen.getByRole("button", { name: "Copy command" })).toHaveTextContent("Copy");
    expect(screen.getByRole("status")).toHaveTextContent("");
  });

  it("says when the clipboard refuses", async () => {
    clipboard(async () => {
      throw new Error("denied");
    });
    render(<CopyButton code="make" />);
    await click(screen.getByRole("button", { name: "Copy code" }));
    expect(screen.getByRole("button", { name: "Copy failed" })).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("Could not copy");
  });

  it("restarts the reset timer on a second click", async () => {
    vi.useFakeTimers();
    clipboard(async () => {});
    render(<CopyButton code="make" />);
    await click(screen.getByRole("button", { name: "Copy code" }));
    act(() => vi.advanceTimersByTime(1500));
    await click(screen.getByRole("button", { name: "Copied" }));
    act(() => vi.advanceTimersByTime(1500));
    expect(screen.getByRole("button", { name: "Copied" })).toBeInTheDocument();
  });

  it("hangs from a code block or ends a command field", () => {
    clipboard(async () => {});
    const { rerender } = render(<CopyButton code="make" />);
    expect(screen.getByRole("button")).toHaveClass("absolute", "rounded-t-none", "js-only");
    rerender(<CopyButton code="make" variant="field" />);
    expect(screen.getByRole("button")).toHaveClass("self-stretch", "border-l");
  });

  it("clears its timer when it unmounts", async () => {
    vi.useFakeTimers();
    clipboard(async () => {});
    const { unmount } = render(<CopyButton code="make" />);
    await click(screen.getByRole("button"));
    unmount();
    expect(vi.getTimerCount()).toBe(0);
  });
});
