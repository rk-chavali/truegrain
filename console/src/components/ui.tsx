import { useMemo, useState, type ReactNode } from "react";
import {
  ActionIcon,
  Alert,
  Badge,
  Menu,
  Paper,
  SegmentedControl,
  Skeleton,
  Table,
  Tooltip,
  useMantineColorScheme,
} from "@mantine/core";

/*
  The primitives every screen is built from.

  Mantine supplies the mechanics. These wrap it so that a screen never
  reaches for a raw library component with ad-hoc props, which is the
  rule Metabase and Superset both enforce and the reason their UIs stay
  coherent across hundreds of files.

  What stays ours: the spec strip, the verdict block, and the semantic
  tones. Those are the design decision; Mantine is the plumbing.
*/

/* ---------- Spec strip ---------- */

export interface Spec {
  k: string;
  v: ReactNode;
  /** Tints the value. Use it when the value is a state, not a count. */
  tone?: "ok" | "refused" | "denied" | "off";
  title?: string;
}

/**
 * The signature of this interface: a dense row of label and value pairs
 * under the page title, values in mono, split by hairlines.
 *
 * It replaced the explanatory paragraph every screen used to open with,
 * and it is where the numbers live. A data tool with no numbers on
 * screen is what made the old console feel empty.
 */
export function Strip({ specs, end }: { specs: Spec[]; end?: ReactNode }) {
  return (
    <div className="strip">
      {specs.map((s) => (
        <div className="strip-cell" key={s.k} title={s.title}>
          <span className="strip-k">{s.k}</span>
          <span className={`strip-v${s.tone ? ` is-${s.tone}` : ""}`}>{s.v}</span>
        </div>
      ))}
      {end ? (
        <>
          <span className="strip-gap" />
          <div className="strip-cell">{end}</div>
        </>
      ) : null}
    </div>
  );
}

/* ---------- Page furniture ---------- */

export function PageHead({
  title,
  note,
  actions,
}: {
  title: string;
  note?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <>
      <div className="page-head">
        <h1>{title}</h1>
        {actions ? <div className="page-actions">{actions}</div> : null}
      </div>
      {note ? <p className="lede">{note}</p> : null}
    </>
  );
}

export function Panel({
  title,
  actions,
  tight,
  children,
}: {
  title?: ReactNode;
  actions?: ReactNode;
  tight?: boolean;
  children: ReactNode;
}) {
  return (
    <Paper component="section" className={tight ? "panel panel-tight" : "panel"}>
      {title ? (
        <header className="panel-head">
          <span>{title}</span>
          {actions ? <span className="spacer">{actions}</span> : null}
        </header>
      ) : null}
      <div className="panel-body">{children}</div>
    </Paper>
  );
}

export function SectionHead({ title, actions }: { title: string; actions?: ReactNode }) {
  return (
    <div className="section-head">
      <h2>{title}</h2>
      {actions ? <span className="spacer">{actions}</span> : null}
    </div>
  );
}

/** An empty state names the next step rather than only reporting nothing. */
export function Empty({
  title,
  children,
  action,
}: {
  title: string;
  children?: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className="empty">
      <h3>{title}</h3>
      {children}
      {action ? (
        <div className="row" style={{ marginTop: "var(--s4)" }}>
          {action}
        </div>
      ) : null}
    </div>
  );
}

export function Notice({
  kind = "error",
  title,
  children,
}: {
  kind?: "error" | "ok" | "warn";
  title?: string;
  children: ReactNode;
}) {
  const color = kind === "ok" ? "brand" : kind === "warn" ? "refused" : "danger";
  return (
    <Alert
      color={color}
      title={title}
      role={kind === "error" ? "alert" : "status"}
      mb="md"
      variant="light"
    >
      {children}
    </Alert>
  );
}

/**
 * A semantic tag.
 *
 * The tones are the product's own vocabulary, not Mantine's palette:
 * refused and denied stay different colours because they are different
 * things, and nothing in a screen picks a colour directly.
 */
export function Tag({
  tone,
  children,
  title,
}: {
  tone?: "ok" | "refused" | "denied" | "danger";
  children: ReactNode;
  title?: string;
}) {
  const color =
    tone === "ok"
      ? "brand"
      : tone === "refused"
        ? "refused"
        : tone === "denied"
          ? "denied"
          : tone === "danger"
            ? "danger"
            : "gray";
  return (
    <Badge variant="light" color={color} title={title} tt="none" fw={500}>
      {children}
    </Badge>
  );
}

export function Segmented<T extends string>({
  value,
  onChange,
  options,
  fullWidth,
}: {
  value: T;
  onChange: (v: T) => void;
  options: { id: T; label: string; count?: number }[];
  fullWidth?: boolean;
}) {
  return (
    <SegmentedControl
      value={value}
      onChange={(v) => onChange(v)}
      fullWidth={fullWidth}
      data={options.map((o) => ({
        value: o.id,
        label:
          o.count === undefined ? (
            o.label
          ) : (
            <span className="row" style={{ gap: "var(--s1)" }}>
              {o.label}
              <span className="num faint">{o.count}</span>
            </span>
          ),
      }))}
    />
  );
}

