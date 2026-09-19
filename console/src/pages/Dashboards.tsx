import { useCallback, useEffect, useMemo, useState } from "react";
import { api, ApiError, cell, isNumeric, type QueryResult } from "../lib/api";
import { Empty, PageHead, Segmented, Strip, Tag, Working, ago } from "../components/ui";
import { listQuestions, describe, type SavedQuestion } from "../lib/saved";
import {
  addTile,
  createDashboard,
  deleteDashboard,
  listDashboards,
  moveTile,
  removeTile,
  setTile,
  type Dashboard,
  type Tile,
} from "../lib/dashboards";
import { Bars } from "./Explore";

/*
  Dashboards.

  A dashboard here is a set of saved questions run together, each tile
  half or full width. Deliberately not a drag-and-drop canvas: a layout
  engine would be the largest thing in this codebase and would buy no
  correctness, and a grid with two widths covers what a board of
  governed metrics needs.

  Every tile runs its own query through the same governed path, so a
  refusal renders as a refusal on the tile rather than an empty box. A
  dashboard that silently drops the panel it could not answer is how a
  wrong number gets believed by omission.
*/

export function Dashboards() {
  const [boards, setBoards] = useState<Dashboard[]>(() => listDashboards());
  const [activeId, setActiveId] = useState<string | undefined>(() => listDashboards()[0]?.id);
  const questions = useMemo(() => listQuestions(), []);

  const active = boards.find((b) => b.id === activeId);
  const reload = useCallback(() => setBoards(listDashboards()), []);

  function create() {
    const name = window.prompt("Name this dashboard");
    if (!name) return;
    const d = createDashboard(name);
    reload();
    setActiveId(d.id);
  }

  if (boards.length === 0) {
    return (
      <div className="page page-pad">
        <PageHead title="Dashboards" />
        <Empty
          title="No dashboards yet"
          action={
            <button className="btn btn-primary" onClick={create}>
              Create a dashboard
            </button>
          }
        >
          <p>
            A dashboard runs several saved questions together. Save a question in Explore first,
            then add it here.
          </p>
          <p className="faint">
            {questions.length === 0
              ? "You have no saved questions yet."
              : `${questions.length} saved question${questions.length === 1 ? "" : "s"} ready to add.`}
          </p>
        </Empty>
      </div>
    );
  }

  return (
    <div className="page page-pad">
      <PageHead
        title="Dashboards"
        actions={
          <>
            <button className="btn btn-sm" onClick={create}>
              New
            </button>
            {active ? (
              <button
                className="btn btn-danger btn-sm"
                onClick={() => {
                  if (!window.confirm(`Delete "${active.name}"?`)) return;
                  deleteDashboard(active.id);
                  const rest = listDashboards();
                  setBoards(rest);
                  setActiveId(rest[0]?.id);
                }}
              >
                Delete
              </button>
            ) : null}
          </>
        }
      />

      {boards.length > 1 ? (
        <div className="row" style={{ marginBottom: "var(--s3)" }}>
          <div className="seg" role="group">
            {boards.map((b) => (
              <button
                key={b.id}
                type="button"
                aria-pressed={b.id === activeId}
                onClick={() => setActiveId(b.id)}
              >
                {b.name}
                <span className="num faint">{b.tiles.length}</span>
              </button>
            ))}
          </div>
        </div>
      ) : null}

      {active ? <Board board={active} questions={questions} onChange={reload} /> : null}
    </div>
  );
}

function Board({
  board,
  questions,
  onChange,
}: {
  board: Dashboard;
  questions: SavedQuestion[];
  onChange: () => void;
}) {
  const [nonce, setNonce] = useState(0);
  const byId = useMemo(() => new Map(questions.map((q) => [q.id, q])), [questions]);
  const unused = questions.filter((q) => !board.tiles.some((t) => t.questionId === q.id));

  return (
    <>
      <Strip
        specs={[
          { k: "Dashboard", v: board.name },
          { k: "Tiles", v: board.tiles.length },
          { k: "Created", v: ago(board.createdAt) },
          { k: "Stored", v: "this browser", tone: "off" },
        ]}
        end={
          <button className="btn btn-sm" onClick={() => setNonce((n) => n + 1)}>
            Refresh all
          </button>
        }
      />

      {unused.length > 0 ? (
        <div className="row row-wrap" style={{ marginBottom: "var(--s4)", gap: "var(--s2)" }}>
          <span className="faint small">Add a saved question:</span>
          {unused.map((q) => (
            <button
              key={q.id}
              className="btn btn-sm"
              onClick={() => {
                addTile(board.id, q.id);
                onChange();
              }}
            >
              {q.name}
            </button>
          ))}
        </div>
      ) : null}

      {board.tiles.length === 0 ? (
        <Empty title="This dashboard is empty">
          <p>
            {questions.length === 0
              ? "Save a question in Explore and it can be added here."
              : "Add one of the saved questions above."}
          </p>
        </Empty>
      ) : (
        <div
          style={{
            display: "grid",
            gridTemplateColumns: "repeat(2, minmax(0, 1fr))",
            gap: "var(--s4)",
            alignItems: "start",
          }}
        >
          {board.tiles.map((tile) => {
            const q = byId.get(tile.questionId);
            if (!q) {
              return (
                <div
                  key={tile.questionId}
                  className="panel"
                  style={{ gridColumn: tile.width === "full" ? "span 2" : undefined }}
                >
                  <div className="panel-body">
                    <p className="faint" style={{ marginBottom: "var(--s3)" }}>
                      This tile points at a saved question that no longer exists.
                    </p>
                    <button
                      className="btn btn-sm"
                      onClick={() => {
                        removeTile(board.id, tile.questionId);
                        onChange();
                      }}
                    >
                      Remove tile
                    </button>
                  </div>
                </div>
              );
            }
            return (
              <TileView
                key={tile.questionId + nonce}
                board={board}
                tile={tile}
                question={q}
                onChange={onChange}
              />
            );
          })}
        </div>
      )}
    </>
  );
}

