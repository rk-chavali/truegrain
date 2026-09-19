import { useCallback, useEffect, useState } from "react";
import { Button } from "@mantine/core";
import { api, ApiError, type Namespace } from "../lib/api";
import {
  DataTable,
  Empty,
  PageHead,
  Panel,
  Strip,
  Tag,
  Working,
  ago,
  duration,
  plural,
} from "../components/ui";
import type { Column } from "../components/ui";

/*
  Checks.

  Four questions about the model this deployment is serving, which is a
  different thing from the model on somebody's laptop. The command line
  can answer all four about a checkout; none of those answers say
  anything about what is running here.

  They are laid out as one list of verdicts rather than four cards,
  because that is what they are: the same shape as a refusal on Explore
  and a decision on Activity. A finding is a sentence and needs the
  width, and four identical cards side by side would give each of them
  a third of it to say something short in.

  The order is the order somebody debugging works in. Does it load, does
  it still answer correctly, does the warehouse still agree, and did it
  change. The last one is the question nobody thinks to ask until a
  number moves.
*/

interface Finding {
  severity: string;
  table?: string;
  column?: string;
  message: string;
}

interface DoctorReport {
  ok: boolean;
  tables_checked: number;
  findings: Finding[];
  skipped?: string;
}

interface CaseResult {
  name: string;
  passed: boolean;
  skipped: boolean;
  reason?: string;
  duration_ms: number;
}

interface TestReport {
  ok: boolean;
  passed: number;
  failed: number;
  skipped: number;
  withheld?: number;
  results: CaseResult[];
}

interface DoctorRun {
  at: string;
  ok: boolean;
  tables_checked: number;
  findings: number;
  error?: string;
  model_version: string;
}

interface DoctorHistory {
  every_seconds: number;
  drifted: number;
  runs: DoctorRun[];
}

interface DiffReport {
  changed: boolean;
  from: string;
  to: string;
  added: string[];
  removed: string[];
  altered: Record<string, { before: string; after: string }>;
}

/*
  One request's three outcomes.

  Absent is not a failure, and keeping them apart is the whole reason
  this type exists: an engine with no test suite has not failed its
  tests, and a screen that said so would send somebody to fix a suite
  that was never written.
*/
type State<T> =
  | { status: "loading" }
  | { status: "ready"; data: T }
  | { status: "absent"; reason: string; hint?: string }
  | { status: "error"; reason: string };

function useCheck<T>(path: string, method: "GET" | "POST" = "GET") {
  const [state, setState] = useState<State<T>>({ status: "loading" });

  const load = useCallback(() => {
    setState({ status: "loading" });
    const call = method === "GET" ? api.get<T>(path) : api.post<T>(path, {});
    call
      .then((data) => setState({ status: "ready", data }))
      .catch((err: unknown) => {
        const e = err as ApiError;
        // 404 is the engine saying the capability is not configured,
        // which every one of these routes uses deliberately.
        if (e.status === 404) {
          setState({ status: "absent", reason: e.message, hint: e.detail?.hint });
          return;
        }
        setState({ status: "error", reason: e.message ?? "The check could not run." });
      });
  }, [path, method]);

  useEffect(() => load(), [load]);
  return [state, load] as const;
}

export function Checks() {
  const [namespaces] = useCheck<{ namespaces: Namespace[] }>("/v1/namespaces");
  const [doctor, rerunDoctor] = useCheck<DoctorReport>("/v1/doctor");
  const [tests, rerunTests] = useCheck<TestReport>("/v1/tests", "POST");
  const [diff] = useCheck<DiffReport>("/v1/diff");
  const [history] = useCheck<DoctorHistory>("/v1/doctor/history");

  const loaded = namespaces.status === "ready" ? namespaces.data.namespaces : [];
  const broken = loaded.filter((n) => !n.available);

  return (
    <div className="page page-pad">
      <PageHead
        title="Checks"
        note="Whether the model this engine is serving still holds together, still answers correctly, and still matches the warehouse."
      />

      <Strip
        specs={[
          { k: "Namespaces", v: namespaces.status === "ready" ? loaded.length : "—" },
          {
            k: "Loaded",
            v: namespaces.status === "ready" ? loaded.length - broken.length : "—",
            tone: broken.length === 0 && namespaces.status === "ready" ? "ok" : undefined,
          },
          {
            k: "Assertions",
            v: tests.status === "ready" ? tests.data.passed + tests.data.failed : "—",
          },
          {
            k: "Failing",
            v: tests.status === "ready" ? tests.data.failed : "—",
            tone: tests.status === "ready" && tests.data.failed > 0 ? "refused" : undefined,
          },
          {
            k: "Warehouse",
            v: doctor.status === "ready" ? (doctor.data.ok ? "agrees" : "drifted") : "—",
            tone: doctor.status === "ready" ? (doctor.data.ok ? "ok" : "refused") : undefined,
            title: "Whether every table and column the model names still exists",
          },
        ]}
      />

      <ModelSection state={namespaces} />
      <TestSection state={tests} rerun={rerunTests} />
      <WarehouseSection state={doctor} rerun={rerunDoctor} history={history} />
      <ChangeSection state={diff} />
    </div>
  );
}

