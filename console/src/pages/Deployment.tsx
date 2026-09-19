import { useEffect, useState } from "react";
import { api } from "../lib/api";
import { Empty, PageHead, Panel, SectionHead, Strip, Tag, duration } from "../components/ui";

/*
  Deployment.

  What this instance actually enforces, rather than what the product
  claims. The engine's /v1/health has always returned this and nothing
  ever showed it, which is a waste of the most honest endpoint in the
  system: it reports the caveats as prose written by whoever knew the
  limitation, including the ones that are embarrassing.

  Those notes are rendered first and undiluted. An operator who has to
  read the source to discover that no row filtering is applied is an
  operator who finds out too late.
*/

interface Health {
  environment?: string;
  workspace?: string;
  workspace_digest?: string;
  ossie_spec_version?: string;
  executor?: string;
  job_store?: string;
  metric_count?: number;
  dimension_count?: number;
  max_concurrent_queries?: number;
  cache_ttl_seconds?: number;
  enforcement_notes?: string[];
  origin?: {
    repository?: string;
    ref?: string;
    commit?: string;
    subdirectory?: string;
  };
  supported_grains?: string[];
  supported_filter_ops?: string[];
  dialect?: {
    dialect: string;
    column_level_security: boolean;
    row_level_security: boolean;
    note?: string;
  };
  governance?: { resolver: string; column_level: boolean; note?: string };
  namespaces?: {
    name: string;
    available: boolean;
    digest?: string;
    metric_count?: number;
    error?: string;
  }[];
  rollups?: {
    name: string;
    namespace: string;
    source: string;
    metrics: number;
    dimensions: number;
    max_age: string;
    checked: boolean;
    fresh?: boolean;
    age_seconds?: number;
    error?: string;
  }[];
  credentials?: { source: string; active_now: number; rotates_live: boolean };
}

