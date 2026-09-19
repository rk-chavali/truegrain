import { useEffect, useMemo, useState } from "react";
import { Modal, TextInput } from "@mantine/core";
import { useHotkeys } from "@mantine/hooks";
import { useNavigate } from "react-router-dom";
import { api, atLeast, type Metric, type Role } from "../lib/api";
import { inScope, useNamespace } from "../lib/namespace";
import { IconSearch } from "./icons";

/*
  The command palette.

  Every screen in this console is two clicks away already, so this is
  not about navigation. It is about the metrics: a workspace with three
  hundred of them makes the Explore list a place you scroll, and the
  fastest way to a metric you can already name is to type its name.

  Opens on the shortcut every tool in this category uses, which is the
  whole reason to have it: somebody arriving from Metabase or Linear
  presses it without being told.

  Hand-rolled rather than adding a palette library. It is a filtered
  list with a selected index, and a dependency for that would arrive
  with its own theme to override.
*/

interface Item {
  id: string;
  label: string;
  hint: string;
  go: () => void;
}

export function Palette({ role }: { role?: Role }) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [selected, setSelected] = useState(0);
  const [metrics, setMetrics] = useState<Metric[]>([]);
  const navigate = useNavigate();
  const scope = useNamespace();

  useHotkeys([["mod+K", () => setOpen(true)]]);

  useEffect(() => {
    if (!open) return;
    setQuery("");
    setSelected(0);
    api
      .get<{ metrics: Metric[] }>("/v1/metrics")
      .then((d) => setMetrics(d.metrics ?? []))
      // Navigation still works without them, so an engine with no model
      // gets a palette of screens rather than an error.
      .catch(() => setMetrics([]));
  }, [open]);

  const items = useMemo<Item[]>(() => {
    const screens: Item[] = [
      {
        id: "explore",
        label: "Explore",
        hint: "Ask a question",
        go: () => void navigate("/explore"),
      },
      {
        id: "catalog",
        label: "Catalog",
        hint: "Search the model",
        go: () => void navigate("/catalog"),
      },
      {
        id: "model",
        label: "Model",
        hint: "Metrics and dimensions",
        go: () => void navigate("/model"),
      },
      {
        id: "activity",
        label: "Activity",
        hint: "Recorded decisions",
        go: () => void navigate("/activity"),
      },
      {
        id: "governance",
        label: "Governance",
        hint: "What you may read",
        go: () => void navigate("/governance"),
      },
      {
        id: "saved",
        label: "Saved",
        hint: "Your questions",
        go: () => void navigate("/saved"),
      },
      {
        id: "dashboards",
        label: "Dashboards",
        hint: "Collections",
        go: () => void navigate("/dashboards"),
      },
    ];
    if (atLeast(role, "admin")) {
      screens.push(
        {
          id: "checks",
          label: "Checks",
          hint: "Is the model still right",
          go: () => void navigate("/checks"),
        },
        {
          id: "deployment",
          label: "Deployment",
          hint: "What is running",
          go: () => void navigate("/deployment"),
        },
        {
          id: "connections",
          label: "Connections",
          hint: "Warehouses",
          go: () => void navigate("/connections"),
        },
        {
          id: "people",
          label: "People",
          hint: "Who has access",
          go: () => void navigate("/people"),
        },
      );
    }

    const metricItems: Item[] = metrics
      .filter((m) => inScope(m.name, scope))
      .map((m) => ({
        id: `metric:${m.name}`,
        label: m.name,
        hint: m.description || "Metric",
        // Explore reads the metric out of the URL and starts with it
        // chosen, so this lands on a built question rather than an
        // empty builder with the name copied into the search box.
        go: () => void navigate(`/explore?metric=${encodeURIComponent(m.name)}`),
      }));

    const q = query.trim().toLowerCase();
    const all = [...screens, ...metricItems];
    if (!q) return all.slice(0, 12);
    return all
      .filter((i) => i.label.toLowerCase().includes(q) || i.hint.toLowerCase().includes(q))
      .slice(0, 12);
  }, [metrics, query, role, scope, navigate]);

  // The selection can outrun a narrowing list.
  const active = Math.min(selected, Math.max(items.length - 1, 0));

  function choose(item: Item | undefined) {
    if (!item) return;
    setOpen(false);
    item.go();
  }

  return (
    <Modal
      opened={open}
      onClose={() => setOpen(false)}
      withCloseButton={false}
      size="lg"
      padding={0}
      centered
    >
      <div className="palette">
        <TextInput
          autoFocus
          size="md"
          variant="unstyled"
          placeholder="Go to a screen, or find a metric"
          aria-label="Search screens and metrics"
          leftSection={<IconSearch size={15} />}
          value={query}
          onChange={(e) => {
            setQuery(e.currentTarget.value);
            setSelected(0);
          }}
          onKeyDown={(e) => {
            if (e.key === "ArrowDown") {
              e.preventDefault();
              setSelected((i) => Math.min(i + 1, items.length - 1));
            } else if (e.key === "ArrowUp") {
              e.preventDefault();
              setSelected((i) => Math.max(i - 1, 0));
            } else if (e.key === "Enter") {
              e.preventDefault();
              choose(items[active]);
            }
          }}
          aria-controls="palette-results"
          aria-activedescendant={items[active] ? `palette-${items[active].id}` : undefined}
        />

        <ul className="palette-list" id="palette-results" role="listbox">
          {items.length === 0 ? (
            <li className="palette-empty faint small">Nothing matches {query}.</li>
          ) : null}
          {items.map((item, i) => (
            <li
              key={item.id}
              id={`palette-${item.id}`}
              role="option"
              aria-selected={i === active}
              className={i === active ? "palette-item is-active" : "palette-item"}
              onMouseEnter={() => setSelected(i)}
              onClick={() => choose(item)}
            >
              <span className="palette-label">{item.label}</span>
              <span className="palette-hint faint">{item.hint}</span>
            </li>
          ))}
        </ul>
      </div>
    </Modal>
  );
}