/*
  The shared shape.

  Every section is a verdict with a body, so the loading, absent and
  error states are written once. Absent is rendered as an explanation
  rather than as an error, because it is one.
*/
function Section<T>({
  title,
  absentTitle,
  state,
  actions,
  children,
}: {
  title: string;
  /*
    What absent means here, in this check's own terms. "Not configured"
    is right for a missing test suite and wrong for a diff, which is
    absent because nothing has been compared yet rather than because
    somebody forgot to turn it on.
  */
  absentTitle: string;
  state: State<T>;
  actions?: React.ReactNode;
  children: (data: T) => React.ReactNode;
}) {
  return (
    <Panel title={title} actions={actions}>
      {state.status === "loading" ? <Working /> : null}
      {state.status === "absent" ? (
        <Empty title={absentTitle}>
          <p>{state.reason}</p>
          {state.hint ? <p className="faint">{state.hint}</p> : null}
        </Empty>
      ) : null}
      {state.status === "error" ? (
        <Empty title="This check could not run">
          <p>{state.reason}</p>
        </Empty>
      ) : null}
      {state.status === "ready" ? children(state.data) : null}
    </Panel>
  );
}

const namespaceColumns: Column<Namespace>[] = [
  {
    key: "name",
    header: "Namespace",
    render: (n) => <span className="mono">{n.name}</span>,
    sort: (a, b) => a.name.localeCompare(b.name),
  },
  {
    key: "state",
    header: "Loads",
    render: (n) => (n.available ? <Tag tone="ok">yes</Tag> : <Tag tone="refused">no</Tag>),
  },
  { key: "metrics", header: "Metrics", numeric: true, render: (n) => n.metric_count },
  {
    key: "why",
    header: "Why not",
    render: (n) => n.error ?? <span className="faint">—</span>,
  },
];

function ModelSection({ state }: { state: State<{ namespaces: Namespace[] }> }) {
  return (
    <Section title="Does the model load" absentTitle="No model is loaded" state={state}>
      {(data) => {
        const rows = data.namespaces;
        const broken = rows.filter((n) => !n.available).length;
        return (
          <>
            <p className="faint small" style={{ marginBottom: "var(--s3)" }}>
              {broken === 0
                ? `Every namespace parsed and resolved. ${plural(rows.length, "namespace")} serving.`
                : `${plural(broken, "namespace")} did not load. A namespace that fails is left out rather than taking the others down.`}
            </p>
            <DataTable
              id="checks-namespaces"
              rows={rows}
              columns={namespaceColumns}
              rowKey={(n) => n.name}
              empty={<Empty title="No namespaces">This engine has no model loaded.</Empty>}
            />
          </>
        );
      }}
    </Section>
  );
}

const caseColumns: Column<CaseResult>[] = [
  {
    key: "result",
    header: "Result",
    shrink: true,
    render: (c) =>
      c.skipped ? (
        <Tag>skipped</Tag>
      ) : c.passed ? (
        <Tag tone="ok">passed</Tag>
      ) : (
        <Tag tone="refused">failed</Tag>
      ),
  },
  { key: "name", header: "Assertion", render: (c) => c.name },
  {
    key: "reason",
    header: "Detail",
    render: (c) => c.reason ?? <span className="faint">—</span>,
  },
  { key: "ms", header: "ms", numeric: true, render: (c) => c.duration_ms },
];

function TestSection({ state, rerun }: { state: State<TestReport>; rerun: () => void }) {
  return (
    <Section
      title="Does it still answer correctly"
      absentTitle="No assertions are configured"
      state={state}
      actions={
        <Button variant="default" size="compact-xs" onClick={rerun}>
          Run again
        </Button>
      }
    >
      {(data) => (
        <>
          <p className="faint small" style={{ marginBottom: "var(--s3)" }}>
            {data.failed === 0
              ? "Every assertion the suite makes about this model still holds."
              : `${plural(data.failed, "assertion")} no longer hold. A number this model reports has moved.`}
            {data.withheld
              ? ` ${plural(data.withheld, "case")} needed the warehouse and this credential may not reach it, so they were not run.`
              : ""}
          </p>
          <DataTable
            id="checks-tests"
            rows={data.results}
            columns={caseColumns}
            rowKey={(c) => c.name}
            empty={<Empty title="The suite is empty">No assertions are declared.</Empty>}
          />
        </>
      )}
    </Section>
  );
}

