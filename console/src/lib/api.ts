/*
  The one place this app talks to the server.

  Every request carries the CSRF header and the session cookie, and every
  failure comes back as one shape. Scattering fetch calls through the
  components is how half of them end up without the header and the other
  half render a raw exception string at a person.
*/

export const CSRF_HEADER = "X-Truegrain-Console";

/** A failure the server described, or a network one we had to name. */
export class ApiError extends Error {
  readonly code: string;
  readonly status: number;
  /** The engine's own structured refusal, when there is one. */
  readonly detail?: EngineRefusal;

  constructor(status: number, code: string, message: string, detail?: EngineRefusal) {
    super(message);
    this.code = code;
    this.status = status;
    this.detail = detail;
  }

  /** True when the caller is not signed in, so the app can show the gate. */
  get unauthenticated(): boolean {
    return this.status === 401;
  }
}

/** What a caller should do about a refusal. */
export type Retry = "modify" | "later" | "never";

/**
 * The engine's refusal, which is a different thing from an error.
 *
 * `refused` means the question cannot be answered accurately and `hint`
 * names one that can. `denied` means access. They are kept apart here for
 * the same reason they are kept apart in the engine: collapsing them
 * turns every fan-out into what looks like a permissions incident.
 */
export interface EngineRefusal {
  code: string;
  message: string;
  hint?: string;
  retry?: Retry;
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: {
      [CSRF_HEADER]: "1",
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    // The session cookie is same-origin; this is explicit so a future
    // change of host does not silently drop it.
    credentials: "same-origin",
    body: body === undefined ? undefined : JSON.stringify(body),
  });

  if (res.status === 204) return undefined as T;

  const text = await res.text();
  let parsed: unknown;
  try {
    parsed = text ? JSON.parse(text) : {};
  } catch {
    // A proxy error page, or the server died mid-response. Say which,
    // rather than showing a JSON parse error to somebody who asked for a
    // list of metrics.
    throw new ApiError(
      res.status,
      "unreadable",
      `The server returned something that is not JSON (${res.status}).`,
    );
  }

  if (res.ok) return parsed as T;

  /*
    Two error shapes reach here, because two servers answer on this origin.

    The control plane wraps its own: {error: {code, message}}. The engine
    returns a refusal flat: {code, reason, hint, retry}, which is the
    shape its SDKs and its OpenAPI contract have always used and is not
    worth changing to suit one client.

    The flat one is checked first and kept whole, because `hint` is the
    most valuable field in the product and flattening it into a message
    string would throw away the part that tells somebody what to ask
    instead.
  */
  const flat = parsed as {
    code?: string;
    reason?: string;
    hint?: string;
    retry?: string;
    error?: { code?: string; message?: string; hint?: string; retry?: string };
  };

  if (flat.code && flat.reason !== undefined) {
    throw new ApiError(res.status, flat.code, flat.reason, {
      code: flat.code,
      message: flat.reason,
      hint: flat.hint,
      retry: flat.retry as EngineRefusal["retry"],
    });
  }

  const err = flat.error ?? {};
  throw new ApiError(
    res.status,
    err.code ?? "error",
    err.message ?? `The request failed (${res.status}).`,
    err.code
      ? {
          code: err.code,
          message: err.message ?? "",
          hint: err.hint,
          retry: err.retry as EngineRefusal["retry"],
        }
      : undefined,
  );
}

export const api = {
  get: <T>(path: string) => request<T>("GET", path),
  post: <T>(path: string, body?: unknown) => request<T>("POST", path, body),
  patch: <T>(path: string, body?: unknown) => request<T>("PATCH", path, body),
  del: <T>(path: string) => request<T>("DELETE", path),
};

/* ---------- Shapes the server returns ---------- */

export type Role = "owner" | "admin" | "member" | "viewer";

export interface User {
  id: string;
  org_id: string;
  email: string;
  name: string;
  role: Role;
  groups: string[];
  disabled: boolean;
  created_at: string;
  last_login_at?: string;
}

export interface Org {
  id: string;
  name: string;
  created_at: string;
}

export interface Bootstrap {
  claimed: boolean;
  has_engine: boolean;
  csrf_header: string;
  org?: Org;
  user?: User;
}

export interface Invite {
  email: string;
  role: Role;
  groups: string[];
  invited_by: string;
  created_at: string;
  expires_at: string;
  accepted_at?: string;
}

export interface Connection {
  id: string;
  name: string;
  dialect: string;
  detail: Record<string, string>;
  created_at: string;
  last_ok_at?: string;
  last_error?: string;
}

export interface Metric {
  name: string;
  namespace: string;
  description: string;
  datatype?: string;
  dimensions: string[];
  synonyms?: string[];
}

export interface QueryResult {
  columns: string[];
  rows: unknown[][];
  row_count?: number;
  compiled_sql?: string;
  rollups?: string[];
}

/** A namespace as /v1/namespaces reports it. */
export interface Namespace {
  name: string;
  available: boolean;
  metric_count: number;
  digest?: string;
  owners?: string[];
  error?: string;
}

/**
 * What a question compiles to, without running it.
 *
 * The distinction this makes possible is the one an analyst asks for
 * first: show me the SQL before you charge me for it. It is also the
 * only way to see the SQL of a question the warehouse would reject.
 */
export interface Compiled {
  compiled_sql: string;
  columns: string[];
  dialect: string;
  namespace: string;
  parts?: unknown;
  model_version?: string;
}

/** A query running as a job, which is the only kind that can be cancelled. */
export interface Job {
  job_id: string;
  state: "running" | "succeeded" | "failed" | "cancelled";
  submitted_at: string;
  finished_at?: string;
  duration_ms?: number;
  elapsed_ms?: number;
  compiled_sql?: string;
  columns?: string[];
  rows?: unknown[][];
  row_count?: number;
  rollups?: string[];
  code?: string;
  reason?: string;
  hint?: string;
  retry?: Retry;
}

/** Roles, ordered, so the UI can hide what a person cannot do. */
const RANK: Record<Role, number> = { viewer: 1, member: 2, admin: 3, owner: 4 };

export function atLeast(role: Role | undefined, needed: Role): boolean {
  if (!role) return false;
  return RANK[role] >= RANK[needed];
}

/**
 * Formats a value for a results grid.
 *
 * Numbers arrive as strings from the warehouse when they are decimals,
 * and that is deliberate on the server side: parsing 885.50 into a float
 * to display it is how a trailing zero disappears off a money column. So
 * a numeric-looking string is left exactly as the warehouse wrote it.
 */
export function cell(value: unknown): string {
  if (value === null || value === undefined) return "—";
  if (typeof value === "boolean") return value ? "true" : "false";
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "bigint") return value.toString();
  // A JSON or array column is an object, and String() would render
  // every one of them as the same "[object Object]".
  return JSON.stringify(value) ?? "";
}

/** True when a column should be right-aligned, which is to say numeric. */
export function isNumeric(value: unknown): boolean {
  if (typeof value === "number") return true;
  return typeof value === "string" && value.trim() !== "" && !Number.isNaN(Number(value));
}
