import * as React from "react"
import { cn } from "cn"

// Docs tables: hairlines only, no zebra and no hover fill. A wide table
// scrolls inside a focusable region with an edge fade, and its first column
// stays put while the rest scrolls.

function Table({
  className,
  label = "Table",
  ...props
}: React.ComponentProps<"table"> & { label?: string }) {
  return (
    <div data-slot="table-container" className="table-wrap" data-overflow="none">
      <div
        className="table-scroll"
        data-scroller=""
        tabIndex={0}
        role="region"
        aria-label={label}
      >
        <table
          data-slot="table"
          className={cn(
            "w-full border-collapse text-sm leading-normal tabular-nums",
            className
          )}
          {...props}
        />
      </div>
    </div>
  )
}

function TableHeader(props: React.ComponentProps<"thead">) {
  return <thead data-slot="table-header" {...props} />
}

function TableBody(props: React.ComponentProps<"tbody">) {
  return <tbody data-slot="table-body" {...props} />
}

function TableRow({ className, ...props }: React.ComponentProps<"tr">) {
  return (
    <tr
      data-slot="table-row"
      className={cn("border-b border-border", className)}
      {...props}
    />
  )
}

const cell =
  "py-2.5 pr-4 align-top last:pr-0 first:sticky first:left-0 first:z-[1] first:bg-background"

function TableHead({ className, ...props }: React.ComponentProps<"th">) {
  return (
    <th
      data-slot="table-head"
      className={cn(
        cell,
        "text-left text-xs font-[650] whitespace-nowrap text-muted-foreground",
        className
      )}
      {...props}
    />
  )
}

/**
 * isNumericCell is true for a quantity such as 4.8 to 5.1 ms or 10000. A
 * version like 0.17.0 is a name, not a quantity, and stays left aligned.
 */
function isNumericCell(text: string): boolean {
  const number = String.raw`\d+(?:\.\d+)?`
  return new RegExp(
    String.raw`^${number}(?:\s*(?:to|-)\s*${number})?\s*(?:ms|s|%|fps|kbps|KiB|MiB)?$`
  ).test(text.trim())
}

function textOf(node: React.ReactNode): string {
  if (typeof node === "string" || typeof node === "number") return String(node)
  if (Array.isArray(node)) return node.map(textOf).join("")
  return ""
}

function TableCell({
  className,
  children,
  numeric = isNumericCell(textOf(children)),
  ...props
}: React.ComponentProps<"td"> & {
  /**
   * Marks a quantity, worked out from text children when not given. A column
   * of nothing but quantities is right aligned, header included (content.css).
   */
  numeric?: boolean
}) {
  return (
    <td
      data-slot="table-cell"
      data-numeric={numeric || undefined}
      className={cn(cell, numeric && "whitespace-nowrap", className)}
      {...props}
    >
      {children}
    </td>
  )
}

export { Table, TableHeader, TableBody, TableHead, TableRow, TableCell, isNumericCell }
