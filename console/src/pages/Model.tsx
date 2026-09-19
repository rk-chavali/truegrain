import { useEffect, useMemo, useState } from "react";
import { CloseButton, Drawer, TextInput } from "@mantine/core";
import { api, type Metric } from "../lib/api";
import { DataTable, Empty, PageHead, Panel, Strip, Tag, type Column } from "../components/ui";
import { IconSearch } from "../components/icons";
import { useSession } from "../lib/session";

/*
  The model.

  A table rather than a stack of cards: this is a reference screen, and
  somebody scanning sixty metrics for one wants rows. Selecting a row
  opens a drawer with the dimensions it can be grouped by, which is the
  real content, because that list is a property of the join graph rather
  than of the metric.
*/

export function Model() {
  const { modelName, modelVersion } = useSession();
  const [metrics, setMetrics] = useState<Metric[]>();
  const [dimensions, setDimensions] = useState<string[]>();
  const [error, setError] = useState<string>();
  const [filter, setFilter] = useState("");
  const [selected, setSelected] = useState<Metric>();

  useEffect(() => {
    api
      .get<{ metrics: Metric[] }>("/v1/metrics")
      .then((d) => setMetrics(d.metrics ?? []))
      .catch((err: unknown) =>
        setError(err instanceof Error ? err.message : "Could not load the model."),
      );
    api
      .get<{ dimensions: { name: string }[] }>("/v1/dimensions")
      .then((d) => setDimensions((d.dimensions ?? []).map((x) => x.name)))
      .catch(() => setDimensions([]));
  }, []);

  const shown = useMemo(() => {
    if (!metrics) return undefined;
    const q = filter.trim().toLowerCase();
    if (!q) return metrics;
    return metrics.filter(
      (m) =>
        m.name.toLowerCase().includes(q) ||
        m.description?.toLowerCase().includes(q) ||
        m.synonyms?.some((s) => s.toLowerCase().includes(q)),
    );
  }, [metrics, filter]);

  const undocumented = useMemo(
    () => (metrics ?? []).filter((m) => !m.description?.trim()).length,
    [metrics],
  );

  const columns: Column<Metric>[] = [
    {
      key: "name",
      header: "Metric",
      render: (m) => <span className="mono">{m.name}</span>,
      sort: (a, b) => a.name.localeCompare(b.name),
    },
    {
      key: "description",
      header: "Description",
      render: (m) =>
        m.description ? (
          <span className="muted">{m.description}</span>
        ) : (
          <span className="faint">No description</span>
        ),
    },
    {
      key: "groupings",
      header: "Groupings",
      numeric: true,
      shrink: true,
      title: "How many dimensions this metric can be grouped by without inflating",
      render: (m) => m.dimensions.length,
      sort: (a, b) => a.dimensions.length - b.dimensions.length,
    },
    {
      key: "type",
      header: "Type",
      shrink: true,
      render: (m) => (m.datatype ? <Tag>{m.datatype}</Tag> : null),
    },
  ];

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
      <PageHead title="Model" />

      <Strip
        specs={[
          { k: "Namespace", v: modelName ?? "—" },
          { k: "Metrics", v: metrics?.length ?? "—" },
          { k: "Dimensions", v: dimensions?.length ?? "—" },
          {
            k: "Undocumented",
            v: undocumented,
            tone: undocumented > 0 ? "refused" : "ok",
            title:
              "An agent picks a metric by its description, so one without a description is hard to choose correctly",
          },
          { k: "Version", v: modelVersion ? modelVersion.slice(0, 12) : "—" },
        ]}
      />

      <TextInput
        value={filter}
        onChange={(e) => setFilter(e.currentTarget.value)}
        placeholder="Filter by name, description or synonym"
        aria-label="Filter metrics"
        leftSection={<IconSearch size={13} />}
        rightSection={filter ? <CloseButton size="sm" onClick={() => setFilter("")} /> : null}
        mb="md"
        maw="26rem"
      />

      <Panel tight>
        <DataTable
          id="model.metrics"
          rows={shown}
          columns={columns}
          rowKey={(m) => m.name}
          onRowClick={setSelected}
          empty={
            <Empty title={`Nothing matches “${filter}”`}>
              <p>Metrics are searched by name, description and synonym.</p>
            </Empty>
          }
        />
      </Panel>

      <Drawer
        opened={Boolean(selected)}
        onClose={() => setSelected(undefined)}
        position="right"
        size="22rem"
        title={<span className="mono">{selected?.name}</span>}
      >
        {selected ? <Detail metric={selected} /> : null}
      </Drawer>
    </div>
  );
}

function Detail({ metric }: { metric: Metric }) {
  return (
    <>
      <p className="muted">
        {metric.description || (
          <span className="faint">
            No description. An agent picks a metric by its description, so this one is hard to
            choose correctly.
          </span>
        )}
      </p>

      {metric.synonyms?.length ? (
        <p className="small faint">Also called {metric.synonyms.join(", ")}.</p>
      ) : null}

      <h4 style={{ margin: "var(--s4) 0 var(--s2)", color: "var(--ink-3)" }}>
        Can be grouped by
      </h4>
      {metric.dimensions.length === 0 ? (
        <p className="faint small">Nothing. This metric can only be read as a single total.</p>
      ) : (
        <div className="stack" style={{ gap: "2px" }}>
          {metric.dimensions.map((d) => (
            <div key={d} className="mono small truncate" title={d}>
              {d}
            </div>
          ))}
        </div>
      )}

      <p className="faint small" style={{ marginTop: "var(--s4)", marginBottom: 0 }}>
        A dimension missing here is not an oversight. It is a grouping that would inflate the
        number, so the planner does not offer it.
      </p>
    </>
  );
}
