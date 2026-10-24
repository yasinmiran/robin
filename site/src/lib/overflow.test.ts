import { describe, expect, it, vi } from "vitest";

import { hasMoreRight, watchAllOverflow, watchOverflow } from "./overflow";

function sized(el: HTMLElement, { scrollLeft = 0, clientWidth = 300, scrollWidth = 300 }) {
  Object.defineProperty(el, "clientWidth", { configurable: true, value: clientWidth });
  Object.defineProperty(el, "scrollWidth", { configurable: true, value: scrollWidth });
  el.scrollLeft = scrollLeft;
  Object.defineProperty(el, "scrollLeft", { configurable: true, writable: true, value: scrollLeft });
  return el;
}

describe("hasMoreRight", () => {
  it("is true while content is hidden to the right", () => {
    expect(hasMoreRight({ scrollLeft: 0, clientWidth: 300, scrollWidth: 500 })).toBe(true);
    expect(hasMoreRight({ scrollLeft: 200, clientWidth: 300, scrollWidth: 500 })).toBe(false);
    expect(hasMoreRight({ scrollLeft: 0, clientWidth: 300, scrollWidth: 300 })).toBe(false);
  });

  it("ignores a sub-pixel remainder", () => {
    expect(hasMoreRight({ scrollLeft: 199.5, clientWidth: 300, scrollWidth: 500 })).toBe(false);
  });
});

describe("watchOverflow", () => {
  it("marks the wrapper and follows scrolling", () => {
    const wrapper = document.createElement("div");
    const scroller = sized(document.createElement("pre"), { scrollWidth: 500 });
    const stop = watchOverflow(wrapper, scroller);
    expect(wrapper.dataset.overflow).toBe("more");
    expect(wrapper.hasAttribute("data-scrolled")).toBe(false);

    scroller.scrollLeft = 200;
    scroller.dispatchEvent(new Event("scroll"));
    expect(wrapper.dataset.overflow).toBe("none");
    expect(wrapper.hasAttribute("data-scrolled")).toBe(true);

    stop();
    scroller.scrollLeft = 0;
    scroller.dispatchEvent(new Event("scroll"));
    expect(wrapper.dataset.overflow).toBe("none");
  });

  it("works where ResizeObserver is missing", () => {
    vi.stubGlobal("ResizeObserver", undefined);
    const wrapper = document.createElement("div");
    const stop = watchOverflow(wrapper, sized(document.createElement("pre"), {}));
    expect(wrapper.dataset.overflow).toBe("none");
    stop();
    vi.unstubAllGlobals();
  });
});

describe("watchAllOverflow", () => {
  it("wires every wrapper that has a scroller", () => {
    document.body.innerHTML = `
      <div data-overflow id="a"><pre data-scroller></pre></div>
      <div data-overflow id="b"></div>`;
    sized(document.querySelector<HTMLElement>("#a pre")!, { scrollWidth: 900 });
    watchAllOverflow();
    expect(document.getElementById("a")!.dataset.overflow).toBe("more");
    expect(document.getElementById("b")!.dataset.overflow).toBe("");
    document.body.innerHTML = "";
  });
});
