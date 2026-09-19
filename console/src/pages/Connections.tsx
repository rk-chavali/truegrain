import { useEffect, useState, type FormEvent } from "react";
import { Button, Group, Select, Textarea, TextInput } from "@mantine/core";
import { api, type Connection } from "../lib/api";
import {
  DataTable,
  Empty,
  Notice,
  PageHead,
  Panel,
  Strip,
  Tag,
  ago,
  type Column,
} from "../components/ui";

/*
  Warehouse connections.

  This is the screen that stores a credential, so it is the screen that
  has to be honest about it. The form says where the secret goes and
  what protects it, once, rather than leaving somebody to assume either
  that it is encrypted or that it is not.

  A stored secret is never read back. The field is write-only: editing a
  connection means typing the credential again, which is mildly annoying
  and is the only version where the API cannot be asked to hand it over.
*/

const DIALECTS = [
  { id: "postgres", label: "PostgreSQL", secret: "Connection string", verified: true },
  { id: "duckdb", label: "DuckDB", secret: "Database file path", verified: true },
  { id: "bigquery", label: "BigQuery", secret: "Project ID", verified: true },
  { id: "snowflake", label: "Snowflake", secret: "Private key (PEM)", verified: false },
  { id: "redshift", label: "Redshift", secret: "Connection string", verified: false },
  { id: "databricks", label: "Databricks", secret: "Connection string", verified: false },
  { id: "clickhouse", label: "ClickHouse", secret: "Connection string", verified: false },
  { id: "trino", label: "Trino", secret: "Connection string", verified: false },
  { id: "athena", label: "Athena", secret: "Connection string", verified: false },
];

export function Connections() {
  const [list, setList] = useState<Connection[]>();
  const [error, setError] = useState<string>();
  const [adding, setAdding] = useState(false);

  async function load() {
    try {
      const d = await api.get<{ connections: Connection[] }>("/api/connections");
      setList(d.connections);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not load connections.");
    }
  }

  useEffect(() => {
    void load();
  }, []);

  async function remove(name: string) {
    if (
      !window.confirm(`Delete the connection "${name}"? The stored credential goes with it.`)
    ) {
      return;
    }
    try {
      await api.del(`/api/connections/${encodeURIComponent(name)}`);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not delete it.");
    }
  }

  const failing = (list ?? []).filter((c) => c.last_error).length;
  const used = (list ?? []).filter((c) => c.last_ok_at).length;

  const columns: Column<Connection>[] = [
    {
      key: "name",
      header: "Name",
      render: (c) => <b>{c.name}</b>,
      sort: (a, b) => a.name.localeCompare(b.name),
    },
    {
      key: "dialect",
      header: "Type",
      shrink: true,
      render: (c) => DIALECTS.find((d) => d.id === c.dialect)?.label ?? c.dialect,
    },
    {
      key: "where",
      header: "Where",
      render: (c) => (
        <span className="muted">
          {Object.entries(c.detail)
            .map(([k, v]) => `${k}: ${v}`)
            .join(", ") || "—"}
        </span>
      ),
    },
    {
      key: "used",
      header: "Last used",
      shrink: true,
      render: (c) =>
        c.last_error ? (
          <Tag tone="refused" title={c.last_error}>
            Failing
          </Tag>
        ) : c.last_ok_at ? (
          <span className="muted">{ago(c.last_ok_at)}</span>
        ) : (
          <span className="faint">Not used yet</span>
        ),
    },
    {
      key: "actions",
      header: "",
      shrink: true,
      render: (c) => (
        <Button
          variant="subtle"
          color="danger"
          size="compact-xs"
          onClick={() => void remove(c.name)}
        >
          Delete
        </Button>
      ),
    },
  ];

  return (
    <div className="page page-pad w-read">
      <PageHead
        title="Connections"
        actions={
          !adding ? <Button onClick={() => setAdding(true)}>Add a connection</Button> : null
        }
      />

      <Strip
        specs={[
          { k: "Configured", v: list?.length ?? "—" },
          { k: "Used", v: used, tone: used ? "ok" : "off" },
          { k: "Failing", v: failing, tone: failing ? "refused" : "ok" },
          {
            k: "Secrets",
            v: "encrypted at rest",
            tone: "ok",
            title: "AES-GCM with a key from the environment, never in the database",
          },
        ]}
      />

      {error ? <Notice>{error}</Notice> : null}

      {adding ? (
        <AddConnection
          onCancel={() => setAdding(false)}
          onSaved={async () => {
            setAdding(false);
            await load();
          }}
        />
      ) : null}

      <Panel tight>
        <DataTable
          id="connections"
          rows={list}
          columns={columns}
          rowKey={(c) => c.id}
          empty={
            <Empty
              title="Nothing is connected yet"
              action={<Button onClick={() => setAdding(true)}>Add a connection</Button>}
            >
              <p>
                Add a warehouse and this instance can compile and run against it. Nothing leaves
                your network: the engine connects out, and no data is copied here.
              </p>
            </Empty>
          }
        />
      </Panel>
    </div>
  );
}

function AddConnection({
  onCancel,
  onSaved,
}: {
  onCancel: () => void;
  onSaved: () => Promise<void>;
}) {
  const [name, setName] = useState("");
  const [dialect, setDialect] = useState("postgres");
  const [secret, setSecret] = useState("");
  const [host, setHost] = useState("");
  const [database, setDatabase] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  const spec = DIALECTS.find((d) => d.id === dialect);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      const detail: Record<string, string> = {};
      if (host) detail.host = host;
      if (database) detail.database = database;
      await api.post("/api/connections", { name, dialect, secret, detail });
      await onSaved();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not save it.");
      setBusy(false);
    }
  }

  return (
    <Panel title="Add a connection">
      <form onSubmit={submit}>
        {error ? <Notice>{error}</Notice> : null}

        <TextInput
          label="Name"
          required
          value={name}
          onChange={(e) => setName(e.currentTarget.value)}
          placeholder="production"
          description="What people will call it."
          autoFocus
          mb="md"
        />

        <Select
          label="Warehouse"
          value={dialect}
          onChange={(v) => setDialect(v ?? "postgres")}
          allowDeselect={false}
          data={DIALECTS.map((d) => ({
            value: d.id,
            label: d.verified ? d.label : `${d.label} (compiles, never run)`,
          }))}
          description={
            spec && !spec.verified
              ? `This engine compiles ${spec.label} SQL and has never executed it against a real instance. It may work; nobody has proven it does.`
              : undefined
          }
          mb="md"
        />

        <Group grow align="flex-start" mb="md">
          <TextInput
            label="Host"
            value={host}
            onChange={(e) => setHost(e.currentTarget.value)}
            placeholder="warehouse.internal"
          />
          <TextInput
            label="Database"
            value={database}
            onChange={(e) => setDatabase(e.currentTarget.value)}
            placeholder="analytics"
          />
        </Group>

        <Textarea
          label={spec?.secret ?? "Credential"}
          required
          rows={3}
          value={secret}
          onChange={(e) => setSecret(e.currentTarget.value)}
          placeholder="postgres://user:password@host:5432/database?sslmode=require"
          autoComplete="off"
          spellCheck={false}
          description="Encrypted before it is stored, and never sent back to this page. To change it later you type it again."
          mb="lg"
        />

        <Group>
          <Button type="submit" loading={busy}>
            Save connection
          </Button>
          <Button variant="default" type="button" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
        </Group>
      </form>
    </Panel>
  );
}