const findingColumns: Column<Finding>[] = [
  {
    key: "severity",
    header: "Severity",
    shrink: true,
    render: (f) =>
      f.severity === "error" ? (
        <Tag tone="danger">{f.severity}</Tag>
      ) : (
        <Tag tone="refused">{f.severity}</Tag>
      ),
  },
  {
    key: "where",
    header: "Where",
    render: (f) => (
      <span className="mono">{[f.table, f.column].filter(Boolean).join(".") || "—"}</span>
    ),
  },
  { key: "what", header: "What the warehouse says", render: (f) => f.message },
];

function WarehouseSection({
  state,
  rerun,
  history,
}: {
  state: State<DoctorReport>;
  rerun: () => void;
  history: State<DoctorHistory>;
}) {
  return (
    <Section
      title="Does the warehouse still agree"
      absentTitle="No warehouse is connected"
      state={state}
      actions={
        <Button variant="default" size="compact-xs" onClick={rerun}>
          Check again
        </Button>
      }
    >
      {(data) =>
        data.skipped ? (
          <Empty title="Nothing was checked">
            <p>{data.skipped}</p>
          </Empty>
        ) : (
          <>
            <p className="faint small" style={{ marginBottom: "var(--s3)" }}>
              {data.ok
                ? `Every table and column the model names exists. ${plural(data.tables_checked, "table")} checked.`
                : `The model names things the warehouse no longer has. ${plural(data.tables_checked, "table")} checked.`}{" "}
              This reads metadata only, never rows.
            </p>
            <DataTable
              id="checks-doctor"
              rows={data.findings}
              columns={findingColumns}
              rowKey={(f, i) => `${f.table ?? ""}.${f.column ?? ""}.${i}`}
              empty={
                <Empty title="Nothing to report">
                  The warehouse matches the model everywhere it was asked.
                </Empty>
              }
            />
            <Watch history={history} />
          </>
        )
      }
    </Section>
  );
}

/*
  What the schedule has seen.

  A single check answers "is it right now"; this answers "since when",
  which is the question somebody asks once a number has already been
  wrong for a while. Rendered as one line rather than a chart: there is
  one fact here, and a sparkline of a boolean would be decoration.

  Silent when nothing is scheduled. An engine checking on demand only is
  a normal way to run this, not a misconfiguration to nag about.
*/
function Watch({ history }: { history: State<DoctorHistory> }) {
  if (history.status !== "ready" || history.data.runs.length === 0) return null;

  const { runs, drifted, every_seconds: every } = history.data;
  const last = runs[runs.length - 1];

  return (
    <p className="faint small" style={{ marginTop: "var(--s3)" }}>
      Checked every {duration(every * 1000)} on a schedule. {plural(runs.length, "run")} kept
      {drifted > 0
        ? `, ${drifted} of them finding the warehouse changed`
        : ", none finding a change"}
      {last ? `. Last checked ${ago(last.at)}` : ""}.
    </p>
  );
}

function ChangeSection({ state }: { state: State<DiffReport> }) {
  return (
    <Section
      title="Did the last reload move a number"
      absentTitle="Nothing to compare yet"
      state={state}
    >
      {(data) => {
        const altered = Object.keys(data.altered).sort();
        return (
          <>
            <Strip
              specs={[
                { k: "From", v: data.from.slice(0, 12) },
                { k: "To", v: data.to.slice(0, 12) },
                {
                  k: "Changed meaning",
                  v: altered.length,
                  tone: altered.length ? "refused" : "ok",
                  title: "Requests that compile differently than they did before",
                },
                { k: "Added", v: data.added.length },
                {
                  k: "Removed",
                  v: data.removed.length,
                  tone: data.removed.length ? "refused" : undefined,
                },
              ]}
            />
            {!data.changed ? (
              <p className="faint small">
                The model changed but no compiled result did. Descriptions, synonyms and
                comments move without moving a number.
              </p>
            ) : (
              <>
                <p className="faint small" style={{ marginBottom: "var(--s3)" }}>
                  These compile differently than they did before the reload. A dashboard reading
                  them is showing a different number today.
                </p>
                {altered.map((label) => (
                  <details key={label} className="sql" style={{ marginBottom: "var(--s2)" }}>
                    <summary>
                      <span className="mono">{label}</span>
                    </summary>
                    <pre>{data.altered[label]?.before}</pre>
                    <pre>{data.altered[label]?.after}</pre>
                  </details>
                ))}
                {data.removed.map((label) => (
                  <p key={label} className="small">
                    <Tag tone="refused">removed</Tag> <span className="mono">{label}</span>
                  </p>
                ))}
              </>
            )}
          </>
        );
      }}
    </Section>
  );
}
