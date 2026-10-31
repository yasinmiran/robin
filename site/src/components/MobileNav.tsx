import { MenuIcon } from "lucide-react";
import { useState } from "react";

import { Lockup } from "@/components/Logo";
import { SidebarNav } from "@/components/SidebarNav";
import { Button } from "@/components/ui/button";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Sheet, SheetContent, SheetDescription, SheetTitle, SheetTrigger } from "@/components/ui/sheet";
import { withBase } from "@/lib/base";
import type { NavGroup } from "@/lib/nav";

interface MobileNavProps {
  groups: NavGroup[];
  current?: string;
}

/** MobileNav puts the sidebar in a sheet behind a menu button on narrow screens. */
export function MobileNav({ groups, current }: MobileNavProps) {
  const [open, setOpen] = useState(false);

  return (
    <Sheet open={open} onOpenChange={setOpen}>
      <SheetTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="icon-lg"
          aria-label="Open navigation"
          className="js-only -ml-2 text-muted-foreground hover:bg-accent hover:text-foreground"
        >
          <MenuIcon className="size-[18px]" strokeWidth={1.5} aria-hidden="true" />
        </Button>
      </SheetTrigger>
      <SheetContent
        side="left"
        className="w-[min(18.5rem,85vw)] gap-0 border-sidebar-border bg-sidebar p-0 shadow-float"
      >
        <div className="flex h-14 shrink-0 items-center border-b border-sidebar-border px-6">
          <SheetTitle className="sr-only">Navigation</SheetTitle>
          <SheetDescription className="sr-only">Every page of the Robin docs.</SheetDescription>
          <a href={withBase("/")} aria-label="Robin home" onClick={() => setOpen(false)} className="flex items-center text-foreground">
            <Lockup size={24} />
          </a>
        </div>
        <ScrollArea className="min-h-0 flex-1">
          <SidebarNav groups={groups} current={current} onNavigate={() => setOpen(false)} className="py-6" />
        </ScrollArea>
      </SheetContent>
    </Sheet>
  );
}
