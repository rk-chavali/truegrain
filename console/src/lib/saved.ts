/*
  Saved questions.

  Stored in localStorage rather than on the server, deliberately and
  temporarily. A saved question is a person's own shortcut, not shared
  state, and keeping it client-side means the feature works before the
  control plane grows a table for it. The shape below is the shape that
  table will take, so moving it is a swap of these four functions.

  What this does not do: share a question with somebody else, survive a
  different browser, or appear on a dashboard for the whole team. Those
  need the server, and the UI says so rather than implying otherwise.
*/

const KEY = "truegrain.saved.v1";

export interface SavedQuestion {
  id: string;
  name: string;
  /** The plan.Request body, replayed verbatim against /v1/query. */
  request: Record<string, unknown>;
  createdAt: string;
}

export function listQuestions(): SavedQuestion[] {
  try {
    const raw = localStorage.getItem(KEY);
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    return Array.isArray(parsed) ? (parsed as SavedQuestion[]) : [];
  } catch {
    // Corrupt storage is not worth failing a page over.
    return [];
  }
}

export function saveQuestion(input: {
  name: string;
  request: Record<string, unknown>;
}): SavedQuestion {
  const q: SavedQuestion = {
    id: `q_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 8)}`,
    name: input.name.trim() || "Untitled",
    request: input.request,
    createdAt: new Date().toISOString(),
  };
  const all = [q, ...listQuestions()].slice(0, 200);
  localStorage.setItem(KEY, JSON.stringify(all));
  return q;
}

export function deleteQuestion(id: string) {
  localStorage.setItem(KEY, JSON.stringify(listQuestions().filter((q) => q.id !== id)));
}

/** A one-line description of what a saved question asks. */
export function describe(q: SavedQuestion): string {
  const metrics = (q.request.metrics as string[] | undefined) ?? [];
  const dims = (q.request.dimensions as string[] | undefined) ?? [];
  const grain = q.request.grain as string | undefined;
  const parts = [metrics.join(", ") || "no metric"];
  if (dims.length) parts.push(`by ${dims.join(", ")}`);
  if (grain) parts.push(`per ${grain}`);
  return parts.join(" ");
}
