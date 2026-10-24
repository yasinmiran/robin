import { useEffect, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

type State = "idle" | "copied" | "failed";

const LABEL: Record<State, string> = { idle: "Copy", copied: "Copied", failed: "Copy failed" };
const ANNOUNCE: Record<State, string> = {
  idle: "",
  copied: "Copied to the clipboard",
  failed: "Could not copy; select the text and copy it instead",
};
const RESET_MS = 2000;

interface CopyButtonProps {
  /** The text that lands on the clipboard. */
  code: string;
  /** "tab" hangs from a code block's top edge; "field" ends the hero's command field. */
  variant?: "tab" | "field";
  /** Names what is copied, for screen readers: "Copy" alone is ambiguous on a page of code. */
  label?: string;
  className?: string;
}

export function CopyButton({ code, variant = "tab", label = "Copy code", className }: CopyButtonProps) {
  const [state, setState] = useState<State>("idle");
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);

  useEffect(() => () => clearTimeout(timer.current), []);

  const copy = async () => {
    clearTimeout(timer.current);
    try {
      await navigator.clipboard.writeText(code);
      setState("copied");
    } catch {
      setState("failed");
    }
    timer.current = setTimeout(() => setState("idle"), RESET_MS);
  };

  return (
    <>
      <Button
        type="button"
        variant="ghost"
        aria-label={state === "idle" ? label : LABEL[state]}
        onClick={copy}
        className={cn(
          "js-only font-sans text-xs font-semibold text-code-muted hover:text-code-foreground",
          variant === "tab" &&
            "absolute top-px right-4 z-[2] h-7 rounded-t-none rounded-b-[8px] bg-white/7 px-3 hover:bg-white/12",
          variant === "field" &&
            "h-full self-stretch rounded-none rounded-r-[9px] border-0 border-l border-white/8 px-4 text-sm hover:bg-white/5",
          className,
        )}
      >
        {LABEL[state]}
      </Button>
      <span role="status" aria-live="polite" className="sr-only">
        {ANNOUNCE[state]}
      </span>
    </>
  );
}
