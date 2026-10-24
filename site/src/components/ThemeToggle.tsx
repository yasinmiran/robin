import { useEffect, useState } from "react";

import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import {
  type Theme,
  applyTheme,
  currentTheme,
  readStoredTheme,
  resolveTheme,
  storeTheme,
} from "@/lib/theme";

const SYSTEM_DARK = "(prefers-color-scheme: dark)";

function storage(): Storage | undefined {
  try {
    return window.localStorage;
  } catch {
    return undefined;
  }
}

/**
 * ThemeToggle switches between light and dark and remembers the choice.
 * Until the visitor picks one, the page follows the system, live.
 */
export function ThemeToggle() {
  const [theme, setTheme] = useState<Theme | null>(null);

  useEffect(() => {
    setTheme(currentTheme());
    const media = window.matchMedia(SYSTEM_DARK);
    const follow = (event: MediaQueryListEvent) => {
      if (readStoredTheme(storage())) return;
      const next = resolveTheme(null, event.matches);
      applyTheme(next);
      setTheme(next);
    };
    media.addEventListener("change", follow);
    return () => media.removeEventListener("change", follow);
  }, []);

  const next: Theme = theme === "dark" ? "light" : "dark";
  const label = theme ? `Switch to ${next} theme` : "Switch theme";

  const toggle = () => {
    applyTheme(next);
    storeTheme(storage(), next);
    setTheme(next);
  };

  return (
    <TooltipProvider delayDuration={400}>
      <Tooltip>
        <TooltipTrigger asChild>
          <Button
            type="button"
            variant="ghost"
            size="icon-lg"
            className="js-only text-muted-foreground hover:bg-accent hover:text-foreground"
            aria-label={label}
            onClick={toggle}
          >
            <svg viewBox="0 0 20 20" aria-hidden="true" className="size-[18px]">
              <circle cx="10" cy="10" r="6.5" fill="none" stroke="currentColor" strokeWidth="1.5" />
              <path d="M10 3.5a6.5 6.5 0 0 1 0 13z" fill="currentColor" />
            </svg>
          </Button>
        </TooltipTrigger>
        <TooltipContent side="bottom">{label}</TooltipContent>
      </Tooltip>
    </TooltipProvider>
  );
}
