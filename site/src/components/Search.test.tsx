import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import type { Pagefind, PagefindData } from "@/lib/search";
import { fakePagefind } from "@/test-utils/pagefind";

import { Search, SearchBox } from "./Search";

const wayland: PagefindData = {
  url: "/attach-wayland.html",
  excerpt: "the screen sharing <mark>portal</mark>",
  meta: { title: "Attaching to Wayland" },
  sub_results: [
    { title: "Attaching to Wayland", url: "/attach-wayland.html", excerpt: "" },
    { title: "The restore token", url: "/attach-wayland.html#later-sessions-the-restore-token", excerpt: "" },
  ],
};

/** deferred gives a test control over when the index finishes loading. */
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function ctrlK() {
  fireEvent.keyDown(window, { key: "k", ctrlKey: true });
}

describe("Search", () => {
  it("shows a search field with its shortcut", () => {
    render(<Search loader={() => new Promise(() => {})} />);
    const trigger = screen.getByRole("button", { name: "Search the docs" });
    expect(trigger).toHaveTextContent("Search docs");
    expect(trigger).toHaveTextContent("Ctrl K");
    expect(trigger).toHaveClass("js-only");
  });

  it("names the Mac shortcut on a Mac", () => {
    vi.spyOn(navigator, "platform", "get").mockReturnValue("MacIntel");
    render(<Search loader={() => new Promise(() => {})} />);
    expect(screen.getByRole("button", { name: "Search the docs" })).toHaveTextContent("⌘K");
  });

  it("runs the wire while the index loads, then finds pages and opens one", async () => {
    const index = deferred<Pagefind>();
    const loader = vi.fn(() => index.promise);
    const navigate = vi.fn();
    render(<Search loader={loader} navigate={navigate} />);

    act(ctrlK);
    const dialog = screen.getByRole("dialog", { name: "Search the docs" });
    expect(screen.getByTestId("search-wire")).toBeInTheDocument();
    expect(loader).toHaveBeenCalledOnce();

    await act(async () => index.resolve(fakePagefind([wayland])));
    expect(screen.queryByTestId("search-wire")).not.toBeInTheDocument();
    expect(dialog).toHaveTextContent("Search covers every docs page");

    await userEvent.type(screen.getByRole("combobox"), "portal");
    const result = await screen.findByRole("option", { name: /Attaching to Wayland/ });
    expect(result.querySelector("mark")).toHaveTextContent("portal");
    expect(screen.getByRole("option", { name: "The restore token" })).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("1 result");

    await userEvent.click(result);
    expect(navigate).toHaveBeenCalledWith("/attach-wayland");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("opens a section result at its heading", async () => {
    const navigate = vi.fn();
    render(<Search loader={async () => fakePagefind([wayland])} navigate={navigate} />);
    await userEvent.click(screen.getByRole("button", { name: "Search the docs" }));
    await userEvent.type(screen.getByRole("combobox"), "token");
    await userEvent.click(await screen.findByRole("option", { name: "The restore token" }));
    expect(navigate).toHaveBeenCalledWith("/attach-wayland#later-sessions-the-restore-token");
  });

  it("says plainly when nothing matches", async () => {
    render(<Search loader={async () => fakePagefind([])} />);
    act(ctrlK);
    await userEvent.type(screen.getByRole("combobox"), "wayland portl");
    expect(await screen.findByText("No results for “wayland portl”.")).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("0 results");
  });

  it("counts several results", async () => {
    const other = { ...wayland, url: "/attach-x11.html", meta: { title: "Attaching to X11" }, sub_results: [] };
    render(<Search loader={async () => fakePagefind([wayland, other])} />);
    act(ctrlK);
    await userEvent.type(screen.getByRole("combobox"), "attach");
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("2 results"));
  });

  it("treats a search that throws as no results", async () => {
    const broken: Pagefind = {
      search: async () => {
        throw new Error("bad index");
      },
    };
    render(<Search loader={async () => broken} />);
    act(ctrlK);
    await userEvent.type(screen.getByRole("combobox"), "x");
    expect(await screen.findByText("No results for “x”.")).toBeInTheDocument();
  });

  it("says the index is still loading if the reader types first", async () => {
    render(<Search loader={() => new Promise(() => {})} />);
    act(ctrlK);
    await userEvent.type(screen.getByRole("combobox"), "x");
    expect(screen.getByText("Loading the search index.")).toBeInTheDocument();
  });

  it("offers a retry when the index fails to load", async () => {
    const loader = vi
      .fn<() => Promise<Pagefind>>()
      .mockRejectedValueOnce(new Error("404"))
      .mockResolvedValueOnce(fakePagefind([wayland]));
    render(<Search loader={loader} />);
    act(ctrlK);

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Search could not load its index. The pages are all in the sidebar.");
    expect(screen.getByRole("status")).toHaveTextContent("");

    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
    expect(loader).toHaveBeenCalledTimes(2);
  });

  it("loads the index once across openings", async () => {
    const loader = vi.fn(async () => fakePagefind([]));
    render(<Search loader={loader} />);
    act(ctrlK);
    await userEvent.keyboard("{Escape}");
    act(ctrlK);
    await screen.findByRole("dialog");
    expect(loader).toHaveBeenCalledOnce();
  });

  it("opens on / but not while typing in a field, and returns focus on Escape", async () => {
    render(
      <>
        <input aria-label="Elsewhere" />
        <Search loader={() => new Promise(() => {})} />
      </>,
    );
    const field = screen.getByRole("textbox", { name: "Elsewhere" });
    fireEvent.keyDown(field, { key: "/" });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();

    const trigger = screen.getByRole("button", { name: "Search the docs" });
    trigger.focus();
    fireEvent.keyDown(trigger, { key: "/" });
    expect(screen.getByRole("dialog")).toBeInTheDocument();

    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });
});

describe("SearchBox", () => {
  it("searches in the page with the field focused", async () => {
    const navigate = vi.fn();
    render(<SearchBox loader={async () => fakePagefind([wayland])} navigate={navigate} />);
    const field = screen.getByRole("combobox");
    expect(field).toHaveFocus();
    await userEvent.type(field, "portal");
    await userEvent.click(await screen.findByRole("option", { name: /Attaching to Wayland/ }));
    expect(navigate).toHaveBeenCalledWith("/attach-wayland");
  });
});
