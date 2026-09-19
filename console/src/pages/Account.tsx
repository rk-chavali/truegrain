import { useState, type FormEvent } from "react";
import { api } from "../lib/api";
import { Button, PasswordInput } from "@mantine/core";
import { useSession } from "../lib/session";
import { Notice, PageHead, Strip } from "../components/ui";

/*
  Your own account.

  Changing a password signs every session out, including this one. That
  is correct and it is stated before the button rather than discovered
  after it: the reason to change a password is usually that it may have
  leaked, and leaving the sessions opened with it alive would fix
  nothing.
*/
export function Account() {
  const { user, refresh } = useSession();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await api.post("/api/me/password", { current, new: next });
      // The server ended this session on purpose. Refreshing drops the
      // app back to the sign-in screen, which is the honest outcome.
      await refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : "That did not work.");
      setBusy(false);
    }
  }

  return (
    <div className="page page-pad w-form">
      <PageHead title="Account" />

      <Strip
        specs={[
          { k: "Signed in as", v: user?.email ?? "\u2014" },
          { k: "Role", v: user?.role ?? "\u2014" },
          {
            k: "Groups",
            v: user?.groups.length ? user.groups.join(", ") : "none",
            tone: user?.groups.length ? "ok" : "off",
            title: "Policy grants written for a group apply to you only if you are in it",
          },
        ]}
      />

      <div className="panel" style={{ marginBottom: "var(--s5)" }}>
        <div className="panel-head">You</div>
        <div className="panel-body">
          <table className="grid">
            <tbody>
              <tr>
                <td className="muted">Name</td>
                <td>{user?.name || <span className="faint">Not set</span>}</td>
              </tr>
              <tr>
                <td className="muted">Email</td>
                <td>{user?.email}</td>
              </tr>
              <tr>
                <td className="muted">Role</td>
                <td>{user?.role}</td>
              </tr>
              <tr>
                <td className="muted">Groups</td>
                <td>
                  {user?.groups.length ? (
                    user.groups.join(", ")
                  ) : (
                    <span className="faint">
                      None. Policy grants written for a group do not apply to you.
                    </span>
                  )}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">Change your password</div>
        <form className="panel-body" onSubmit={submit}>
          {error ? <Notice>{error}</Notice> : null}
          <PasswordInput
            label="Current password"
            required
            value={current}
            onChange={(e) => setCurrent(e.currentTarget.value)}
            autoComplete="current-password"
            mb="md"
          />
          <PasswordInput
            label="New password"
            required
            minLength={12}
            value={next}
            onChange={(e) => setNext(e.currentTarget.value)}
            autoComplete="new-password"
            description="At least 12 characters."
            mb="lg"
          />
          <Button type="submit" loading={busy}>
            Change password and sign out everywhere
          </Button>
          <p className="field-hint" style={{ marginTop: "var(--s2)" }}>
            Every session ends, on every device, including this one. You will sign in again with
            the new password.
          </p>
        </form>
      </div>
    </div>
  );
}
