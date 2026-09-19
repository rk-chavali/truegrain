import { describe, expect, test } from "vitest";
import { toCSV } from "./csv";

/*
  CSV export.

  Two of these are security properties rather than formatting ones, and
  both are invisible until somebody opens the file.
*/

test("a value containing a comma or a quote survives a round trip", () => {
  const csv = toCSV(["region", "note"], [["North, East", 'he said "no"']]);
  expect(csv).toBe('region,note\r\n"North, East","he said ""no"""');
});

test("a newline inside a value does not become a new row", () => {
  const csv = toCSV(["a"], [["line one\nline two"]]);
  // One header row and one quoted data row, so three CRLF-delimited
  // segments would mean the value had escaped its cell.
  expect(csv.split("\r\n")).toHaveLength(2);
  expect(csv).toContain('"line one\nline two"');
});

/*
  The one that matters most.

  Excel and Sheets treat a leading =, +, - or @ as a formula, so a
  dimension value like =HYPERLINK(...) or =cmd|'...' is code that runs
  when a colleague opens the export. Prefixing a single quote makes the
  spreadsheet treat it as text and strips the quote on display.
*/
describe("spreadsheet formula injection", () => {
  test.each([
    ["=1+1", "'=1+1"],
    ["+1", "'+1"],
    ["-1", "'-1"],
    ["@SUM(A1)", "'@SUM(A1)"],
    // Quoting is only applied when the value contains a CSV
    // metacharacter. This one has none, so the neutralising prefix is
    // the whole of the defence and the field stays bare.
    ["=cmd|'/c calc'!A1", "'=cmd|'/c calc'!A1"],
  ])("%s is neutralised", (input, expected) => {
    const csv = toCSV(["x"], [[input]]);
    expect(csv).toBe(`x\r\n${expected}`);
  });

  test("an ordinary value is not touched", () => {
    expect(toCSV(["x"], [["North East"]])).toBe("x\r\nNorth East");
  });
});

test("a decimal keeps its trailing zero", () => {
  /*
    Money arrives from the warehouse as a string on purpose: parsing
    885.50 into a float to display it is how the trailing zero
    disappears. The exporter must not round-trip it through Number.
  */
  expect(toCSV(["total"], [["885.50"]])).toBe("total\r\n885.50");
});

test("null and undefined become empty cells rather than the words", () => {
  expect(toCSV(["a", "b"], [[null, undefined]])).toBe("a,b\r\n,");
});

test("a JSON column exports as JSON, not as [object Object]", () => {
  /*
    Postgres jsonb and Snowflake VARIANT both arrive as objects. The
    old exporter ran them through String(), which wrote the same
    "[object Object]" for every distinct value in the column.
  */
  expect(toCSV(["meta"], [[{ tier: "gold" }]])).toBe('meta\r\n"{""tier"":""gold""}"');
  expect(toCSV(["tags"], [[["a", "b"]]])).toBe('tags\r\n"[""a"",""b""]"');
});
