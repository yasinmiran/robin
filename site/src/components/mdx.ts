import Callout from "@/components/Callout.astro";
import CodeBlock from "@/components/CodeBlock.astro";
import DocTable from "@/components/DocTable.astro";
import Flag from "@/components/Flag.astro";
import Flags from "@/components/Flags.astro";
import TableCell from "@/components/TableCell.astro";
import { TableBody, TableHead, TableHeader, TableRow } from "@/components/ui/table";

/** components maps MDX elements and the components pages use without importing. */
export const components = {
  Callout,
  Flag,
  Flags,
  pre: CodeBlock,
  table: DocTable,
  thead: TableHeader,
  tbody: TableBody,
  tr: TableRow,
  th: TableHead,
  td: TableCell,
};
