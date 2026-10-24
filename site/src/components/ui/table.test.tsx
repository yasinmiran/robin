import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow, isNumericCell } from "./table";

describe("Table", () => {
  it("scrolls inside a focusable, labelled region with an edge fade", () => {
    render(
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Encoder</TableHead>
            <TableHead>Time per frame</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          <TableRow>
            <TableCell>openh264</TableCell>
            <TableCell>1.2 to 3.2 ms</TableCell>
          </TableRow>
        </TableBody>
      </Table>,
    );
    const region = screen.getByRole("region", { name: "Table" });
    expect(region).toHaveAttribute("tabindex", "0");
    expect(region).toHaveAttribute("data-scroller");
    expect(region.parentElement).toHaveAttribute("data-overflow", "none");
    expect(screen.getByRole("columnheader", { name: "Encoder" })).toHaveClass("first:sticky");
  });

  it("takes the caller's word on whether a cell is a quantity", () => {
    render(
      <table>
        <tbody>
          <tr>
            <TableCell numeric>
              <span>4.8 ms</span>
            </TableCell>
            <TableCell numeric={false}>10000</TableCell>
          </tr>
        </tbody>
      </table>,
    );
    const [given, overruled] = screen.getAllByRole("cell");
    expect(given).toHaveAttribute("data-numeric", "true");
    expect(overruled).not.toHaveAttribute("data-numeric");
  });

  it("marks quantities and leaves words alone", () => {
    render(
      <table>
        <tbody>
          <tr>
            <TableCell>1.2 to 3.2 ms</TableCell>
            <TableCell>{["10", "000"]}</TableCell>
            <TableCell>openh264</TableCell>
            <TableCell>
              <code>xvfb</code>
            </TableCell>
          </tr>
        </tbody>
      </table>,
    );
    const [measured, joined, word, code] = screen.getAllByRole("cell");
    expect(measured).toHaveAttribute("data-numeric", "true");
    expect(measured).toHaveClass("whitespace-nowrap");
    expect(joined).toHaveAttribute("data-numeric", "true");
    expect(word).not.toHaveAttribute("data-numeric");
    expect(code).not.toHaveAttribute("data-numeric");
  });

  it("can be named for what it holds", () => {
    render(
      <Table label="Encoders compared">
        <tbody />
      </Table>,
    );
    expect(screen.getByRole("region", { name: "Encoders compared" })).toBeInTheDocument();
  });
});

describe("isNumericCell", () => {
  it.each([
    ["10000", true],
    ["4.8 to 5.1 ms", true],
    ["0.17.0", false],
    ["1,000", false],
    ["30 fps", true],
    ["Fedora 43", false],
    ["none (spawn)", false],
    ["", false],
  ])("%s: %s", (text, numeric) => {
    expect(isNumericCell(text)).toBe(numeric);
  });
});