/** A determinate-feeling wait. Two pixels, not a spinner. */
export function Working() {
  return <div className="working" role="status" aria-label="Loading" />;
}

export function SkeletonRows({ rows = 4, cols = 3 }: { rows?: number; cols?: number }) {
  return (
    <Table>
      <Table.Tbody>
        {Array.from({ length: rows }, (_, r) => (
          <Table.Tr key={r}>
            {Array.from({ length: cols }, (__, c) => (
              <Table.Td key={c}>
                <Skeleton height={10} width={c === 0 ? "60%" : "35%"} radius="xs" />
              </Table.Td>
            ))}
          </Table.Tr>
        ))}
      </Table.Tbody>
    </Table>
  );
}

/* ---------- Data table ---------- */

export interface Column<T> {
  key: string;
  header: string;
  render: (row: T) => ReactNode;
  /** Present makes the column sortable. */
  sort?: (a: T, b: T) => number;
  /** Right-aligns and sets tabular figures, which is what a number wants. */
  numeric?: boolean;
  /** Shrinks the column to its content. */
  shrink?: boolean;
  title?: string;
}

export type Density = "condensed" | "regular" | "relaxed";

const DENSITY_KEY = "truegrain.density.v1";

/**
 * Reads the density the person last chose.
 *
 * Persisted because a preference that resets on every visit is not a
 * preference. The enterprise-table conventions all list this alongside
 * a reset, and it is the cheapest respect a dense tool can pay.
 */
function readDensity(id: string): Density {
  try {
    const all = JSON.parse(localStorage.getItem(DENSITY_KEY) ?? "{}") as Record<
      string,
      Density
    >;
    return all[id] ?? "regular";
  } catch {
    return "regular";
  }
}

function writeDensity(id: string, d: Density) {
  try {
    const all = JSON.parse(localStorage.getItem(DENSITY_KEY) ?? "{}") as Record<
      string,
      Density
    >;
    all[id] = d;
    localStorage.setItem(DENSITY_KEY, JSON.stringify(all));
  } catch {
    // A browser with storage disabled still gets a working table.
  }
}

/**
 * The table this product is mostly made of.
 *
 * Built to the conventions the enterprise-table literature agrees on
 * and the old hand-rolled one missed: numbers right-aligned in a
 * monospace face, a sticky header, sortable columns with a visible
 * indicator, hover affordance, a density the person chooses and keeps,
 * and real loading and empty states rather than a blank box.
 */
export function DataTable<T>({
  id,
  rows,
  columns,
  loading,
  empty,
  rowKey,
  onRowClick,
  maxHeight,
  dense,
}: {
  /** Identifies the table so its density preference survives a reload. */
  id: string;
  rows: T[] | undefined;
  columns: Column<T>[];
  loading?: boolean;
  empty?: ReactNode;
  rowKey: (row: T, index: number) => string;
  onRowClick?: (row: T) => void;
  maxHeight?: string;
  /** Starts condensed, for result grids that can run to hundreds of rows. */
  dense?: boolean;
}) {
  const [density, setDensity] = useState<Density>(() =>
    dense ? "condensed" : readDensity(id),
  );
  const [sortKey, setSortKey] = useState<string>();
  const [desc, setDesc] = useState(false);

  const sorted = useMemo(() => {
    if (!rows) return undefined;
    const column = columns.find((c) => c.key === sortKey);
    if (!column?.sort) return rows;
    const out = [...rows].sort(column.sort);
    return desc ? out.reverse() : out;
  }, [rows, columns, sortKey, desc]);

  function toggleSort(c: Column<T>) {
    if (!c.sort) return;
    if (sortKey === c.key) setDesc((d) => !d);
    else {
      setSortKey(c.key);
      setDesc(false);
    }
  }

  if (loading || !sorted) {
    return <SkeletonRows rows={6} cols={columns.length} />;
  }
  if (sorted.length === 0 && empty) {
    return <>{empty}</>;
  }

  const body = (
    <Table
      className={`grid density-${density}`}
      highlightOnHover={Boolean(onRowClick)}
      stickyHeader
      stickyHeaderOffset={0}
    >
      <Table.Thead>
        <Table.Tr>
          {columns.map((c) => (
            <Table.Th
              key={c.key}
              title={c.title}
              className={[
                c.numeric ? "num" : "",
                c.shrink ? "shrink" : "",
                c.sort ? "sortable" : "",
              ]
                .filter(Boolean)
                .join(" ")}
              aria-sort={
                sortKey === c.key
                  ? desc
                    ? "descending"
                    : "ascending"
                  : c.sort
                    ? "none"
                    : undefined
              }
              onClick={() => toggleSort(c)}
            >
              {c.header}
              {c.sort ? (
                <span className="sort-mark" aria-hidden="true">
                  {sortKey === c.key ? (desc ? "▾" : "▴") : " "}
                </span>
              ) : null}
            </Table.Th>
          ))}
        </Table.Tr>
      </Table.Thead>
      <Table.Tbody>
        {sorted.map((row, i) => (
          <Table.Tr
            key={rowKey(row, i)}
            onClick={onRowClick ? () => onRowClick(row) : undefined}
            style={onRowClick ? { cursor: "pointer" } : undefined}
          >
            {columns.map((c) => (
              <Table.Td key={c.key} className={c.numeric ? "num" : undefined}>
                {c.render(row)}
              </Table.Td>
            ))}
          </Table.Tr>
        ))}
      </Table.Tbody>
    </Table>
  );

  return (
    <>
      {maxHeight ? (
        <Table.ScrollContainer minWidth={0} mah={maxHeight} type="native">
          {body}
        </Table.ScrollContainer>
      ) : (
        body
      )}
      <div className="table-foot">
        <span className="faint small">
          {sorted.length} {sorted.length === 1 ? "row" : "rows"}
        </span>
        <DensityMenu
          value={density}
          onChange={(d) => {
            setDensity(d);
            writeDensity(id, d);
          }}
        />
      </div>
    </>
  );
}

