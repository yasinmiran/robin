import Callout from "@/components/Callout.astro";
import CodeBlock from "@/components/CodeBlock.astro";
import Flag from "@/components/Flag.astro";
import Flags from "@/components/Flags.astro";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

/** components maps MDX elements and the components pages use without importing. */
export const components = {
  Callout,
  Flag,
  Flags,
  pre: CodeBlock,
  table: Table,
  thead: TableHeader,
  tbody: TableBody,
  tr: TableRow,
  th: TableHead,
  td: TableCell,
};
