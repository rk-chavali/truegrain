/*
  CSV export.

  Hand-rolled rather than a dependency: RFC 4180 is one quoting rule and
  a line separator, and a package for it would be more bytes than the
  function.

  Two details that matter and are usually got wrong.

  Decimals arrive from the warehouse as strings on purpose, because
  parsing 885.50 into a float to display it is how a trailing zero
  disappears off a money column. So values are written exactly as they
  arrived, never round-tripped through Number.

  A leading =, +, - or @ is prefixed with a single quote. Excel and
  Sheets treat those as formulas, so a dimension value like "=cmd" is a
  spreadsheet injection in anybody who opens the file. The quote is
  stripped by the spreadsheet on display.
*/

/**
 * A warehouse value as text.
 *
 * Every branch is named rather than leaning on String(), because the
 * one String() gets wrong is the one that matters: a JSON or array
 * column is an object, and String() renders every one of them as the
 * same "[object Object]".
 */
function asText(value: unknown): string {
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "bigint") return value.toString();
  if (typeof value === "boolean") return value ? "true" : "false";
  return JSON.stringify(value) ?? "";
}

function field(value: unknown): string {
  if (value === null || value === undefined) return "";
  const text = asText(value);

  const risky = /^[=+\-@\t\r]/.test(text);
  const escaped = risky ? `'${text}` : text;

  if (/[",\r\n]/.test(escaped)) return `"${escaped.replaceAll('"', '""')}"`;
  return escaped;
}

export function toCSV(columns: string[], rows: unknown[][]): string {
  const lines = [columns.map(field).join(",")];
  for (const row of rows) lines.push(row.map(field).join(","));
  // CRLF, which is what RFC 4180 says and what Excel expects.
  return lines.join("\r\n");
}

/** Hands the browser a file without a round trip to the server. */
export function download(filename: string, contents: string, type = "text/csv;charset=utf-8") {
  // The BOM makes Excel read it as UTF-8 rather than the system codepage,
  // which is what turns an accented dimension value into mojibake.
  const blob = new Blob(["\uFEFF" + contents], { type });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  URL.revokeObjectURL(url);
}