export function Deployment() {
  const [health, setHealth] = useState<Health>();
  const [error, setError] = useState<string>();

  useEffect(() => {
    api
      .get<Health>("/v1/health")
      .then(setHealth)
      .catch((err: unknown) =>
        setError(err instanceof Error ? err.message : "Could not read the deployment."),
      );
  }, []);

  if (error) {
    return (
      <div className="page page-pad">
        <Empty title="Cannot read this deployment">
          <p>{error}</p>
        </Empty>
      </div>
    );
  }

  const governed = health?.governance?.column_level === true;
  const rollups = health?.rollups ?? [];
  const stale = rollups.filter((r) => r.checked && !r.fresh);

  return (
    <div className="page page-pad">
      <PageHead title="Deployment" />

      <Strip
        specs={[
          {
            k: "Environment",
            v: health?.environment ?? "none declared",
            tone: health?.environment ? "ok" : "off",
          },
          { k: "Warehouse", v: health?.dialect?.dialect ?? "—" },
          {
            /*
              Where the model came from, which is the question behind "is the
              thing serving the thing we merged". Without it the answer needs
              ssh and a directory listing, and by then the disputed number is
              a week old.

              The commit, not the ref: a branch moves, and what answered is
              the commit.
            */
            k: "Model source",
            v: health?.origin?.commit
              ? `${health.origin.commit.slice(0, 7)} on ${health.origin.ref || "the default branch"}`
              : "a local path",
            tone: health?.origin?.commit ? "ok" : "off",
          },
          {
            k: "Column policy",
            v: governed ? (health?.governance?.resolver ?? "on") : "none",
            tone: governed ? "ok" : "refused",
          },
          {
            k: "Concurrency cap",
            v: health?.max_concurrent_queries
              ? String(health.max_concurrent_queries)
              : "uncapped",
            tone: health?.max_concurrent_queries ? "ok" : "refused",
          },
          {
            k: "Result cache",
            v: health?.cache_ttl_seconds ? duration(health.cache_ttl_seconds) : "off",
            tone: health?.cache_ttl_seconds ? "ok" : "off",
          },
          {
            k: "Rollups",
            v:
              rollups.length === 0
                ? "none"
                : `${rollups.length - stale.length}/${rollups.length} fresh`,
            tone: rollups.length === 0 ? "off" : stale.length ? "refused" : "ok",
          },
        ]}
      />

      {/*
        The honest part, first. These come from the engine and each one
        was written by whoever knew the limitation.
      */}
      {health?.enforcement_notes?.length ? (
        <Panel title="What is not enforced here">
          <ul
            className="stack"
            style={{ margin: 0, paddingLeft: "var(--s4)", gap: "var(--s2)" }}
          >
            {health.enforcement_notes.map((n) => (
              <li key={n} className="muted">
                {n}
              </li>
            ))}
          </ul>
        </Panel>
      ) : null}

      <SectionHead title="Engine" />
      <Panel tight>
        <table className="grid">
          <tbody>
            <Fact k="Executor" v={health?.executor} />
            <Fact k="Job store" v={health?.job_store} />
            <Fact k="Workspace" v={health?.workspace} />
            <Fact k="Model version" v={health?.workspace_digest} mono />
            <Fact k="Ossie spec" v={health?.ossie_spec_version} mono />
            <Fact k="Grains" v={health?.supported_grains?.join(", ")} mono />
            <Fact k="Filter operators" v={health?.supported_filter_ops?.join(", ")} mono />
          </tbody>
        </table>
      </Panel>

      {health?.dialect?.note ? (
        <>
          <SectionHead title="Warehouse security" />
          <Panel>
            <div className="row" style={{ gap: "var(--s2)", marginBottom: "var(--s3)" }}>
              <Tag tone={health.dialect.column_level_security ? "ok" : undefined}>
                column security:{" "}
                {health.dialect.column_level_security ? "enforced" : "not read"}
              </Tag>
              <Tag tone={health.dialect.row_level_security ? "ok" : undefined}>
                row security: {health.dialect.row_level_security ? "enforced" : "not read"}
              </Tag>
            </div>
            <p className="muted" style={{ marginBottom: 0 }}>
              {health.dialect.note}
            </p>
          </Panel>
        </>
      ) : null}

      {health?.namespaces?.length ? (
        <>
          <SectionHead title="Namespaces" />
          <Panel tight>
            <table className="grid">
              <thead>
                <tr>
                  <th>Name</th>
                  <th className="shrink">State</th>
                  <th className="shrink num">Metrics</th>
                  <th>Digest</th>
                </tr>
              </thead>
              <tbody>
                {health.namespaces.map((n) => (
                  <tr key={n.name}>
                    <td className="mono">{n.name}</td>
                    <td>
                      {n.available ? (
                        <Tag tone="ok">available</Tag>
                      ) : (
                        <Tag tone="danger">failed</Tag>
                      )}
                    </td>
                    <td className="num">{n.metric_count ?? "—"}</td>
                    <td className="mono faint truncate">{n.error ?? n.digest ?? "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </Panel>
        </>
      ) : null}

      <SectionHead title="Rollups" />
      {rollups.length === 0 ? (
        <Empty title="No pre-aggregated tables are declared">
          <p>
            A <span className="mono">rollups.yaml</span> beside the model declares which
            pre-aggregated tables exist and how each proves it is current. Without one, every
            query reads the source tables.
          </p>
        </Empty>
      ) : (
        <Panel tight>
          <table className="grid">
            <thead>
              <tr>
                <th>Rollup</th>
                <th>Source</th>
                <th className="shrink num">Metrics</th>
                <th className="shrink num">Dimensions</th>
                <th className="shrink">Max age</th>
                <th className="shrink">State</th>
              </tr>
            </thead>
            <tbody>
              {rollups.map((r) => (
                <tr key={r.name}>
                  <td className="mono">{r.name}</td>
                  <td className="mono faint truncate">{r.source}</td>
                  <td className="num">{r.metrics}</td>
                  <td className="num">{r.dimensions}</td>
                  <td className="mono">{r.max_age}</td>
                  <td>
                    {!r.checked ? (
                      <Tag>never checked</Tag>
                    ) : r.fresh ? (
                      <Tag tone="ok">fresh, {duration(r.age_seconds)} old</Tag>
                    ) : (
                      <Tag tone="refused">
                        {r.error ? "unreadable" : `stale, ${duration(r.age_seconds)} old`}
                      </Tag>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </Panel>
      )}

      {health?.credentials ? (
        <>
          <SectionHead title="Credentials" />
          <Panel>
            <div className="row" style={{ gap: "var(--s4)" }}>
              <div>
                <div className="strip-k">Source</div>
                <div className="strip-v">{health.credentials.source}</div>
              </div>
              <div>
                <div className="strip-k">Live now</div>
                <div className="strip-v is-ok">{health.credentials.active_now}</div>
              </div>
            </div>
            <p className="faint small" style={{ marginTop: "var(--s3)", marginBottom: 0 }}>
              Two live at once is a rotation in progress. Watch it return to one.
            </p>
          </Panel>
        </>
      ) : null}
    </div>
  );
}

function Fact({ k, v, mono }: { k: string; v?: string; mono?: boolean }) {
  if (!v) return null;
  return (
    <tr>
      <td className="shrink faint">{k}</td>
      <td className={mono ? "mono" : undefined}>{v}</td>
    </tr>
  );
}