function DensityMenu({ value, onChange }: { value: Density; onChange: (d: Density) => void }) {
  return (
    <Menu position="top-end" withinPortal>
      <Menu.Target>
        <ActionIcon variant="subtle" color="gray" size="sm" aria-label="Row density">
          <RowsIcon />
        </ActionIcon>
      </Menu.Target>
      <Menu.Dropdown>
        <Menu.Label>Row density</Menu.Label>
        {(["condensed", "regular", "relaxed"] as const).map((d) => (
          <Menu.Item key={d} onClick={() => onChange(d)} fw={value === d ? 600 : 400}>
            {d}
          </Menu.Item>
        ))}
      </Menu.Dropdown>
    </Menu>
  );
}

function RowsIcon() {
  return (
    <svg
      width="14"
      height="14"
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      aria-hidden="true"
    >
      <path d="M2 4h12M2 8h12M2 12h12" strokeLinecap="round" />
    </svg>
  );
}

/* ---------- Colour scheme ---------- */

/**
 * Light and dark, because these tools are looked at for eight hours and
 * every reference product ships one. Defaults to the system setting.
 */
export function ColorSchemeToggle() {
  const { colorScheme, setColorScheme } = useMantineColorScheme();
  const next = colorScheme === "dark" ? "light" : "dark";
  return (
    <Tooltip label={`Switch to ${next} mode`} position="top">
      <ActionIcon
        variant="subtle"
        color="gray"
        size="sm"
        aria-label={`Switch to ${next} mode`}
        onClick={() => setColorScheme(next)}
      >
        {colorScheme === "dark" ? <SunIcon /> : <MoonIcon />}
      </ActionIcon>
    </Tooltip>
  );
}

function SunIcon() {
  return (
    <svg
      width="14"
      height="14"
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      strokeLinecap="round"
      aria-hidden="true"
    >
      <circle cx="8" cy="8" r="3" />
      <path d="M8 1v1.6M8 13.4V15M15 8h-1.6M2.6 8H1M12.9 3.1l-1.1 1.1M4.2 11.8l-1.1 1.1M12.9 12.9l-1.1-1.1M4.2 4.2 3.1 3.1" />
    </svg>
  );
}

function MoonIcon() {
  return (
    <svg
      width="14"
      height="14"
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M13.5 9.6A5.8 5.8 0 0 1 6.4 2.5a5.8 5.8 0 1 0 7.1 7.1z" />
    </svg>
  );
}

/* ---------- Formatting ---------- */

export function plural(n: number, one: string, many = one + "s") {
  return `${n} ${n === 1 ? one : many}`;
}

/** A short relative time, for dense tables. */
export function ago(iso?: string): string {
  if (!iso) return "never";
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "never";
  const secs = Math.round((Date.now() - then) / 1000);
  if (secs < 60) return "just now";
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.round(hours / 24);
  if (days < 30) return `${days}d ago`;
  return new Date(iso).toLocaleDateString();
}

/** Seconds as a human duration, for freshness and age readouts. */
export function duration(seconds?: number): string {
  if (seconds === undefined || seconds < 0) return "unknown";
  if (seconds < 90) return `${Math.round(seconds)}s`;
  const mins = seconds / 60;
  if (mins < 90) return `${Math.round(mins)}m`;
  const hours = mins / 60;
  if (hours < 48) return `${Math.round(hours)}h`;
  return `${Math.round(hours / 24)}d`;
}
