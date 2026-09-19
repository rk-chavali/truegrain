import { useEffect, useMemo, useState } from "react";
import { Button } from "@mantine/core";
import { api, type ApiError } from "../lib/api";
import { Empty, PageHead, Segmented, SkeletonRows, Strip, Tag, ago } from "../components/ui";
import { download, toCSV } from "../lib/csv";
import { IconDownload } from "../components/icons";

/*
  Activity.

  The audit log read as a product screen rather than a log viewer. The
  default lens is refusals, because that is the question this engine
  exists to answer and the one nothing else records: which questions
  were declined, and what was suggested instead.

  Refused and denied are counted and coloured separately. A governance
  report that added them together would read every fan-out as an access
  incident, which is the confusion the engine's two decision values
  exist to prevent.
*/

interface Event {
  time: string;
  identity: string;
  environment?: string;
  namespace?: string;
  metrics?: string[];
  dimensions?: string[];
  decision: string;
  refusal_code?: string;
  retry?: string;
  reason?: string;
  duration_ms?: number;
  rollups?: string[];
}

type Lens = "refused" | "denied" | "allowed" | "all";

export function Activity() {
  const [events, setEvents] = useState<Event[]>();
  const [error, setError] = useState<ApiError>();
  const [lens, setLens] = useState<Lens>("refused");
  const [open, setOpen] = useState<number>();

  useEffect(() => {
    api
      .get<{ events: Event[] }>("/v1/audit?limit=500")
      .then((d) => setEvents(d.events ?? []))
      .catch((err: unknown) => setError(err as ApiError));
  }, []);

  const counts = useMemo(() => {
    const c = { refused: 0, denied: 0, allowed: 0, error: 0 };
    for (const e of events ?? []) {
      if (e.decision === "refused") c.refused++;
      else if (e.decision === "denied") c.denied++;
      else if (e.decision === "allowed") c.allowed++;
      else c.error++;
    }
    return c;
  }, [events]);

  const shown = useMemo(() => {
    if (!events) return [];
    return lens === "all" ? events : events.filter((e) => e.decision === lens);
  }, [events, lens]);

  /* The share of questions the engine declined, which is the number
     this product is actually about. */
  const asked = counts.refused + counts.denied + counts.allowed;
  const refusalRate = asked > 0 ? Math.round((counts.refused / asked) * 100) : 0;

  const topCode = useMemo(() => {
    const tally = new Map<string, number>();
    for (const e of events ?? []) {
      if (e.decision !== "refused" || !e.refusal_code) continue;
      tally.set(e.refusal_code, (tally.get(e.refusal_code) ?? 0) + 1);
    }
    return [...tally.entries()].sort((a, b) => b[1] - a[1])[0];
  }, [events]);

  /*
    Absent and withheld are different answers and this screen says so.

    The engine is careful never to collapse refused into denied, and this
    is the same distinction one level up: audit_not_served means no
    engine here records decisions at all, while a 403 means it does and
    this account is not one of its readers. One is a deployment fact and
    the other is about you, and a single "not readable" message for both
    would send an admin to fix a configuration that is already correct.
  */
  if (error) {
    const absent = error.code === "audit_not_served";
    return (
      <div className="page page-pad">
        <Empty
          title={absent ? "No decisions are recorded here" : "You cannot read the audit log"}
        >
          {absent ? (
            <>
              <p>This engine is not recording the decisions it makes.</p>
              <p className="faint">
                An operator turns it on by pointing this instance at a model and a warehouse.
                Until then there is nothing to show rather than something being withheld.
              </p>
            </>
          ) : (
            <>
              <p>Reading recorded decisions is for owners and administrators.</p>
              <p className="faint">
                It records what everybody else asked, which is why it is granted separately from
                running queries. An administrator can change your role.
              </p>
            </>
          )}
        </Empty>
      </div>
    );
  }

  return (
    <div className="page page-pad">
      <PageHead
        title="Activity"
        actions={
          events && events.length > 0 ? (
            <Button
              variant="default"
              size="compact-xs"
              leftSection={<IconDownload size={13} />}
              onClick={() =>
                download(
                  `truegrain-activity-${Date.now()}.csv`,
                  toCSV(
                    ["time", "identity", "decision", "code", "metrics", "dimensions", "ms"],
                    shown.map((e) => [
                      e.time,
                      e.identity,
                      e.decision,
                      e.refusal_code ?? "",
                      (e.metrics ?? []).join(" "),
                      (e.dimensions ?? []).join(" "),
                      e.duration_ms ?? "",
                    ]),
                  ),
                )
              }
            >
              Export
            </Button>
          ) : null
        }
      />

      <Strip
        specs={[
          { k: "Decisions", v: events?.length ?? "—" },
          { k: "Answered", v: counts.allowed, tone: "ok" },
          { k: "Refused", v: counts.refused, tone: counts.refused ? "refused" : undefined },
          { k: "Denied", v: counts.denied, tone: counts.denied ? "denied" : undefined },
          {
            k: "Refusal rate",
            v: `${refusalRate}%`,
            tone: refusalRate > 0 ? "refused" : "ok",
            title: "The share of questions the engine declined rather than answering wrongly",
          },
          ...(topCode ? [{ k: "Most common", v: topCode[0], tone: "refused" as const }] : []),
        ]}
      />

      <div className="row" style={{ marginBottom: "var(--s3)" }}>
        <Segmented
          value={lens}
          onChange={setLens}
          options={[
            { id: "refused", label: "Refused", count: counts.refused },
            { id: "denied", label: "Denied", count: counts.denied },
            { id: "allowed", label: "Answered", count: counts.allowed },
            { id: "all", label: "All", count: events?.length ?? 0 },
          ]}
        />
      </div>

      {!events ? (
        <section className="panel panel-tight">
          <div className="panel-body">
            <SkeletonRows rows={8} cols={4} />
          </div>
        </section>
      ) : shown.length === 0 ? (
        <Empty
          title={
            lens === "refused"
              ? "Nothing has been refused"
              : lens === "denied"
                ? "Nothing has been denied"
                : "Nothing here yet"
          }
        >
          <p>
            {lens === "refused"
              ? "Every question asked so far could be answered accurately."
              : lens === "denied"
                ? "No caller has asked for a field they are not granted."
                : "Run a query and it will appear here."}
          </p>
        </Empty>
      ) : (
        <section className="panel panel-tight">
          <div className="panel-body">
            <table className="grid">
              <thead>
                <tr>
                  <th className="shrink">When</th>
                  <th className="shrink">Decision</th>
                  <th>Who</th>
                  <th>Asked for</th>
                  <th className="shrink num">ms</th>
                </tr>
              </thead>
              <tbody>
                {shown.map((e, i) => (
                  <Row
                    key={i}
                    event={e}
                    open={open === i}
                    onToggle={() => setOpen(open === i ? undefined : i)}
                  />
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}
    </div>
  );
}

function Row({ event, open, onToggle }: { event: Event; open: boolean; onToggle: () => void }) {
  const tone =
    event.decision === "refused"
      ? "refused"
      : event.decision === "denied"
        ? "denied"
        : event.decision === "allowed"
          ? "ok"
          : "danger";

  return (
    <>
      <tr onClick={onToggle} style={{ cursor: event.reason ? "pointer" : "default" }}>
        <td className="mono small faint" title={new Date(event.time).toLocaleString()}>
          {ago(event.time)}
        </td>
        <td>
          <Tag tone={tone}>{event.decision}</Tag>
        </td>
        <td className="truncate">{event.identity}</td>
        <td className="muted truncate">
          {(event.metrics ?? []).join(", ") || "—"}
          {event.dimensions?.length ? (
            <span className="faint"> by {event.dimensions.join(", ")}</span>
          ) : null}
        </td>
        <td className="num faint">{event.duration_ms ?? ""}</td>
      </tr>
      {open && event.reason ? (
        <tr>
          <td colSpan={5} style={{ background: "var(--surface-2)" }}>
            <div className="row" style={{ gap: "var(--s2)", marginBottom: "var(--s2)" }}>
              {event.refusal_code ? (
                <span className="mono small">{event.refusal_code}</span>
              ) : null}
              {event.retry ? <Tag>retry: {event.retry}</Tag> : null}
              {event.environment ? <Tag>{event.environment}</Tag> : null}
              {event.rollups?.length ? (
                <Tag tone="ok">from {event.rollups.join(", ")}</Tag>
              ) : null}
            </div>
            <p className="muted" style={{ marginBottom: 0 }}>
              {event.reason}
            </p>
          </td>
        </tr>
      ) : null}
    </>
  );
}
