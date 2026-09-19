import { Navigate, Route, Routes, useLocation } from "react-router-dom";
import { useSession } from "./lib/session";
import { Shell } from "./components/Shell";
import { AcceptInvite, SignIn, Setup, Unreachable } from "./pages/Gate";
import { Explore } from "./pages/Explore";
import { Model } from "./pages/Model";
import { Activity } from "./pages/Activity";
import { Connections } from "./pages/Connections";
import { People } from "./pages/People";
import { Account } from "./pages/Account";
import { Deployment } from "./pages/Deployment";
import { Checks } from "./pages/Checks";
import { Governance } from "./pages/Governance";
import { Catalog } from "./pages/Catalog";
import { Saved } from "./pages/Saved";
import { Dashboards } from "./pages/Dashboards";

/*
  Four states, decided once, at the top.

  Whether the instance is set up, and whether you are signed in, together
  decide which application this is. Working that out per route is how a
  half-rendered shell flashes before the sign-in screen replaces it.
*/
export function App() {
  const { loading, error, claimed, user } = useSession();
  const location = useLocation();

  // An invitation link has to work before anybody is signed in and
  // whatever the instance's state is, so it is checked first.
  const invite = location.pathname.match(/^\/invite\/(.+)$/);
  if (invite) {
    return (
      <Routes>
        <Route path="/invite/:token" element={<AcceptInvite />} />
      </Routes>
    );
  }

  if (loading) {
    // Deliberately nothing. A spinner that appears for 40ms on every load
    // is worse than a frame of empty page.
    return <div className="gate" />;
  }
  if (error) return <Unreachable error={error} />;
  if (!claimed) return <Setup />;
  if (!user) return <SignIn />;

  return (
    <Routes>
      <Route element={<Shell />}>
        <Route path="/explore" element={<Explore />} />
        <Route path="/dashboards" element={<Dashboards />} />
        <Route path="/saved" element={<Saved />} />
        <Route path="/catalog" element={<Catalog />} />
        <Route path="/model" element={<Model />} />
        <Route path="/activity" element={<Activity />} />
        <Route path="/deployment" element={<Deployment />} />
        <Route path="/checks" element={<Checks />} />
        <Route path="/governance" element={<Governance />} />
        <Route path="/connections" element={<Connections />} />
        <Route path="/people" element={<People />} />
        <Route path="/account" element={<Account />} />
        <Route path="*" element={<Navigate to="/explore" replace />} />
      </Route>
    </Routes>
  );
}
