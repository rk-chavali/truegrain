import { useSyncExternalStore } from "react";

/*
  The namespace in scope.

  A workspace can hold several namespaces, each its own model with its
  own owners and its own version. Until now the console showed all of
  them at once as one flat list of prefixed names, which works for the
  fixture and stops working the moment finance and growth both define
  revenue.

  Scope, not navigation, which is why the control sits in the top bar
  beside the workspace and the model version rather than in the rail.
  The rail moves you between screens; this changes what the screen you
  are on is about.

  Held here rather than in a React context because the bar and the
  screens under it are siblings, and a context would mean a provider
  wrapping the whole shell to carry one string. Persisted because
  changing it is a statement about what you are working on, and losing
  it on reload would make it feel like a filter that keeps resetting.
*/

const KEY = "truegrain.namespace.v1";

/** Every namespace, meaning no scope has been chosen. */
export const ALL = "";

const listeners = new Set<() => void>();

function read(): string {
  try {
    return localStorage.getItem(KEY) ?? ALL;
  } catch {
    // Storage can be unavailable in a locked-down browser. Falling back
    // to every namespace is the state that shows the most, which is the
    // safer failure for a filter.
    return ALL;
  }
}

export function setNamespace(name: string): void {
  try {
    if (name === ALL) localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, name);
  } catch {
    // Not worth failing the interaction over; the change still applies
    // for this session through the notification below.
  }
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** The namespace in scope, or ALL. */
export function useNamespace(): string {
  return useSyncExternalStore(subscribe, read, () => ALL);
}

/**
 * Splits a qualified name into its namespace and the rest.
 *
 * Names arrive as `retail.order_revenue` and `retail.customers.region`,
 * so only the first segment is the namespace and the remainder is left
 * intact rather than being split further.
 */
export function splitName(name: string): { namespace: string; rest: string } {
  const dot = name.indexOf(".");
  if (dot < 0) return { namespace: "", rest: name };
  return { namespace: name.slice(0, dot), rest: name.slice(dot + 1) };
}

/** True when a qualified name belongs to the namespace in scope. */
export function inScope(name: string, scope: string): boolean {
  if (scope === ALL) return true;
  return splitName(name).namespace === scope;
}
