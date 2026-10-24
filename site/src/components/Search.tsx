import { SearchIcon } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { Command, CommandGroup, CommandInput, CommandItem, CommandList } from "@/components/ui/command";
import { Dialog, DialogContent, DialogDescription, DialogTitle, DialogTrigger } from "@/components/ui/dialog";
import {
  type Pagefind,
  type PagefindLoader,
  type SearchResult,
  isApplePlatform,
  isSearchShortcut,
  loadPagefind,
  runSearch,
} from "@/lib/search";
import { cn } from "@/lib/utils";

type IndexState =
  | { status: "idle" | "loading" | "failed"; pagefind?: undefined }
  | { status: "ready"; pagefind: Pagefind };

interface SearchIndex {
  state: IndexState;
  load: () => void;
}

/** useSearchIndex loads the static index once, on first use, and again after a failure. */
function useSearchIndex(loader: PagefindLoader): SearchIndex {
  const [state, setState] = useState<IndexState>({ status: "idle" });
  const pending = useRef<Promise<void> | null>(null);

  const load = useCallback(() => {
    if (pending.current) return;
    setState({ status: "loading" });
    pending.current = loader().then(
      (pagefind) => setState({ status: "ready", pagefind }),
      () => {
        pending.current = null;
        setState({ status: "failed" });
      },
    );
  }, [loader]);

  return { state, load };
}

interface SearchPanelProps {
  index: SearchIndex;
  navigate: (url: string) => void;
  autoFocus?: boolean;
  className?: string;
}

function SearchPanel({ index, navigate, autoFocus, className }: SearchPanelProps) {
  const { state, load } = index;
  const [query, setQuery] = useState("");
  const [results, setResults] = useState<SearchResult[] | null>(null);

  useEffect(load, [load]);

  useEffect(() => {
    if (state.status !== "ready" || !query.trim()) {
      setResults(null);
      return;
    }
    let stale = false;
    runSearch(state.pagefind, query).then(
      (found) => !stale && setResults(found),
      () => !stale && setResults([]),
    );
    return () => {
      stale = true;
    };
  }, [state, query]);

  const announce =
    state.status === "failed"
      ? ""
      : results === null
        ? ""
        : results.length === 1
          ? "1 result"
          : `${results.length} results`;

  return (
    <Command shouldFilter={false} label="Search the docs" className={cn("relative rounded-panel! p-0", className)}>
      {state.status === "loading" && <div className="wire" data-testid="search-wire" aria-hidden="true" />}
      <CommandInput
        value={query}
        onValueChange={setQuery}
        placeholder="Search the docs"
        aria-label="Search the docs"
        autoFocus={autoFocus}
      />
      <CommandList className="max-h-[min(24rem,60vh)] px-1 pb-1">
        {state.status === "failed" ? (
          <div role="alert" className="px-3 py-4 text-sm">
            <p>Search could not load its index. The pages are all in the sidebar.</p>
            <Button type="button" variant="secondary" className="mt-3 h-9 rounded-md px-3.5" onClick={load}>
              Retry
            </Button>
          </div>
        ) : results === null ? (
          <p className="px-3 py-4 text-sm text-muted-foreground">
            {query.trim() && state.status !== "ready"
              ? "Loading the search index."
              : "Search covers every docs page: titles, headings, text and flag names."}
          </p>
        ) : results.length === 0 ? (
          <div className="px-3 py-4 text-sm">
            <p>No results for &ldquo;{query.trim()}&rdquo;.</p>
            <p className="mt-1 text-muted-foreground">
              Search covers every docs page: titles, headings, text and flag names.
            </p>
          </div>
        ) : (
          results.map((result) => (
            <CommandGroup key={result.url}>
              <CommandItem
                value={result.url}
                onSelect={() => navigate(result.url)}
                className="flex-col items-start gap-0.5 rounded-md! px-3 py-2"
              >
                <span className="font-semibold text-foreground">{result.title}</span>
                <span
                  className="search-excerpt line-clamp-2 text-xs text-muted-foreground"
                  dangerouslySetInnerHTML={{ __html: result.excerpt }}
                />
              </CommandItem>
              {result.sections.map((section) => (
                <CommandItem
                  key={section.url}
                  value={section.url}
                  onSelect={() => navigate(section.url)}
                  className="rounded-md! py-1.5 pr-3 pl-6 text-muted-foreground"
                >
                  {section.title}
                </CommandItem>
              ))}
            </CommandGroup>
          ))
        )}
      </CommandList>
      <span role="status" aria-live="polite" className="sr-only">
        {announce}
      </span>
    </Command>
  );
}

function goTo(url: string) {
  window.location.assign(url);
}

interface SearchProps {
  loader?: PagefindLoader;
  navigate?: (url: string) => void;
}

/** Search is the header's search field: a trigger that opens a command dialog. */
export function Search({ loader = loadPagefind, navigate = goTo }: SearchProps) {
  const index = useSearchIndex(loader);
  const [open, setOpen] = useState(false);
  const [shortcut, setShortcut] = useState("Ctrl K");

  useEffect(() => {
    const nav = navigator as Navigator & { userAgentData?: { platform?: string } };
    if (isApplePlatform(nav.userAgentData?.platform ?? nav.platform ?? "")) setShortcut("⌘K");
    const onKey = (event: KeyboardEvent) => {
      if (!isSearchShortcut(event)) return;
      event.preventDefault();
      setOpen(true);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <button
          type="button"
          aria-label="Search the docs"
          aria-keyshortcuts="Control+K /"
          className="js-only flex size-9 items-center justify-center gap-2 rounded-md text-sm text-muted-foreground hover:bg-accent hover:text-foreground nav:h-9 nav:w-60 nav:justify-start nav:border nav:border-input nav:bg-card nav:pr-2 nav:pl-3 nav:hover:bg-card"
        >
          <SearchIcon className="size-4 shrink-0" strokeWidth={1.75} aria-hidden="true" />
          <span className="hidden nav:inline">Search docs</span>
          <kbd className="ml-auto hidden nav:inline">{shortcut}</kbd>
        </button>
      </DialogTrigger>
      <DialogContent
        showCloseButton={false}
        className="top-[12vh] translate-y-0 gap-0 overflow-hidden rounded-panel p-0 shadow-float sm:max-w-xl"
      >
        <DialogTitle className="sr-only">Search the docs</DialogTitle>
        <DialogDescription className="sr-only">
          Type to search. Use the arrow keys to pick a result and Enter to open it.
        </DialogDescription>
        <SearchPanel
          index={index}
          navigate={(url) => {
            setOpen(false);
            navigate(url);
          }}
        />
      </DialogContent>
    </Dialog>
  );
}

/** SearchBox is search laid out in the page, focused on arrival, for the 404 page. */
export function SearchBox({ loader = loadPagefind, navigate = goTo }: SearchProps) {
  const index = useSearchIndex(loader);
  return (
    <SearchPanel
      index={index}
      navigate={navigate}
      autoFocus
      className="js-only border border-input bg-card shadow-none"
    />
  );
}
