import { useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { Button, Checkbox, CloseButton, NumberInput, Select, TextInput } from "@mantine/core";
import {
  api,
  ApiError,
  cell,
  isNumeric,
  type Compiled,
  type Job,
  type Metric,
  type QueryResult,
} from "../lib/api";
import { Empty, Segmented, Strip, Tag, Working, plural } from "../components/ui";
import { IconDownload, IconSearch } from "../components/icons";
import { toCSV, download } from "../lib/csv";
import { saveQuestion } from "../lib/saved";
import { inScope, useNamespace } from "../lib/namespace";

/*
  Explore.

  The builder is a list of metrics and the dimensions those metrics can
  be grouped by. The result region has three outcomes, not two, and the
  refusal is not smaller than the answer: it carries the engine's own
  reason, the hint, and a button that swaps in the metric that does
  answer.
*/

const GRAINS = ["", "day", "week", "month", "quarter", "year"] as const;

type View = "table" | "chart" | "line";

/** One drill-down: a dimension pinned to a value the person clicked. */
interface Drill {
  dimension: string;
  value: string;
}

/*
  How often a running job is asked whether it has finished.

  Short enough that a fast query still feels immediate, long enough
  that a five minute query is forty requests rather than six hundred.
*/
const POLL_MS = 500;

export function Explore() {
  const [metrics, setMetrics] = useState<Metric[]>();
  const [loadError, setLoadError] = useState<string>();
  const [filter, setFilter] = useState("");

  const scope = useNamespace();
  const [pickedMetrics, setPickedMetrics] = useState<string[]>([]);
  const [pickedDims, setPickedDims] = useState<string[]>([]);
  const [grain, setGrain] = useState("");
  /*
    Filters added by clicking a value in the result.

    Drill-down is the one interaction an analyst does more than any
    other: see a total broken down, pick the row that looks wrong, and
    ask the same question again about only that. Typing it back into a
    filter form is the part everybody skips, and skipping it is how
    somebody ends up believing a number they never checked.
  */
  const [filters, setFilters] = useState<Drill[]>([]);
  const [limit, setLimit] = useState(100);

  const [running, setRunning] = useState(false);
  const [previewing, setPreviewing] = useState(false);
  const [jobID, setJobID] = useState<string>();
  const [compiled, setCompiled] = useState<Compiled>();
  const [result, setResult] = useState<QueryResult>();
  const [refusal, setRefusal] = useState<ApiError>();
  const [elapsed, setElapsed] = useState<number>();
  const [view, setView] = useState<View>("table");
  const [savedNote, setSavedNote] = useState<string>();

  /*
    A metric named in the URL starts the question.

    The palette navigates here with one, so choosing a metric there
    lands on a built question rather than an empty builder beside a
    name somebody has to find again.
  */
  const [params, setParams] = useSearchParams();
  useEffect(() => {
    const wanted = params.get("metric");
    if (!wanted) return;
    setPickedMetrics([wanted]);
    // Consumed, so a reload does not re-apply it over a question the
    // person has since changed.
    setParams({}, { replace: true });
  }, [params, setParams]);

  useEffect(() => {
    api
      .get<{ metrics: Metric[] }>("/v1/metrics")
      .then((d) => setMetrics(d.metrics ?? []))
      .catch((err: unknown) =>
        setLoadError(err instanceof Error ? err.message : "Could not load the model."),
      );
  }, []);

  /*
    Derived, like the dimensions below. Narrowing the namespace has to
    take a metric out of the question as well as out of the list, or
    the builder would go on running a question it no longer shows.
  */
  const chosen = useMemo(
    () => pickedMetrics.filter((m) => inScope(m, scope)),
    [pickedMetrics, scope],
  );

  /*
    Only the dimensions every chosen metric shares.

    An intersection rather than a union: a dimension one metric has and
    another does not is a question spanning two grains, and offering it
    would be offering a refusal. The engine would still catch it; this
    means a person does not have to be caught.
  */
  const available = useMemo(() => {
    if (!metrics || chosen.length === 0) return [];
    const picked = metrics.filter((m) => chosen.includes(m.name));
    if (picked.length === 0) return [];
    const [first, ...rest] = picked;
    let shared = new Set(first?.dimensions ?? []);
    for (const m of rest) shared = new Set(m.dimensions.filter((d) => shared.has(d)));
    return [...shared].sort();
  }, [metrics, chosen]);

  /*
    Derived, not stored.

    Choosing a second metric can narrow the shared dimensions, and a
    dimension that is no longer offered must stop being part of the
    question. Pruning the stored selection in an effect meant a render
    where the query body and the checkboxes disagreed; deriving it
    means there is never such a render.
  */
  const dims = useMemo(
    () => pickedDims.filter((d) => available.includes(d)),
    [pickedDims, available],
  );

  const shown = useMemo(() => {
    if (!metrics) return [];
    const q = filter.trim().toLowerCase();
    // Scope first, then the search box. Searching inside a namespace is
    // what somebody means when they have chosen one.
    const scoped = metrics.filter((m) => inScope(m.name, scope));
    if (!q) return scoped;
    return scoped.filter(
      (m) => m.name.toLowerCase().includes(q) || m.description?.toLowerCase().includes(q),
    );
  }, [metrics, filter, scope]);

  /*
    A filter on a dimension that is no longer grouped by is still a
    filter, and still narrows the answer. It is kept rather than derived
    away: dropping it silently would widen the number on screen without
    anybody asking for that.
  */
  const request = useMemo(() => {
    const body: Record<string, unknown> = { metrics: chosen, limit };
    if (dims.length) body.dimensions = dims;
    if (grain) body.grain = grain;
    if (filters.length) {
      body.filters = filters.map((f) => ({
        dimension: f.dimension,
        op: "eq",
        values: [f.value],
      }));
    }
    return body;
  }, [chosen, dims, grain, limit, filters]);

  function clearResult() {
    setRefusal(undefined);
    setResult(undefined);
    setCompiled(undefined);
    setSavedNote(undefined);
  }

  /*
    Compiles without running.

    The question an analyst asks before a warehouse bill, and the only
    way to read the SQL of a question the warehouse would reject: the
    engine decides refusals before it decides anything else, so a
    fan-out is declined here exactly as it would be on the way to a
    query, without a round trip to the warehouse.
  */
  async function preview() {
    clearResult();
    setPreviewing(true);
    try {
      setCompiled(await api.post<Compiled>("/v1/compile", request));
    } catch (err) {
      setRefusal(err instanceof ApiError ? err : new ApiError(0, "error", String(err)));
    } finally {
      setPreviewing(false);
    }
  }

  /*
    Runs as a job rather than as one blocking request.

    A warehouse query can take minutes, and the synchronous path gave a
    person no way to stop one: they could close the tab, which abandons
    the query rather than cancelling it, and the warehouse went on
    being billed for an answer nobody was waiting for.

    The compiled SQL comes back on submission, so it is on screen while
    the query is still running.
  */
  async function run() {
    clearResult();
    setRunning(true);
    const started = performance.now();
    try {
      const job = await api.post<Job>("/v1/jobs", request);
      setJobID(job.job_id);
      if (job.compiled_sql) {
        setCompiled({
          compiled_sql: job.compiled_sql,
          columns: job.columns ?? [],
          dialect: "",
          namespace: "",
        });
      }

      const finished = await poll(job.job_id);
      if (finished.state === "cancelled") {
        // Not a refusal and not an answer. The person stopped it, and
        // saying anything louder than nothing would be the interface
        // reporting their own decision back to them as an event.
        return;
      }
      if (finished.state === "failed") {
        const code = finished.code ?? "error";
        const reason = finished.reason ?? "The query failed.";
        setRefusal(
          new ApiError(422, code, reason, {
            code,
            message: reason,
            hint: finished.hint,
            retry: finished.retry,
          }),
        );
        return;
      }
      setResult({
        columns: finished.columns ?? [],
        rows: finished.rows ?? [],
        row_count: finished.row_count,
        compiled_sql: finished.compiled_sql,
        rollups: finished.rollups,
      });
    } catch (err) {
      setRefusal(err instanceof ApiError ? err : new ApiError(0, "error", String(err)));
    } finally {
      setElapsed(Math.round(performance.now() - started));
      setRunning(false);
      setJobID(undefined);
    }
  }

  /** Polls one job to a terminal state. */
  async function poll(id: string): Promise<Job> {
    for (;;) {
      const job = await api.get<Job>(`/v1/jobs/${encodeURIComponent(id)}`);
      if (job.state !== "running") return job;
      await new Promise((resolve) => setTimeout(resolve, POLL_MS));
    }
  }

  /*
    Stops the query in the warehouse, not just in this tab.

    The job keeps its own state, so the poll above sees "cancelled" and
    ends on its own rather than being raced from here.
  */
  async function cancel() {
    if (!jobID) return;
    try {
      await api.del(`/v1/jobs/${encodeURIComponent(jobID)}`);
    } catch {
      // A job that finished a moment before the click is already gone,
      // and the poll is about to report what it returned.
    }
  }

  function applyHint(metric: string) {
    setPickedMetrics([metric]);
    setRefusal(undefined);
  }

  if (loadError) {
    return (
      <div className="page page-pad">
        <Empty title="No model is loaded">
          <p>{loadError}</p>
          <p className="faint">
            An administrator points this instance at a warehouse and a model directory.
          </p>
        </Empty>
      </div>
    );
  }

  return (
    <div className="explore">
      <div className="builder">
        <div className="builder-scroll">
          <div className="builder-section">
            <h4>
              <span>Metrics</span>
              {chosen.length > 0 ? (
                <Button
                  variant="subtle"
                  color="gray"
                  size="compact-xs"
                  onClick={() => setPickedMetrics([])}
                >
                  Clear
                </Button>
              ) : null}
            </h4>
            <TextInput
              value={filter}
              onChange={(e) => setFilter(e.currentTarget.value)}
              placeholder="Find a metric"
              aria-label="Find a metric"
              leftSection={<IconSearch size={13} />}
              rightSection={
                filter ? <CloseButton size="sm" onClick={() => setFilter("")} /> : null
              }
              mb="sm"
            />
            {!metrics ? <Working /> : null}
            {metrics && shown.length === 0 ? (
              <p className="faint small">Nothing matches.</p>
            ) : null}
            {shown.map((m) => (
              <Checkbox
                key={m.name}
                className="pick"
                checked={chosen.includes(m.name)}
                onChange={(e) => {
                  /*
                    Read during the event, never inside the updater.
                    React calls the updater later and has released the
                    synthetic event by then, so e.currentTarget is null
                    and the whole tree unmounts on a thrown read.
                  */
                  const on = e.currentTarget.checked;
                  setPickedMetrics((c) =>
                    on ? [...c, m.name] : c.filter((x) => x !== m.name),
                  );
                }}
                label={
                  <span style={{ minWidth: 0 }}>
                    <span className="pick-name">{bare(m.name)}</span>
                    {m.description ? <small>{m.description}</small> : null}
                  </span>
                }
              />
            ))}
          </div>

          <div className="builder-section">
            <h4>
              <span>Group by</span>
              {dims.length > 0 ? (
                <Button
                  variant="subtle"
                  color="gray"
                  size="compact-xs"
                  onClick={() => setPickedDims([])}
                >
                  Clear
                </Button>
              ) : null}
            </h4>
            {chosen.length === 0 ? (
              <p className="faint small">Pick a metric first.</p>
            ) : available.length === 0 ? (
              <p className="faint small">
                These metrics share no dimension, so there is nothing they can both be grouped
                by.
              </p>
            ) : (
              available.map((d) => (
                <Checkbox
                  key={d}
                  className="pick"
                  checked={dims.includes(d)}
                  onChange={(e) => {
                    const on = e.currentTarget.checked;
                    setPickedDims((c) => (on ? [...c, d] : c.filter((x) => x !== d)));
                  }}
                  label={<span className="pick-name">{bare(d)}</span>}
                />
              ))
            )}
            {chosen.length > 1 && available.length > 0 ? (
              <p className="faint small" style={{ marginTop: "var(--s2)" }}>
                Only dimensions all {chosen.length} metrics share.
              </p>
            ) : null}
          </div>

          <div className="builder-section">
            <div className="builder-row">
              <Select
                label="Time grain"
                value={grain}
                onChange={(v) => setGrain(v ?? "")}
                data={GRAINS.map((g) => ({ value: g, label: g === "" ? "None" : g }))}
                allowDeselect={false}
                style={{ flex: 1 }}
              />
              <NumberInput
                label="Row limit"
                value={limit}
                onChange={(v) => setLimit(typeof v === "number" ? v : Number(v) || 0)}
                min={1}
                max={100000}
                clampBehavior="strict"
                className="input-mono"
                w="6.5rem"
              />
            </div>
          </div>
        </div>

        <div className="builder-foot">
          {running ? (
            <Button variant="default" onClick={() => void cancel()}>
              Cancel
            </Button>
          ) : (
            <Button onClick={() => void run()} disabled={chosen.length === 0}>
              Run
            </Button>
          )}
          <Button
            variant="subtle"
            color="gray"
            onClick={() => void preview()}
            disabled={chosen.length === 0 || running}
            loading={previewing}
            title="Compile without running it, and without touching the warehouse"
          >
            Show SQL
          </Button>
          <span className="faint small">
            {chosen.length === 0
              ? "Pick a metric"
              : `${chosen.length} metric${chosen.length === 1 ? "" : "s"}${
                  dims.length ? `, ${dims.length} group${dims.length === 1 ? "" : "s"}` : ""
                }`}
          </span>
        </div>
      </div>

      <div className="result">
        {running ? <Working /> : null}
        <div className="result-pad">
          {!running && !result && !refusal && !compiled ? <Blank /> : null}
          {compiled && !result && !refusal ? <Preview compiled={compiled} /> : null}
          {refusal ? (
            <Verdict error={refusal} metrics={metrics ?? []} onApply={applyHint} />
          ) : null}
          {result ? (
            <Answer
              result={result}
              elapsed={elapsed}
              view={view}
              setView={setView}
              savedNote={savedNote}
              dimensions={dims}
              grain={grain}
              filters={filters}
              onDrill={(dimension, value) => {
                setFilters((current) =>
                  current.some((f) => f.dimension === dimension)
                    ? current.map((f) => (f.dimension === dimension ? { dimension, value } : f))
                    : [...current, { dimension, value }],
                );
              }}
              onRemoveFilter={(dimension) =>
                setFilters((current) => current.filter((f) => f.dimension !== dimension))
              }
              onSave={() => {
                const name = window.prompt("Name this question", chosen.map(bare).join(", "));
                if (!name) return;
                saveQuestion({ name, request });
                setSavedNote(`Saved as "${name}".`);
              }}
            />
          ) : null}
        </div>
      </div>
    </div>
  );
}

/*
  What would run, having run nothing.

  Deliberately not dressed as an answer. It uses the spec strip and the
  open SQL block rather than the verdict bar, because no verdict has
  been reached: the warehouse has not been asked. The SQL is shown
  expanded rather than behind a disclosure, since seeing it is the
  entire reason this was clicked.
*/
function Preview({ compiled }: { compiled: Compiled }) {
  return (
    <>
      <Strip
        specs={[
          { k: "Compiled", v: "not run", tone: "ok" },
          { k: "Dialect", v: compiled.dialect || "—" },
          { k: "Namespace", v: compiled.namespace || "—" },
          { k: "Columns", v: compiled.columns.length },
        ]}
      />
      <div className="sql">
        <p className="faint small" style={{ marginBottom: "var(--s2)" }}>
          This is the statement the warehouse would receive. Nothing has been executed and
          nothing has been billed.
        </p>
        <pre>{compiled.compiled_sql}</pre>
      </div>
    </>
  );
}

function bare(name: string): string {
  const parts = name.split(".");
  return parts.length > 2 ? parts.slice(1).join(".") : (parts[parts.length - 1] ?? name);
}

function Blank() {
  return (
    <Empty title="Ask something">
      <p>
        Pick a metric and run it. Add a dimension to break the number down; only dimensions your
        chosen metrics share are offered.
      </p>
      <p className="faint">
        A question that cannot be answered accurately comes back as a refusal naming one that
        can, rather than a plausible wrong number.
      </p>
    </Empty>
  );
}

/**
 * The refusal, given the room a result gets.
 *
 * Denied is access and nothing the person can rewrite their way out of.
 * Refused is correctness, and the hint is actionable. The two are never
 * the same colour.
 */
function Verdict({
  error,
  metrics,
  onApply,
}: {
  error: ApiError;
  metrics: Metric[];
  onApply: (metric: string) => void;
}) {
  const denied = error.code === "access_denied" || error.status === 403;
  const refused =
    !denied && (error.detail !== undefined || (error.status >= 400 && error.status < 500));
  const kind = denied ? "denied" : refused ? "refused" : "error";
  const hint = error.detail?.hint;

  /*
    A hint names the metrics that answer, unqualified: "line_revenue,
    units_sold" rather than "retail.line_revenue". Matching on words
    rather than substrings stops "revenue" from finding "order_revenue".
  */
  const words = new Set((hint ?? "").split(/[^A-Za-z0-9_]+/));
  const suggested = hint
    ? metrics.find((m) => words.has(m.name.split(".").pop() ?? m.name))?.name
    : undefined;

  return (
    <div className={`verdict verdict-${kind}`}>
      <div className="verdict-bar">
        {denied ? "Denied" : refused ? "Refused" : "Failed"}
        {error.code ? <span className="code">{error.code}</span> : null}
      </div>
      <div className="verdict-body">
        <h3>
          {denied
            ? "You cannot read one of these fields"
            : refused
              ? "This question cannot be answered accurately"
              : "The query did not run"}
        </h3>
        <p>{error.message}</p>
        {hint ? <p>{hint}</p> : null}
        {suggested ? (
          <Button variant="default" onClick={() => onApply(suggested)}>
            Use {bare(suggested)} instead
          </Button>
        ) : null}
        {denied ? (
          <p className="faint small">
            This is about access, not correctness. The number exists; your account is not
            granted the field.
          </p>
        ) : null}
      </div>
    </div>
  );
}

function Answer({
  result,
  elapsed,
  view,
  setView,
  onSave,
  savedNote,
  dimensions,
  grain,
  filters,
  onDrill,
  onRemoveFilter,
}: {
  result: QueryResult;
  elapsed?: number;
  view: View;
  setView: (v: View) => void;
  onSave: () => void;
  savedNote?: string;
  dimensions: string[];
  grain: string;
  filters: Drill[];
  onDrill: (dimension: string, value: string) => void;
  onRemoveFilter: (dimension: string) => void;
}) {
  const { columns, rows } = result;
  const single = rows.length === 1 && columns.length === 1;

  const chartable =
    columns.length === 2 &&
    rows.length > 1 &&
    rows.length <= 60 &&
    rows.every((r) => isNumeric(r[1]));

  /*
    A time grain makes the first column a sequence, and a sequence is a
    line. Categories are not ordered, so they stay bars: a line drawn
    between two regions asserts a progression that does not exist.

    Chosen from the data rather than offered as a preference. The
    alternatives are still there for somebody who disagrees, but the
    default should already be right.
  */
  const temporal = grain !== "";
  const shape: View = temporal ? "line" : "chart";

  return (
    <>
      {filters.length > 0 ? (
        <div className="drills" role="group" aria-label="Narrowed to">
          <span className="faint small">Narrowed to</span>
          {filters.map((f) => (
            <span className="drill-chip" key={f.dimension}>
              <span className="mono">{bare(f.dimension)}</span>
              <b>{f.value}</b>
              <CloseButton
                size="xs"
                aria-label={`Stop narrowing by ${bare(f.dimension)}`}
                onClick={() => onRemoveFilter(f.dimension)}
              />
            </span>
          ))}
        </div>
      ) : null}

      <Strip
        specs={[
          { k: "Verdict", v: "Answered", tone: "ok" },
          { k: "Rows", v: rows.length },
          { k: "Columns", v: columns.length },
          { k: "Elapsed", v: elapsed !== undefined ? `${elapsed} ms` : "—" },
          ...(result.rollups?.length
            ? [{ k: "Read from", v: result.rollups.join(", "), tone: "ok" as const }]
            : []),
        ]}
        end={
          <div className="row" style={{ gap: "var(--s2)" }}>
            {chartable ? (
              <Segmented
                value={view}
                onChange={setView}
                options={[
                  { id: "table", label: "Table" },
                  temporal ? { id: "line", label: "Line" } : { id: "chart", label: "Bars" },
                ]}
              />
            ) : null}
            <Button variant="default" size="compact-xs" onClick={onSave}>
              Save
            </Button>
            <Button
              variant="default"
              size="compact-xs"
              leftSection={<IconDownload size={13} />}
              onClick={() => download(`truegrain-${Date.now()}.csv`, toCSV(columns, rows))}
            >
              CSV
            </Button>
          </div>
        }
      />

      {savedNote ? (
        <p className="small" style={{ color: "var(--ok)" }}>
          {savedNote}
        </p>
      ) : null}

      {single ? (
        <div className="panel">
          <div className="panel-body">
            <div className="figure">{cell(rows[0]?.[0])}</div>
            <div className="figure-label">{columns[0]}</div>
          </div>
        </div>
      ) : (
        <section className="panel panel-tight">
          <div className="panel-body">
            {chartable && view !== "table" ? (
              <div style={{ padding: "var(--s4)" }}>
                {shape === "line" ? (
                  <Line rows={rows} label={columns[0] ?? ""} />
                ) : (
                  <Bars rows={rows} />
                )}
              </div>
            ) : (
              <div className="scroll-y" style={{ maxHeight: "32rem" }}>
                <table className="grid grid-dense">
                  <thead>
                    <tr>
                      {columns.map((c, i) => (
                        <th
                          key={c}
                          className={rows.some((r) => isNumeric(r[i])) ? "num" : undefined}
                        >
                          {c}
                        </th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map((row, i) => (
                      <tr key={i}>
                        {row.map((value, j) => {
                          const dimension = dimensions[j];
                          // Only a grouped dimension can be drilled into.
                          // A metric cell is a number, and filtering a
                          // question by its own answer means nothing.
                          const drillable = dimension !== undefined && !isNumeric(value);
                          return (
                            <td key={j} className={isNumeric(value) ? "num" : undefined}>
                              {drillable ? (
                                <button
                                  type="button"
                                  className="drill"
                                  title={`Ask this again for ${cell(value)} only`}
                                  onClick={() => onDrill(dimension, String(value))}
                                >
                                  {cell(value)}
                                </button>
                              ) : (
                                cell(value)
                              )}
                            </td>
                          );
                        })}
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        </section>
      )}

      {result.compiled_sql ? (
        <div className="sql" style={{ marginTop: "var(--s4)" }}>
          <details>
            <summary>
              The SQL this ran. Every answer carries it, so a number can be checked.
            </summary>
            <pre>{result.compiled_sql}</pre>
          </details>
        </div>
      ) : null}

      <p className="faint small" style={{ marginTop: "var(--s3)" }}>
        {plural(rows.length, "row")} returned.
      </p>
    </>
  );
}

/** One bar per row, scaled to the largest value. No chart dependency. */
/*
  A time series.

  SVG by hand rather than a charting library: this draws one polyline
  and a baseline, and a dependency that renders it would arrive with a
  theme, a tooltip and an opinion about fonts, all of which would then
  need overriding back to the tokens already here.

  The y axis starts at zero. A line chart that starts at its own minimum
  makes a two percent move look like a cliff, which for a tool whose
  entire argument is "do not show a misleading number" would be a poor
  place to make an exception.
*/
export function Line({ rows, label }: { rows: unknown[][]; label: string }) {
  const values = rows.map((r) => Number(r[1]) || 0);
  const max = Math.max(...values, 0) || 1;
  const width = 720;
  const height = 220;
  const pad = 8;

  const x = (i: number) =>
    rows.length === 1 ? width / 2 : pad + (i * (width - pad * 2)) / (rows.length - 1);
  const y = (v: number) => height - pad - (v / max) * (height - pad * 2);

  const points = values.map((v, i) => `${x(i)},${y(v)}`).join(" ");
  const first = rows[0]?.[0];
  const last = rows[rows.length - 1]?.[0];

  return (
    <figure className="line-chart">
      <figcaption className="faint small">
        {label} from {cell(first)} to {cell(last)}
      </figcaption>
      <svg
        viewBox={`0 0 ${width} ${height}`}
        role="img"
        aria-label={`${plural(rows.length, "point")} from ${cell(first)} to ${cell(last)}, highest ${cell(max)}`}
        preserveAspectRatio="none"
      >
        <line
          className="line-base"
          x1={pad}
          y1={height - pad}
          x2={width - pad}
          y2={height - pad}
        />
        <polyline className="line-path" points={points} />
        {values.map((v, i) => (
          <circle key={i} className="line-dot" cx={x(i)} cy={y(v)} r={2.5}>
            <title>{`${cell(rows[i]?.[0])}: ${cell(rows[i]?.[1])}`}</title>
          </circle>
        ))}
      </svg>
    </figure>
  );
}

export function Bars({ rows }: { rows: unknown[][] }) {
  const values = rows.map((r) => Number(r[1]) || 0);
  const max = Math.max(...values.map(Math.abs), 0) || 1;
  return (
    <div className="bars">
      {rows.map((row, i) => (
        <div className="bar-row" key={i}>
          <span className="bar-label" title={cell(row[0])}>
            {cell(row[0])}
          </span>
          <span className="bar-track">
            <span
              className="bar-fill"
              style={{ width: `${(Math.abs(values[i] ?? 0) / max) * 100}%` }}
            />
          </span>
          <span className="bar-value">{cell(row[1])}</span>
        </div>
      ))}
    </div>
  );
}

export { Tag };
