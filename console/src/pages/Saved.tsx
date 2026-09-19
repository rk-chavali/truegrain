import { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Button } from "@mantine/core";
import { api, cell, isNumeric, type QueryResult } from "../lib/api";
import { Empty, PageHead, Panel, Strip, Tag, ago, plural } from "../components/ui";
import { deleteQuestion, describe, listQuestions, type SavedQuestion } from "../lib/saved";
import { download, toCSV } from "../lib/csv";
import { IconDownload } from "../components/icons";

/*
  Saved questions.

  A person's own shortcuts. Running one here rather than bouncing back
  to Explore is the point: the question is already composed, so the only
  thing left is the answer.

  These live in this browser, and the screen says so rather than letting
  somebody believe they shared something.
*/
export function Saved() {
  const [items, setItems] = useState<SavedQuestion[]>(() => listQuestions());
  const [runningId, setRunningId] = useState<string>();
  const [result, setResult] = useState<{ id: string; data?: QueryResult; error?: string }>();
  const navigate = useNavigate();

  const newest = useMemo(() => items[0]?.createdAt, [items]);

  async function run(q: SavedQuestion) {
    setRunningId(q.id);
    setResult(undefined);
    try {
      const data = await api.post<QueryResult>("/v1/query", q.request);
      setResult({ id: q.id, data });
    } catch (err) {
      setResult({ id: q.id, error: err instanceof Error ? err.message : "That did not run." });
    } finally {
      setRunningId(undefined);
    }
  }

  if (items.length === 0) {
    return (
      <div className="page page-pad">
        <PageHead title="Saved" />
        <Empty
          title="Nothing saved yet"
          action={<Button onClick={() => navigate("/explore")}>Go to Explore</Button>}
        >
          <p>
            Run a question in Explore and press Save. It comes back here, ready to run again.
          </p>
        </Empty>
      </div>
    );
  }

  return (
    <div className="page page-pad">
      <PageHead title="Saved" />
      <Strip
        specs={[
          { k: "Questions", v: items.length },
          { k: "Newest", v: ago(newest) },
          { k: "Stored", v: "this browser", tone: "off" },
        ]}
      />

      <Panel tight>
        <table className="grid">
          <thead>
            <tr>
              <th>Question</th>
              <th>Asks for</th>
              <th className="shrink">Saved</th>
              <th className="shrink" />
            </tr>
          </thead>
          <tbody>
            {items.map((q) => (
              <tr key={q.id}>
                <td>
                  <b>{q.name}</b>
                </td>
                <td className="mono small muted truncate">{describe(q)}</td>
                <td className="faint small">{ago(q.createdAt)}</td>
                <td className="shrink">
                  <div className="row" style={{ gap: "var(--s2)" }}>
                    <Button
                      variant="default"
                      size="compact-xs"
                      onClick={() => void run(q)}
                      loading={runningId === q.id}
                    >
                      Run
                    </Button>
                    <Button
                      variant="subtle"
                      color="danger"
                      size="compact-xs"
                      onClick={() => {
                        deleteQuestion(q.id);
                        setItems(listQuestions());
                        if (result?.id === q.id) setResult(undefined);
                      }}
                    >
                      Delete
                    </Button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </Panel>

      {result?.error ? (
        <div className="verdict verdict-refused" style={{ marginTop: "var(--s4)" }}>
          <div className="verdict-bar">Refused</div>
          <div className="verdict-body">
            <p style={{ marginBottom: 0 }}>{result.error}</p>
          </div>
        </div>
      ) : null}

      {result?.data ? (
        <section className="panel panel-tight" style={{ marginTop: "var(--s4)" }}>
          <header className="panel-head">
            <Tag tone="ok">Answered</Tag>
            <span className="faint small">{plural(result.data.rows.length, "row")}</span>
            <Button
              className="spacer"
              variant="default"
              size="compact-xs"
              leftSection={<IconDownload size={13} />}
              onClick={() =>
                download(
                  `truegrain-${Date.now()}.csv`,
                  toCSV(result.data!.columns, result.data!.rows),
                )
              }
            >
              CSV
            </Button>
          </header>
          <div className="panel-body">
            <div className="scroll-y" style={{ maxHeight: "24rem" }}>
              <table className="grid grid-dense">
                <thead>
                  <tr>
                    {result.data.columns.map((c) => (
                      <th key={c}>{c}</th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {result.data.rows.map((row, i) => (
                    <tr key={i}>
                      {row.map((v, j) => (
                        <td key={j} className={isNumeric(v) ? "num" : undefined}>
                          {cell(v)}
                        </td>
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        </section>
      ) : null}
    </div>
  );
}
