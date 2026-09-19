/*
  Dashboards.

  A dashboard is an ordered list of saved questions and a width for
  each. Stored beside them in localStorage, for the same reason and with
  the same honest limit: this browser only, not shared, and the screen
  says so.

  Deliberately not a drag-and-drop canvas. A grid with two widths covers
  what a dashboard of governed metrics actually needs, and a layout
  engine would be the largest thing in this codebase for the least
  correctness.
*/

const KEY = "truegrain.dashboards.v1";

export type TileWidth = "half" | "full";

export interface Tile {
  questionId: string;
  width: TileWidth;
  /** table or chart, decided per tile rather than globally. */
  view: "table" | "chart" | "figure";
}

export interface Dashboard {
  id: string;
  name: string;
  tiles: Tile[];
  createdAt: string;
}

export function listDashboards(): Dashboard[] {
  try {
    const raw = localStorage.getItem(KEY);
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    return Array.isArray(parsed) ? (parsed as Dashboard[]) : [];
  } catch {
    return [];
  }
}

function write(all: Dashboard[]) {
  localStorage.setItem(KEY, JSON.stringify(all));
}

export function createDashboard(name: string): Dashboard {
  const d: Dashboard = {
    id: `d_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 8)}`,
    name: name.trim() || "Untitled",
    tiles: [],
    createdAt: new Date().toISOString(),
  };
  write([d, ...listDashboards()]);
  return d;
}

export function updateDashboard(id: string, change: (d: Dashboard) => Dashboard) {
  write(listDashboards().map((d) => (d.id === id ? change(d) : d)));
}

export function deleteDashboard(id: string) {
  write(listDashboards().filter((d) => d.id !== id));
}

export function addTile(id: string, questionId: string) {
  updateDashboard(id, (d) =>
    d.tiles.some((t) => t.questionId === questionId)
      ? d
      : { ...d, tiles: [...d.tiles, { questionId, width: "half", view: "table" }] },
  );
}

export function removeTile(id: string, questionId: string) {
  updateDashboard(id, (d) => ({
    ...d,
    tiles: d.tiles.filter((t) => t.questionId !== questionId),
  }));
}

export function setTile(id: string, questionId: string, patch: Partial<Tile>) {
  updateDashboard(id, (d) => ({
    ...d,
    tiles: d.tiles.map((t) => (t.questionId === questionId ? { ...t, ...patch } : t)),
  }));
}

/** Moves a tile one place in the order. */
export function moveTile(id: string, questionId: string, by: -1 | 1) {
  updateDashboard(id, (d) => {
    const i = d.tiles.findIndex((t) => t.questionId === questionId);
    const j = i + by;
    if (i < 0 || j < 0 || j >= d.tiles.length) return d;
    const tiles = [...d.tiles];
    const a = tiles[i];
    const b = tiles[j];
    if (!a || !b) return d;
    tiles[i] = b;
    tiles[j] = a;
    return { ...d, tiles };
  });
}