function TileView({
  board,
  tile,
  question,
  onChange,
}: {
  board: Dashboard;
  tile: Tile;
  question: SavedQuestion;
  onChange: () => void;
}) {
  const [state, setState] = useState<{
    loading: boolean;
    data?: QueryResult;
    error?: ApiError;
  }>({ loading: true });

  useEffect(() => {
    let live = true;
    setState({ loading: true });
    api
      .post<QueryResult>("/v1/query", question.request)
      .then((data) => live && setState({ loading: false, data }))
      .catch(
        (err: unknown) =>
          live &&
          setState({
            loading: false,
            error: err instanceof ApiError ? err : new ApiError(0, "error", String(err)),
          }),
      );
    return () => {
      live = false;
    };
  }, [question]);

  const rows = state.data?.rows ?? [];
  const columns = state.data?.columns ?? [];
  const single = rows.length === 1 && columns.length === 1;
  const chartable =
    columns.length === 2 &&
    rows.length > 1 &&
    rows.length <= 40 &&
    rows.every((r) => isNumeric(r[1]));

  return (
    <section
      className="panel panel-tight"
      style={{ gridColumn: tile.width === "full" ? "span 2" : undefined, minWidth: 0 }}
    >
      <header className="panel-head">
        <span className="truncate" title={describe(question)}>
          {question.name}
        </span>
        <div className="spacer row" style={{ gap: "var(--s1)" }}>
          {chartable ? (
            <Segmented
              value={tile.view === "chart" ? "chart" : "table"}
              onChange={(v) => {
                setTile(board.id, tile.questionId, { view: v });
                onChange();
              }}
              options={[
                { id: "table" as const, label: "Table" },
                { id: "chart" as const, label: "Chart" },
              ]}
            />
          ) : null}
          <button
            className="btn btn-quiet btn-sm"
            title={tile.width === "full" ? "Make half width" : "Make full width"}
            onClick={() => {
              setTile(board.id, tile.questionId, {
                width: tile.width === "full" ? "half" : "full",
              });
              onChange();
            }}
          >
            {tile.width === "full" ? "Half" : "Full"}
          </button>
          <button
            className="btn btn-quiet btn-sm"
            title="Move earlier"
            onClick={() => {
              moveTile(board.id, tile.questionId, -1);
              onChange();
            }}
          >
            Up
          </button>
          <button
            className="btn btn-danger btn-sm"
            onClick={() => {
              removeTile(board.id, tile.questionId);
              onChange();
            }}
          >
            Remove
          </button>
        </div>
      </header>

      {state.loading ? <Working /> : null}

      <div className="panel-body">
        {state.error ? (
          /* A refused tile shows the refusal. Dropping the panel would
             let a dashboard tell a story by omission. */
          <div className="row row-top" style={{ gap: "var(--s2)" }}>
            <Tag tone={state.error.status === 403 ? "denied" : "refused"}>
              {state.error.status === 403 ? "Denied" : "Refused"}
            </Tag>
            <p className="muted small" style={{ marginBottom: 0 }}>
              {state.error.message}
            </p>
          </div>
        ) : state.loading ? (
          <div className="skeleton" style={{ width: "55%" }} />
        ) : single ? (
          <>
            <div className="figure">{cell(rows[0]?.[0])}</div>
            <div className="figure-label">{columns[0]}</div>
          </>
        ) : chartable && tile.view === "chart" ? (
          <Bars rows={rows} />
        ) : (
          <div className="scroll-y" style={{ maxHeight: "18rem" }}>
            <table className="grid grid-dense">
              <thead>
                <tr>
                  {columns.map((c) => (
                    <th key={c}>{c}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {rows.map((row, i) => (
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
        )}
      </div>
    </section>
  );
}
