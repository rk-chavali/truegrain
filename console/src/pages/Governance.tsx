import { useEffect, useState } from "react";
import { Button, Select } from "@mantine/core";
import { api, ApiError, type Metric } from "../lib/api";
import { Empty, Notice, PageHead, Panel, Strip, Working } from "../components/ui";
import { inScope, useNamespace } from "../lib/namespace";

/*
  Governance.

  Two questions, and they are asked by two different people. An operator
  wants to know what this engine enforces; an analyst wants to know why
  they cannot group by a column. The first is a fact about the
  deployment and sits in the spec strip with the other deployment facts.
  The second is a question with an answer, so it is a form.

  What this screen deliberately cannot do is tell you what somebody else
  can see. That would be useful for an administrator and it would also
  publish the policy the engine was configured to enforce, so the engine
  serves no such route and there is nothing here to call.
*/

interface Capabilities {
  resolver: string;
  column_level: boolean;
  note: string;
}

interface PolicyReport {
  governance: Capabilities;
  enforcement_notes?: string[];
}

interface Explanation {
  metric: string;
  identity: string;
  readable: string[];
  governance: Capabilities;
}

export function Governance() {
  const scope = useNamespace();
  const [policy, setPolicy] = useState<PolicyReport>();
  const [policyError, setPolicyError] = useState<string>();
  const [metrics, setMetrics] = useState<Metric[]>([]);

  const [metric, setMetric] = useState<string | null>(null);
  const [explaining, setExplaining] = useState(false);
  const [explanation, setExplanation] = useState<Explanation>();
  const [explainError, setExplainError] = useState<string>();

  useEffect(() => {
    api
      .get<PolicyReport>("/v1/policy")
      .then(setPolicy)
      .catch((err: unknown) =>
        setPolicyError(err instanceof Error ? err.message : "Could not read the policy."),
      );
    api
      .get<{ metrics: Metric[] }>("/v1/metrics")
      .then((d) => setMetrics(d.metrics ?? []))
      .catch(() => setMetrics([]));
  }, []);

  async function explain() {
    if (!metric) return;
    setExplaining(true);
    setExplainError(undefined);
    setExplanation(undefined);
    try {
      setExplanation(await api.post<Explanation>("/v1/policy/explain", { metric }));
    } catch (err) {
      setExplainError(
        err instanceof ApiError ? err.message : "That metric could not be explained.",
      );
    } finally {
      setExplaining(false);
    }
  }

  const choices = metrics.filter((m) => inScope(m.name, scope));

  return (
    <div className="page page-pad w-read">
      <PageHead
        title="Governance"
        note="What this engine enforces, and what it will let you read."
      />

      {policyError ? <Notice title="The policy could not be read">{policyError}</Notice> : null}

      {policy ? (
        <>
          <Strip
            specs={[
              { k: "Resolver", v: policy.governance.resolver },
              {
                k: "Column level",
                v: policy.governance.column_level ? "enforced" : "not enforced",
                tone: policy.governance.column_level ? "ok" : "denied",
                title: "Whether this resolver makes real per-column decisions",
              },
            ]}
          />

          {/*
            An engine granting everything says so here in its own words.
            The alternative is somebody reading a governance screen and
            concluding a gate exists because the product has one.
          */}
          <Notice kind={policy.governance.column_level ? "ok" : "warn"}>
            {policy.governance.note}
          </Notice>

          {policy.enforcement_notes && policy.enforcement_notes.length > 0 ? (
            <Panel title="What is not enforced">
              <ul className="notes">
                {policy.enforcement_notes.map((note) => (
                  <li key={note}>{note}</li>
                ))}
              </ul>
            </Panel>
          ) : null}
        </>
      ) : policyError ? null : (
        <Working />
      )}

      <Panel title="What you may read">
        <p className="faint small" style={{ marginBottom: "var(--s3)" }}>
          Pick a metric to see which dimensions you can group it by. This answers for you and
          for nobody else: an engine that reported another person&rsquo;s access would be
          publishing the policy it exists to enforce.
        </p>

        <div className="builder-row" style={{ alignItems: "flex-end" }}>
          <Select
            label="Metric"
            placeholder="Pick a metric"
            searchable
            value={metric}
            onChange={setMetric}
            data={choices.map((m) => ({ value: m.name, label: m.name }))}
            style={{ flex: 1 }}
          />
          <Button onClick={() => void explain()} disabled={!metric} loading={explaining}>
            Explain
          </Button>
        </div>

        {explainError ? (
          <Notice title="That could not be explained">{explainError}</Notice>
        ) : null}

        {explanation ? (
          <div style={{ marginTop: "var(--s4)" }}>
            <Strip
              specs={[
                { k: "You are", v: explanation.identity },
                { k: "Readable", v: explanation.readable.length, tone: "ok" },
              ]}
            />
            {explanation.readable.length === 0 ? (
              <Empty title="Nothing to group this by">
                <p>
                  This metric declares no dimension you may read. An administrator can grant
                  access to the fields behind it.
                </p>
              </Empty>
            ) : (
              <ul className="notes">
                {explanation.readable.map((dim) => (
                  <li key={dim} className="mono small">
                    {dim}
                  </li>
                ))}
              </ul>
            )}
          </div>
        ) : null}
      </Panel>
    </div>
  );
}
