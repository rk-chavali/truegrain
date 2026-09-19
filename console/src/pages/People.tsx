import { useEffect, useState, type FormEvent } from "react";
import { Button, CopyButton, Group, Select, TextInput } from "@mantine/core";
import { api, atLeast, type Invite, type Role, type User } from "../lib/api";
import { useSession } from "../lib/session";
import {
  DataTable,
  Empty,
  Notice,
  PageHead,
  Panel,
  SectionHead,
  Strip,
  Tag,
  ago,
  type Column,
} from "../components/ui";

/*
  People.

  Two lists that are really one question: who can get in. Accounts, and
  invitations that have not been used yet.

  There is no mail sender. An invitation produces a link the admin sends
  however they already talk to that person, which works on a network
  with no outbound mail and skips the single most common reason a
  self-hosted install stalls. The link is shown once, and the page says
  so rather than letting somebody close it and go looking later.
*/

/* Each line stands on its own, because it is shown under the form
   without the role name in front of it. */
const ROLES: { id: Role; what: string }[] = [
  { id: "viewer", what: "Viewers read metrics and run queries" },
  { id: "member", what: "Members read metrics and run queries. This is the usual choice" },
  { id: "admin", what: "Admins do that, and manage people and connections" },
  { id: "owner", what: "Owners do everything, including managing other owners" },
];

export function People() {
  const { user } = useSession();
  const [users, setUsers] = useState<User[]>();
  const [invites, setInvites] = useState<Invite[]>();
  const [error, setError] = useState<string>();
  const [link, setLink] = useState<{ email: string; url: string }>();

  async function load() {
    try {
      const d = await api.get<{ users: User[]; invites: Invite[] }>("/api/people");
      setUsers(d.users);
      setInvites(d.invites.filter((i) => !i.accepted_at));
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not load people.");
    }
  }

  useEffect(() => {
    void load();
  }, []);

  async function change(target: User, patch: Partial<{ role: Role; disabled: boolean }>) {
    setError(undefined);
    try {
      await api.patch(`/api/people/${encodeURIComponent(target.id)}`, {
        role: patch.role ?? target.role,
        groups: target.groups,
        disabled: patch.disabled ?? target.disabled,
      });
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "That change did not apply.");
    }
  }

  /** An admin may act on somebody at or below their own role, never themselves. */
  function editable(u: User): boolean {
    return atLeast(user?.role, "admin") && atLeast(user?.role, u.role) && u.id !== user?.id;
  }

  const userColumns: Column<User>[] = [
    {
      key: "person",
      header: "Person",
      render: (u) => (
        <>
          <b>{u.name || u.email}</b>
          <div className="faint small">{u.email}</div>
          {u.id === user?.id || u.disabled ? (
            <Group gap={4} mt={2}>
              {u.id === user?.id ? <Tag>You</Tag> : null}
              {u.disabled ? <Tag tone="refused">Disabled</Tag> : null}
            </Group>
          ) : null}
        </>
      ),
      sort: (a, b) => (a.name || a.email).localeCompare(b.name || b.email),
    },
    {
      key: "role",
      header: "Role",
      shrink: true,
      sort: (a, b) => a.role.localeCompare(b.role),
      render: (u) =>
        editable(u) ? (
          <Select
            value={u.role}
            onChange={(v) => v && void change(u, { role: v })}
            data={ROLES.map((r) => ({ value: r.id, label: r.id }))}
            allowDeselect={false}
            w="7rem"
            aria-label={`Role for ${u.email}`}
          />
        ) : (
          u.role
        ),
    },
    {
      key: "groups",
      header: "Groups",
      render: (u) => <span className="muted">{u.groups.join(", ") || "—"}</span>,
    },
    {
      key: "seen",
      header: "Last signed in",
      shrink: true,
      render: (u) => (
        <span className="muted">{u.last_login_at ? ago(u.last_login_at) : "Never"}</span>
      ),
    },
    {
      key: "actions",
      header: "",
      shrink: true,
      render: (u) =>
        editable(u) ? (
          <Button
            variant="subtle"
            color={u.disabled ? "gray" : "danger"}
            size="compact-xs"
            onClick={() => void change(u, { disabled: !u.disabled })}
          >
            {u.disabled ? "Enable" : "Disable"}
          </Button>
        ) : null,
    },
  ];

  const inviteColumns: Column<Invite>[] = [
    { key: "email", header: "Email", render: (i) => i.email },
    { key: "role", header: "Role", shrink: true, render: (i) => i.role },
    {
      key: "expires",
      header: "Expires",
      shrink: true,
      render: (i) => (
        <span className="muted">{new Date(i.expires_at).toLocaleDateString()}</span>
      ),
    },
    {
      key: "actions",
      header: "",
      shrink: true,
      render: (i) => (
        <Button
          variant="subtle"
          color="danger"
          size="compact-xs"
          onClick={async () => {
            await api.del(`/api/people/invite/${encodeURIComponent(i.email)}`);
            await load();
          }}
        >
          Withdraw
        </Button>
      ),
    },
  ];

  return (
    <div className="page page-pad w-read">
      <PageHead title="People" />

      <Strip
        specs={[
          { k: "Accounts", v: users?.length ?? "—" },
          {
            k: "Admins",
            v: (users ?? []).filter((u) => u.role === "owner" || u.role === "admin").length,
          },
          { k: "Disabled", v: (users ?? []).filter((u) => u.disabled).length, tone: "off" },
          { k: "Invited", v: invites?.length ?? 0, tone: invites?.length ? "refused" : "ok" },
          {
            k: "Groups",
            v: new Set((users ?? []).flatMap((u) => u.groups)).size,
            title: "Groups are what the governance policy reads",
          },
        ]}
      />

      {error ? <Notice>{error}</Notice> : null}

      {link ? (
        <Notice kind="ok" title={`Send this link to ${link.email}`}>
          <p className="small">
            It works once, lasts seven days, and is not stored anywhere. If it is lost, invite
            them again.
          </p>
          <TextInput
            readOnly
            value={link.url}
            onFocus={(e) => e.currentTarget.select()}
            className="input-mono"
            mb="sm"
            aria-label="Invitation link"
          />
          <Group gap="xs">
            <CopyButton value={link.url}>
              {({ copied, copy }) => (
                <Button variant="default" size="compact-xs" onClick={copy}>
                  {copied ? "Copied" : "Copy link"}
                </Button>
              )}
            </CopyButton>
            <Button
              variant="subtle"
              color="gray"
              size="compact-xs"
              onClick={() => setLink(undefined)}
            >
              Done
            </Button>
          </Group>
        </Notice>
      ) : null}

      <InviteForm
        onInvited={async (email, path) => {
          setLink({ email, url: `${window.location.origin}${path}` });
          await load();
        }}
        onError={setError}
      />

      <SectionHead title="Accounts" />
      <Panel tight>
        <DataTable id="people.users" rows={users} columns={userColumns} rowKey={(u) => u.id} />
      </Panel>

      <SectionHead title="Waiting to join" />
      <Panel tight>
        <DataTable
          id="people.invites"
          rows={invites}
          columns={inviteColumns}
          rowKey={(i) => i.email}
          empty={
            <Empty title="No invitations are outstanding">
              <p>Invite somebody above and their link appears here until it is used.</p>
            </Empty>
          }
        />
      </Panel>
    </div>
  );
}

