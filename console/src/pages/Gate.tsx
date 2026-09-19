import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { Button, PasswordInput, Skeleton, TextInput } from "@mantine/core";
import { api } from "../lib/api";
import { useSession } from "../lib/session";
import { Grain } from "../components/icons";
import { Notice } from "../components/ui";

/*
  The three ways somebody arrives with no session: setting the instance
  up, signing in, or accepting an invitation.

  One frame and one voice. Copy says what will happen rather than what
  the system is doing: "Create the first account", not "Initialize
  instance".
*/

function Frame({
  heading,
  lede,
  foot,
  children,
}: {
  heading: string;
  lede?: string;
  foot?: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className="gate">
      <div className="gate-inner">
        <div className="gate-card">
          <div className="gate-mark">
            <Grain size={16} />
            <span>truegrain</span>
          </div>
          <h1>{heading}</h1>
          {lede ? <p className="lede">{lede}</p> : null}
          {children}
        </div>
      </div>
      <div className="gate-foot">
        {foot ?? "Self-hosted. Your warehouse stays in your account."}
      </div>
    </div>
  );
}

const PASSWORD_HINT =
  "At least 12 characters. Length is the only rule: a long phrase beats a short one with a symbol in it.";

/** First run. Open exactly once, and it says so. */
export function Setup() {
  const { refresh } = useSession();
  const [org, setOrg] = useState("");
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await api.post("/api/auth/claim", { org, name, email, password });
      await refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Setup failed.");
      setBusy(false);
    }
  }

  return (
    <Frame
      heading="Create the first account"
      lede="Nobody has set this instance up yet. This account owns it, and sign-up closes as soon as it exists."
      foot="After this, people join by invitation."
    >
      <form onSubmit={submit}>
        {error ? <Notice>{error}</Notice> : null}
        <TextInput
          label="Workspace name"
          value={org}
          onChange={(e) => setOrg(e.currentTarget.value)}
          placeholder="Acme Analytics"
          autoComplete="organization"
          mb="md"
        />
        <TextInput
          label="Your name"
          value={name}
          onChange={(e) => setName(e.currentTarget.value)}
          autoComplete="name"
          mb="md"
        />
        <TextInput
          label="Email"
          type="email"
          required
          value={email}
          onChange={(e) => setEmail(e.currentTarget.value)}
          autoComplete="username"
          mb="md"
        />
        <PasswordInput
          label="Password"
          required
          minLength={12}
          value={password}
          onChange={(e) => setPassword(e.currentTarget.value)}
          autoComplete="new-password"
          description={PASSWORD_HINT}
          mb="lg"
        />
        <Button type="submit" loading={busy} fullWidth>
          {busy ? "Creating the account" : "Create account and sign in"}
        </Button>
      </form>
    </Frame>
  );
}

export function SignIn() {
  const { refresh } = useSession();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await api.post("/api/auth/login", { email, password });
      await refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Sign-in failed.");
      setBusy(false);
    }
  }

  return (
    <Frame heading="Sign in">
      <form onSubmit={submit}>
        {error ? <Notice>{error}</Notice> : null}
        <TextInput
          label="Email"
          type="email"
          required
          value={email}
          onChange={(e) => setEmail(e.currentTarget.value)}
          autoComplete="username"
          data-autofocus
          autoFocus
          mb="md"
        />
        <PasswordInput
          label="Password"
          required
          value={password}
          onChange={(e) => setPassword(e.currentTarget.value)}
          autoComplete="current-password"
          mb="lg"
        />
        <Button type="submit" loading={busy} fullWidth>
          Sign in
        </Button>
      </form>
    </Frame>
  );
}

/** Accepting an invitation. The address is fixed by the invite. */
export function AcceptInvite() {
  const { token = "" } = useParams();
  const { refresh } = useSession();
  const navigate = useNavigate();

  const [invite, setInvite] = useState<{ email: string; role: string }>();
  const [lookupError, setLookupError] = useState<string>();
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api
      .get<{ email: string; role: string }>(`/api/invites/${encodeURIComponent(token)}`)
      .then(setInvite)
      .catch((err: unknown) =>
        setLookupError(err instanceof Error ? err.message : "That invitation is not valid."),
      );
  }, [token]);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await api.post(`/api/invites/${encodeURIComponent(token)}/accept`, { name, password });
      await refresh();
      await navigate("/explore");
    } catch (err) {
      setError(err instanceof Error ? err.message : "That did not work.");
      setBusy(false);
    }
  }

  if (lookupError) {
    return (
      <Frame heading="This invitation cannot be used" lede={lookupError}>
        <p className="muted">
          Invitations last seven days and work once. Ask whoever sent it to issue a new one.
        </p>
        <Button variant="default" onClick={() => navigate("/")}>
          Go to sign in
        </Button>
      </Frame>
    );
  }

  if (!invite) {
    return (
      <Frame heading="Checking the invitation">
        <Skeleton height={10} width="70%" radius="xs" />
      </Frame>
    );
  }

  return (
    <Frame
      heading="Accept your invitation"
      lede={`You have been invited as ${invite.email}, with the ${invite.role} role.`}
    >
      <form onSubmit={submit}>
        {error ? <Notice>{error}</Notice> : null}
        <TextInput
          label="Your name"
          value={name}
          onChange={(e) => setName(e.currentTarget.value)}
          autoComplete="name"
          autoFocus
          mb="md"
        />
        <PasswordInput
          label="Choose a password"
          required
          minLength={12}
          value={password}
          onChange={(e) => setPassword(e.currentTarget.value)}
          autoComplete="new-password"
          description="At least 12 characters."
          mb="lg"
        />
        <Button type="submit" loading={busy} fullWidth>
          Create account
        </Button>
      </form>
    </Frame>
  );
}

/** Shown when the server cannot be reached at all. */
export function Unreachable({ error }: { error: string }) {
  return (
    <Frame heading="Cannot reach the server" lede={error}>
      <p className="muted">
        The page is running but the API behind it is not answering. Check that the process is
        up, then reload.
      </p>
      <Button variant="default" onClick={() => window.location.reload()}>
        Reload
      </Button>
    </Frame>
  );
}
