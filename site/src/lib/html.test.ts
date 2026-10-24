import { describe, expect, it } from "vitest";

import { codeText, decodeEntities, escapeHtml, tableLabel, withLead } from "./html";

describe("decodeEntities", () => {
  it("decodes named, decimal and hex entities", () => {
    expect(decodeEntities("&lt;host&gt; &amp; &quot;a&quot; &apos;b&#39; &#x2F; &nbsp;")).toBe(
      "<host> & \"a\" 'b' /  ",
    );
  });

  it("leaves unknown entities as they are", () => {
    expect(decodeEntities("&bogus; &#xZZ;")).toBe("&bogus; &#xZZ;");
  });
});

describe("escapeHtml", () => {
  it("escapes markup", () => {
    expect(escapeHtml(`<b class="x">&</b>`)).toBe("&lt;b class=&quot;x&quot;&gt;&amp;&lt;/b&gt;");
  });
});

describe("codeText", () => {
  it("strips Shiki's markup and decodes the text", () => {
    const html =
      '<code><span class="line"><span style="color:var(--x)">robin</span><span> gui &lt;host&gt;</span></span>\n<span class="line">make</span>\n</code>';
    expect(codeText(html)).toBe("robin gui <host>\nmake");
  });

  it("leaves out the prompt, which is not part of the command", () => {
    const html = '<span class="code-comment"># a desktop</span>\n<span class="code-prompt">$ </span>robin gui box';
    expect(codeText(html)).toBe("# a desktop\nrobin gui box");
  });
});

describe("withLead", () => {
  it("runs the lead into the first paragraph", () => {
    expect(withLead("<p>To open your browser.</p><p>More.</p>", "Shared client.")).toBe(
      '<p><b class="callout-lead">Shared client.</b> To open your browser.</p><p>More.</p>',
    );
  });

  it("keeps paragraph attributes and escapes the lead", () => {
    expect(withLead(' <p class="x">Body</p>', "<A & B>")).toBe(
      ' <p class="x"><b class="callout-lead">&lt;A &amp; B&gt;</b> Body</p>',
    );
  });

  it("gives the lead its own paragraph when the body does not start with one", () => {
    expect(withLead("<ul><li>x</li></ul>", "Note.")).toBe('<p><b class="callout-lead">Note.</b></p><ul><li>x</li></ul>');
  });

  it("returns the body untouched without a lead", () => {
    expect(withLead("<p>Body</p>", undefined)).toBe("<p>Body</p>");
  });
});

describe("tableLabel", () => {
  it("joins the column headings", () => {
    const html =
      '<thead><tr><th class="x">Package manager</th><th>Xvfb</th><th><code>&lt;dir&gt;</code> path</th></tr></thead>' +
      "<tbody><tr><th>not a heading</th><td>apt</td></tr></tbody>";
    expect(tableLabel(html)).toBe("Package manager, Xvfb, <dir> path");
  });

  it("falls back to Table without headings", () => {
    expect(tableLabel("<tbody><tr><td>1</td></tr></tbody>")).toBe("Table");
    expect(tableLabel("<thead><tr><th> </th></tr></thead>")).toBe("Table");
  });
});