function InviteForm({
  onInvited,
  onError,
}: {
  onInvited: (email: string, path: string) => Promise<void>;
  onError: (message: string) => void;
}) {
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<Role>("member");
  const [groups, setGroups] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    try {
      const d = await api.post<{ accept_path: string; email: string }>("/api/people/invite", {
        email,
        role,
        groups: groups
          .split(",")
          .map((g) => g.trim())
          .filter(Boolean),
      });
      setEmail("");
      setGroups("");
      await onInvited(d.email, d.accept_path);
    } catch (err) {
      onError(err instanceof Error ? err.message : "Could not create the invitation.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Panel title="Invite somebody">
      <form onSubmit={submit}>
        <Group align="flex-end" gap="md" wrap="wrap">
          <TextInput
            label="Email"
            type="email"
            required
            value={email}
            onChange={(e) => setEmail(e.currentTarget.value)}
            placeholder="person@yourcompany.com"
            style={{ flex: "2 1 16rem" }}
          />
          <Select
            label="Role"
            value={role}
            onChange={(v) => setRole((v as Role) ?? "member")}
            data={ROLES.filter((r) => r.id !== "owner").map((r) => ({
              value: r.id,
              label: r.id,
            }))}
            allowDeselect={false}
            style={{ flex: "1 1 8rem" }}
          />
          <TextInput
            label="Groups"
            value={groups}
            onChange={(e) => setGroups(e.currentTarget.value)}
            placeholder="analysts"
            style={{ flex: "1 1 10rem" }}
          />
          <Button type="submit" loading={busy}>
            Create invitation
          </Button>
        </Group>
        <p className="field-hint" style={{ marginTop: "var(--s3)" }}>
          {ROLES.find((r) => r.id === role)?.what}. Ownership is granted after the account
          exists, not by invitation.
        </p>
      </form>
    </Panel>
  );
}
