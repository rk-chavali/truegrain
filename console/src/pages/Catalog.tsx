import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { CloseButton, SegmentedControl, TextInput } from "@mantine/core";
import { api, type Metric } from "../lib/api";
import { IconSearch } from "../components/icons";
import { Empty, PageHead, Panel, Strip, Tag, SkeletonRows } from "../components/ui";

/*
  The catalog.

  One search across everything the model defines, which is the question
  a newcomer actually has: "is there a metric for X". The model screen
  is a reference; this is a search.

  It also reports the two things a catalog is for and nobody looks at
  until it is too late: what is undocumented, and what is unreachable.
  A metric with no dimensions can only ever be a single total, which is
  usually a modelling mistake rather than a decision.
*/

interface Dimension {
  name: string;
  namespace?: string;
  datatype?: string;
  description?: string;
  is_time?: boolean;
}

type Row =
  | {
      kind: "metric";
      name: string;
      description: string;
      namespace: string;
      extra: number;
      datatype?: string;
    }
  | {
      kind: "dimension";
      name: string;
      description: string;
      namespace: string;
      extra: number;
      datatype?: string;
    };

export function Catalog() {
  const [metrics, setMetrics] = useState<Metric[]>();
  const [dimensions, setDimensions] = useState<Dimension[]>();
  const [error, setError] = useState<string>();
  const [q, setQ] = useState("");
  const [kind, setKind] = useState<"all" | "metric" | "dimension">("all");

  useEffect(() => {
    api
      .get<{ metrics: Metric[] }>("/v1/metrics")
      .then((d) => setMetrics(d.metrics ?? []))
      .catch((err: unknown) =>
        setError(err instanceof Error ? err.message : "Could not load the model."),
      );
    api
      .get<{ dimensions: Dimension[] }>("/v1/dimensions")
      .then((d) => setDimensions(d.dimensions ?? []))
      .catch(() => setDimensions([]));
  }, []);

  /* How many metrics each dimension can group, which is the only
     measure of a dimension's reach the model exposes. */
  const reach = useMemo(() => {
    const m = new Map<string, number>();
    for (const metric of metrics ?? []) {
      for (const d of metric.dimensions) m.set(d, (m.get(d) ?? 0) + 1);
    }
    return m;
  }, [metrics]);

  const rows = useMemo<Row[]>(() => {
    const out: Row[] = [];
    for (const m of metrics ?? []) {
      out.push({
        kind: "metric",
        name: m.name,
        description: m.description ?? "",
        namespace: m.namespace,
        extra: m.dimensions.length,
        datatype: m.datatype,
      });
    }
    for (const d of dimensions ?? []) {
      out.push({
        kind: "dimension",
        name: d.name,
        description: d.description ?? "",
        namespace: d.namespace ?? "",
        extra: reach.get(d.name) ?? 0,
        datatype: d.is_time ? "time" : d.datatype,
      });
    }
    return out;
  }, [metrics, dimensions, reach]);

  const shown = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return rows.filter(
      (r) =>
        (kind === "all" || r.kind === kind) &&
        (!needle ||
          r.name.toLowerCase().includes(needle) ||
          r.description.toLowerCase().includes(needle)),
    );
  }, [rows, q, kind]);

  const undocumented = rows.filter((r) => !r.description.trim()).length;
  const orphaned = (metrics ?? []).filter((m) => m.dimensions.length === 0).length;

  if (error) {
    return (
      <div className="page page-pad">
        <Empty title="No model is loaded">
          <p>{error}</p>
        </Empty>
      </div>
    );
  }

  return (
    <div className="page page-pad">
      <PageHead title="Catalog" />

      <Strip
        specs={[
          { k: "Objects", v: rows.length },
          { k: "Metrics", v: metrics?.length ?? "—" },
          { k: "Dimensions", v: dimensions?.length ?? "—" },
          { k: "Undocumented", v: undocumented, tone: undocumented ? "refused" : "ok" },
          {
            k: "Total only",
            v: orphaned,
            tone: orphaned ? "refused" : "ok",
            title:
              "Metrics with no dimension at all, which can only ever be read as one number",
          },
        ]}
      />

      <div className="row" style={{ marginBottom: "var(--s3)" }}>
        <TextInput
          value={q}
          onChange={(e) => setQ(e.currentTarget.value)}
          placeholder="Search metrics and dimensions"
          aria-label="Search the catalog"
          leftSection={<IconSearch size={13} />}
          rightSection={q ? <CloseButton size="sm" onClick={() => setQ("")} /> : null}
          w="26rem"
          autoFocus
        />
        <SegmentedControl
          value={kind}
          onChange={(v) => setKind(v)}
          data={[
            { value: "all", label: "Everything" },
            { value: "metric", label: "Metrics" },
            { value: "dimension", label: "Dimensions" },
          ]}
        />
      </div>

      {!metrics || !dimensions ? (
        <Panel tight>
          <SkeletonRows rows={8} cols={4} />
        </Panel>
      ) : shown.length === 0 ? (
        <Empty title={`Nothing matches “${q}”`}>
          <p>Names and descriptions are both searched.</p>
        </Empty>
      ) : (
        <Panel tight>
          <table className="grid">
            <thead>
              <tr>
                <th className="shrink">Kind</th>
                <th>Name</th>
                <th>Description</th>
                <th
                  className="shrink num"
                  title="For a metric, how many dimensions it can be grouped by. For a dimension, how many metrics it can group."
                >
                  Reach
                </th>
                <th className="shrink">Type</th>
              </tr>
            </thead>
            <tbody>
              {shown.map((r) => (
                <tr key={`${r.kind}:${r.name}`}>
                  <td>
                    <Tag tone={r.kind === "metric" ? "ok" : undefined}>{r.kind}</Tag>
                  </td>
                  <td className="mono">
                    {r.kind === "metric" ? <Link to="/model">{r.name}</Link> : r.name}
                  </td>
                  <td className="muted">
                    {r.description || <span className="faint">No description</span>}
                  </td>
                  <td className={r.extra === 0 ? "num faint" : "num"}>{r.extra}</td>
                  <td>{r.datatype ? <Tag>{r.datatype}</Tag> : null}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Panel>
      )}
    </div>
  );
}
