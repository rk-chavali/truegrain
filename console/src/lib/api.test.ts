import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ApiError, api, atLeast, cell, isNumeric, CSRF_HEADER } from "./api";

/*
  The API client.

  Two servers answer on this origin and they describe failure
  differently: the control plane wraps its own errors, the engine
  returns a refusal flat. Getting that wrong is not cosmetic. It is what
  made the most valuable screen in the product say "The request failed
  (422)" instead of naming the metric that answers.
*/

const fetchMock = vi.fn();

beforeEach(() => {
  fetchMock.mockReset();
  vi.stubGlobal("fetch", fetchMock);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

function respond(status: number, body: unknown) {
  fetchMock.mockResolvedValue({
    status,
    ok: status >= 200 && status < 300,
    text: () => Promise.resolve(JSON.stringify(body)),
  });
}

test("every request carries the CSRF header, which a cross-site form cannot add", async () => {
  respond(200, { ok: true });
  await api.get("/api/bootstrap");

  const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
  expect((init.headers as Record<string, string>)[CSRF_HEADER]).toBe("1");
  expect(init.credentials).toBe("same-origin");
});

test("the engine's flat refusal keeps its hint", async () => {
  /*
    The hint is the most valuable field in the product: it names the
    metric that answers the question that was just declined. Flattening
    the refusal into a message string throws it away.
  */
  respond(422, {
    code: "fan_out_would_inflate",
    reason: 'metric "order_revenue" aggregates SUM(...) over orders',
    hint: "Metrics defined at the order_lines grain answer this correctly: line_revenue, units_sold.",
    retry: "modify",
  });

  const err = await api.post("/v1/query", {}).catch((e: unknown) => e);

  expect(err).toBeInstanceOf(ApiError);
  const api_err = err as ApiError;
  expect(api_err.code).toBe("fan_out_would_inflate");
  expect(api_err.message).toContain("order_revenue");
  expect(api_err.detail?.hint).toContain("line_revenue");
  expect(api_err.detail?.retry).toBe("modify");
});

test("the control plane's wrapped error is read too", async () => {
  respond(409, {
    error: { code: "email_taken", message: "that email already has an account" },
  });

  const err = (await api.post("/api/people/invite", {}).catch((e: unknown) => e)) as ApiError;

  expect(err.code).toBe("email_taken");
  expect(err.message).toBe("that email already has an account");
});

test("an unauthenticated response is recognisable, so the app can show the gate", async () => {
  respond(401, { error: { code: "unauthenticated", message: "sign in to continue" } });

  const err = (await api.get("/v1/metrics").catch((e: unknown) => e)) as ApiError;

  expect(err.unauthenticated).toBe(true);
});

test("a non-JSON body says so rather than surfacing a parse error", async () => {
  fetchMock.mockResolvedValue({
    status: 502,
    ok: false,
    text: () => Promise.resolve("<html>Bad Gateway</html>"),
  });

  const err = (await api.get("/v1/metrics").catch((e: unknown) => e)) as ApiError;

  expect(err.code).toBe("unreadable");
  expect(err.message).toContain("not JSON");
});

/*
  Role comparison.

  Every screen hides what a person cannot do by asking this. Comparing
  role strings directly is the bug it exists to prevent: the day
  somebody writes `role === "admin"` is the day an owner stops being
  able to do an admin's job.
*/
test("a role carries the authority of every role below it", () => {
  expect(atLeast("owner", "admin")).toBe(true);
  expect(atLeast("admin", "admin")).toBe(true);
  expect(atLeast("member", "admin")).toBe(false);
  expect(atLeast("viewer", "member")).toBe(false);
  expect(atLeast(undefined, "viewer")).toBe(false);
});

/*
  Rendering a cell.

  A decimal arrives as a string because the server refuses to round-trip
  it through a float. The formatter must not undo that.
*/
test("a decimal is rendered exactly as the warehouse wrote it", () => {
  expect(cell("885.50")).toBe("885.50");
  expect(cell("0.100")).toBe("0.100");
});

test("a null renders as a dash rather than the word null", () => {
  expect(cell(null)).toBe("—");
  expect(cell(undefined)).toBe("—");
});

test("numeric detection decides alignment, and a label is not a number", () => {
  expect(isNumeric("885.50")).toBe(true);
  expect(isNumeric(42)).toBe(true);
  expect(isNumeric("North East")).toBe(false);
  expect(isNumeric("")).toBe(false);
  expect(isNumeric(null)).toBe(false);
});

test("a JSON column renders its value, not [object Object]", () => {
  expect(cell({ tier: "gold" })).toBe('{"tier":"gold"}');
  expect(cell(["a", "b"])).toBe('["a","b"]');
});
