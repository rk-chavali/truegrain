import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { api, type Bootstrap, type User } from "./api";

/*
  Who is signed in, and whether this instance has been set up at all.

  One request answers both, because the app cannot render anything until
  it knows: a setup form, a sign-in form, and the application itself are
  three different first paints, and asking three separate questions means
  showing the wrong one first every time.
*/

interface SessionValue {
  loading: boolean;
  error?: string;
  claimed: boolean;
  hasEngine: boolean;
  user?: User;
  orgName?: string;
  /** The loaded model's content hash, shown in the top bar. */
  modelVersion?: string;
  modelName?: string;
  refresh: () => Promise<void>;
  signOut: () => Promise<void>;
}

const Ctx = createContext<SessionValue | null>(null);

export function SessionProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<{
    loading: boolean;
    error?: string;
    boot?: Bootstrap;
    model?: { model: string; model_version: string };
  }>({ loading: true });

  const refresh = useCallback(async () => {
    try {
      const boot = await api.get<Bootstrap>("/api/bootstrap");

      // The model version is a second request because it is behind
      // authentication and bootstrap is not. Failing to read it is not a
      // failure to load the app: it only means the top bar is missing a
      // hash, which is better than a blank screen.
      let model: { model: string; model_version: string } | undefined;
      if (boot.user && boot.has_engine) {
        try {
          model = await api.get<{ model: string; model_version: string }>("/v1/model/version");
        } catch {
          model = undefined;
        }
      }
      setState({ loading: false, boot, model });
    } catch (err) {
      setState({
        loading: false,
        error: err instanceof Error ? err.message : "The server is not reachable.",
      });
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const signOut = useCallback(async () => {
    try {
      await api.post("/api/auth/logout");
    } finally {
      // Refresh either way. If the call failed because the session was
      // already gone, the right outcome is still the sign-in screen.
      await refresh();
    }
  }, [refresh]);

  const value = useMemo<SessionValue>(
    () => ({
      loading: state.loading,
      error: state.error,
      claimed: state.boot?.claimed ?? false,
      hasEngine: state.boot?.has_engine ?? false,
      user: state.boot?.user,
      orgName: state.boot?.org?.name,
      modelVersion: state.model?.model_version,
      modelName: state.model?.model,
      refresh,
      signOut,
    }),
    [state, refresh, signOut],
  );

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useSession(): SessionValue {
  const value = useContext(Ctx);
  if (!value) throw new Error("useSession outside SessionProvider");
  return value;
}
