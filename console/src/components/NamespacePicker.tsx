import { useEffect, useState } from "react";
import { Select } from "@mantine/core";
import { api, type Namespace } from "../lib/api";
import { ALL, setNamespace, useNamespace } from "../lib/namespace";

/*
  Picks the namespace the current screen is about.

  In the bar rather than the rail, because this is scope and not
  navigation: the rail moves you between screens, this changes what the
  screen you are on is looking at. It sits beside the workspace and the
  model version, which are the two other facts of the same kind.

  Absent when there is nothing to choose. One namespace is the common
  case for a first install, and a select with a single option is a
  control that asks a question with one answer.
*/
export function NamespacePicker() {
  const [namespaces, setNamespaces] = useState<Namespace[]>();
  const scope = useNamespace();

  useEffect(() => {
    api
      .get<{ namespaces: Namespace[] }>("/v1/namespaces")
      .then((d) => setNamespaces(d.namespaces ?? []))
      // No model, no namespaces, and the bar already says so. A second
      // message here would be the same fact twice.
      .catch(() => setNamespaces([]));
  }, []);

  if (!namespaces || namespaces.length < 2) return null;

  return (
    <label className="fact fact-control">
      <span>Namespace</span>
      <Select
        size="xs"
        variant="unstyled"
        aria-label="Namespace in scope"
        value={scope}
        onChange={(value) => setNamespace(value ?? ALL)}
        data={[
          { value: ALL, label: `All ${namespaces.length}` },
          ...namespaces.map((ns) => ({
            value: ns.name,
            /*
              An unavailable namespace is still offered. Choosing it is
              how somebody reads the error explaining why it did not
              load, and hiding it would make a broken namespace look
              like a namespace that was never declared.
            */
            label: ns.available ? `${ns.name} (${ns.metric_count})` : `${ns.name} — not loaded`,
          })),
        ]}
      />
    </label>
  );
}
