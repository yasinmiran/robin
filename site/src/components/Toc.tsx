import { useEffect, useState } from "react";

import { type Heading, activeSlug } from "@/lib/toc";
import { cn } from "@/lib/utils";

interface TocProps {
  items: Heading[];
  /** Follow the reader: mark the section on screen as current. */
  spy?: boolean;
  /** Show the "On this page" label; a <details> summary can stand in for it. */
  showLabel?: boolean;
  className?: string;
}

/** Distance from the top of the viewport where a section counts as being read. */
const READING_LINE = 96;

export function Toc({ items, spy = true, showLabel = true, className }: TocProps) {
  const [active, setActive] = useState<string | undefined>(undefined);

  useEffect(() => {
    if (!spy || items.length === 0) return;
    let frame = 0;
    const update = () => {
      frame = 0;
      const positions = items.flatMap((item) => {
        const el = document.getElementById(item.slug);
        return el ? [{ slug: item.slug, top: el.getBoundingClientRect().top }] : [];
      });
      setActive(activeSlug(positions, READING_LINE));
    };
    const schedule = () => {
      if (!frame) frame = requestAnimationFrame(update);
    };
    update();
    window.addEventListener("scroll", schedule, { passive: true });
    window.addEventListener("resize", schedule);
    return () => {
      cancelAnimationFrame(frame);
      window.removeEventListener("scroll", schedule);
      window.removeEventListener("resize", schedule);
    };
  }, [items, spy]);

  if (items.length === 0) return null;

  return (
    <nav aria-label="On this page" className={cn("text-sm", className)}>
      {showLabel && <p className="mb-2 text-xs font-[650] text-muted-foreground">On this page</p>}
      <ol className="border-l border-border">
        {items.map((item) => {
          const current = item.slug === active;
          return (
            <li key={item.slug}>
              <a
                href={`#${item.slug}`}
                aria-current={current ? "true" : undefined}
                className={cn(
                  "-ml-px block border-l py-1 pr-1 text-muted-foreground no-underline hover:text-foreground",
                  item.depth === 3 ? "pl-6" : "pl-3.5",
                  current ? "border-primary font-semibold text-foreground" : "border-transparent",
                )}
              >
                {item.text}
              </a>
            </li>
          );
        })}
      </ol>
    </nav>
  );
}
