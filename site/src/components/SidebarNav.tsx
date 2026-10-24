import { type NavGroup, docHref } from "@/lib/nav";
import { cn } from "@/lib/utils";

interface SidebarNavProps {
  groups: NavGroup[];
  /** Slug of the page being read; its row is marked current. */
  current?: string;
  /** Called when a link is followed, so a sheet can close. */
  onNavigate?: () => void;
  className?: string;
}

/** SidebarNav lists every docs page, grouped by section, in reading order. */
export function SidebarNav({ groups, current, onNavigate, className }: SidebarNavProps) {
  return (
    <nav aria-label="Docs" className={cn("text-sm", className)}>
      {groups.map((group) => {
        const labelId = `nav-${group.label.toLowerCase().replace(/\s+/g, "-")}`;
        return (
          <div key={group.label} className="mt-6 first:mt-0">
            <p id={labelId} className="mb-1.5 pl-6 text-xs font-[650] tracking-[0.01em] text-muted-foreground">
              {group.label}
            </p>
            <ul aria-labelledby={labelId} className="pr-4">
              {group.items.map((item) => {
                const active = item.slug === current;
                return (
                  <li key={item.slug}>
                    <a
                      href={docHref(item.slug)}
                      aria-current={active ? "page" : undefined}
                      onClick={onNavigate}
                      className={cn(
                        "flex min-h-9 items-center rounded-r-md py-1.5 pr-3 pl-6 text-sidebar-foreground no-underline hover:bg-sidebar-accent focus-visible:-outline-offset-2 pointer-coarse:min-h-11",
                        active && "bg-sidebar-accent font-[650] shadow-[inset_2px_0_0_var(--sidebar-primary)]",
                      )}
                    >
                      {item.title}
                    </a>
                  </li>
                );
              })}
            </ul>
          </div>
        );
      })}
    </nav>
  );
}
