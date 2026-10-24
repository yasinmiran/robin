type Scroller = Pick<HTMLElement, "scrollLeft" | "clientWidth" | "scrollWidth">;

/** hasMoreRight is true while content is hidden past the scroller's right edge. */
export function hasMoreRight({ scrollLeft, clientWidth, scrollWidth }: Scroller): boolean {
  return scrollLeft + clientWidth < scrollWidth - 1;
}

/**
 * watchOverflow keeps data-overflow on wrapper set to "more" while its
 * scroller hides content to the right, which draws the edge fade, and sets
 * data-scrolled once it has scrolled away from the left edge, which draws a
 * table's sticky column edge.
 */
export function watchOverflow(wrapper: HTMLElement, scroller: HTMLElement): () => void {
  const update = () => {
    wrapper.dataset.overflow = hasMoreRight(scroller) ? "more" : "none";
    wrapper.toggleAttribute("data-scrolled", scroller.scrollLeft > 0);
  };
  update();
  scroller.addEventListener("scroll", update, { passive: true });
  const resize = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(update);
  resize?.observe(scroller);
  return () => {
    scroller.removeEventListener("scroll", update);
    resize?.disconnect();
  };
}

/** watchAllOverflow wires every [data-overflow] wrapper under root to its [data-scroller]. */
export function watchAllOverflow(root: ParentNode = document): void {
  for (const wrapper of root.querySelectorAll<HTMLElement>("[data-overflow]")) {
    const scroller = wrapper.querySelector<HTMLElement>("[data-scroller]");
    if (scroller) watchOverflow(wrapper, scroller);
  }
}
