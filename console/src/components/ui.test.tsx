import { expect, test, vi } from "vitest";
import { DataTable, Strip, Tag, type Column } from "./ui";
import { renderWithProviders, screen, userEvent, within } from "../test/render";

/*
  The primitives.

  Tested where a bug would be a wrong answer rather than a wrong pixel:
  a sort that reorders the wrong column, a numeric cell that is not
  aligned as one, a tone that collapses refused into denied.
*/

interface Row {
  name: string;
  count: number;
}

const rows: Row[] = [
  { name: "beta", count: 2 },
  { name: "alpha", count: 30 },
  { name: "gamma", count: 1 },
];

const columns: Column<Row>[] = [
  {
    key: "name",
    header: "Name",
    render: (r) => r.name,
    sort: (a, b) => a.name.localeCompare(b.name),
  },
  {
    key: "count",
    header: "Count",
    numeric: true,
    render: (r) => r.count,
    sort: (a, b) => a.count - b.count,
  },
];

function bodyValues(column: number): string[] {
  const table = screen.getByRole("table");
  const body = table.querySelectorAll("tbody tr");
  return [...body].map((tr) => tr.querySelectorAll("td")[column]?.textContent ?? "");
}

test("rows render in the order given until a column is sorted", () => {
  renderWithProviders(
    <DataTable id="t" rows={rows} columns={columns} rowKey={(r) => r.name} />,
  );
  expect(bodyValues(0)).toEqual(["beta", "alpha", "gamma"]);
});

test("clicking a sortable header sorts, and clicking again reverses it", async () => {
  const user = userEvent.setup();
  renderWithProviders(
    <DataTable id="t" rows={rows} columns={columns} rowKey={(r) => r.name} />,
  );

  await user.click(screen.getByRole("columnheader", { name: /name/i }));
  expect(bodyValues(0)).toEqual(["alpha", "beta", "gamma"]);

  await user.click(screen.getByRole("columnheader", { name: /name/i }));
  expect(bodyValues(0)).toEqual(["gamma", "beta", "alpha"]);
});

test("a numeric column sorts numerically, not as text", () => {
  /*
    The bug this prevents: 30 sorting before 2 because they were
    compared as strings, which looks plausible in a small table and is
    wrong in every one.
  */
  const ascending = [...rows].sort(columns[1]!.sort);
  expect(ascending.map((r) => r.count)).toEqual([1, 2, 30]);
});

test("the sort state is announced, so a screen reader knows which column ordered the table", async () => {
  const user = userEvent.setup();
  renderWithProviders(
    <DataTable id="t" rows={rows} columns={columns} rowKey={(r) => r.name} />,
  );

  const header = screen.getByRole("columnheader", { name: /name/i });
  expect(header).toHaveAttribute("aria-sort", "none");

  await user.click(header);
  expect(header).toHaveAttribute("aria-sort", "ascending");
  await user.click(header);
  expect(header).toHaveAttribute("aria-sort", "descending");
});

test("a numeric cell is marked as one, which is what right-aligns it", () => {
  renderWithProviders(
    <DataTable id="t" rows={rows} columns={columns} rowKey={(r) => r.name} />,
  );
  const firstRow = screen.getByRole("table").querySelectorAll("tbody tr")[0]!;
  const cells = firstRow.querySelectorAll("td");
  expect(cells[0]).not.toHaveClass("num");
  expect(cells[1]).toHaveClass("num");
});

test("an empty result shows the empty state rather than a bare table", () => {
  renderWithProviders(
    <DataTable
      id="t"
      rows={[]}
      columns={columns}
      rowKey={(r) => r.name}
      empty={<p>Nothing here</p>}
    />,
  );
  expect(screen.getByText("Nothing here")).toBeInTheDocument();
  expect(screen.queryByRole("table")).not.toBeInTheDocument();
});

test("a row click handler fires with the row that was clicked", async () => {
  const user = userEvent.setup();
  const onRowClick = vi.fn();
  renderWithProviders(
    <DataTable
      id="t"
      rows={rows}
      columns={columns}
      rowKey={(r) => r.name}
      onRowClick={onRowClick}
    />,
  );

  const second = screen.getByRole("table").querySelectorAll("tbody tr")[1]!;
  await user.click(within(second as HTMLElement).getByText("alpha"));

  expect(onRowClick).toHaveBeenCalledWith({ name: "alpha", count: 30 });
});

/*
  The semantic trio.

  The engine goes out of its way never to collapse refused into denied,
  because they are opposite things: one is correctness and one is
  access. A UI that painted both the same would undo that on the only
  screen anybody looks at.
*/
test("refused and denied are different tones", () => {
  const { container } = renderWithProviders(
    <>
      <Tag tone="refused">Refused</Tag>
      <Tag tone="denied">Denied</Tag>
      <Tag tone="ok">Answered</Tag>
    </>,
  );

  /*
    Mantine resolves a colour into CSS custom properties on the element
    rather than into its class, so the style attribute is where the
    difference actually lives. An earlier version of this compared
    classNames and passed for everything, which is worse than no test.
  */
  const styles = [...container.querySelectorAll(".mantine-Badge-root")].map((el) =>
    el.getAttribute("style"),
  );

  expect(new Set(styles).size).toBe(3);
  expect(styles.every((s) => s && s.includes("--badge"))).toBe(true);
});

test("a spec strip renders every label and value it is given", () => {
  renderWithProviders(
    <Strip
      specs={[
        { k: "Rows", v: 5 },
        { k: "Verdict", v: "Answered", tone: "ok" },
      ]}
    />,
  );
  expect(screen.getByText("Rows")).toBeInTheDocument();
  expect(screen.getByText("5")).toBeInTheDocument();
  expect(screen.getByText("Answered")).toBeInTheDocument();
});
