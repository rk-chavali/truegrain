import type { ReactNode } from "react";
import { NavLink, Outlet } from "react-router-dom";
import { Button, Tooltip } from "@mantine/core";
import { atLeast } from "../lib/api";
import { useSession } from "../lib/session";
import { ColorSchemeToggle } from "./ui";
import { NamespacePicker } from "./NamespacePicker";
import { Palette } from "./Palette";
import {
  Grain,
  IconAccount,
  IconActivity,
  IconCatalog,
  IconChecks,
  IconConnection,
  IconDashboard,
  IconDeployment,
  IconExplore,
  IconGovernance,
  IconModel,
  IconPeople,
  IconSaved,
} from "./icons";

/*
  The shell.

  The top bar carries the workspace and the model version and never
  repeats the page title, which is already an h1 a few pixels below it.
  The old bar said "Model" directly above a heading that said "Model",
  which is a line of chrome spent saying nothing.

  One nav list, rendered twice: as the rail on a wide screen and as a
  scrolling strip on a narrow one. Two hand-maintained copies is how a
  link ends up in one and not the other.
*/

interface Link {
  to: string;
  label: string;
  icon: (p: { size?: number }) => ReactNode;
}

function sections(admin: boolean): { group: string; links: Link[] }[] {
  const groups: { group: string; links: Link[] }[] = [
    {
      group: "Analyse",
      links: [
        { to: "/explore", label: "Explore", icon: IconExplore },
        { to: "/dashboards", label: "Dashboards", icon: IconDashboard },
        { to: "/saved", label: "Saved", icon: IconSaved },
      ],
    },
    {
      group: "Understand",
      links: [
        { to: "/catalog", label: "Catalog", icon: IconCatalog },
        { to: "/model", label: "Model", icon: IconModel },
        { to: "/activity", label: "Activity", icon: IconActivity },
        { to: "/governance", label: "Governance", icon: IconGovernance },
      ],
    },
  ];
  if (admin) {
    groups.push({
      group: "Operate",
      links: [
        { to: "/deployment", label: "Deployment", icon: IconDeployment },
        { to: "/checks", label: "Checks", icon: IconChecks },
        { to: "/connections", label: "Connections", icon: IconConnection },
        { to: "/people", label: "People", icon: IconPeople },
      ],
    });
  }
  return groups;
}

export function Shell() {
  const { user, orgName, hasEngine, modelVersion, signOut } = useSession();
  const groups = sections(atLeast(user?.role, "admin"));
  const flat = groups.flatMap((g) => g.links);

  return (
    <div className="shell">
      <Palette role={user?.role} />
      <nav className="rail" aria-label="Sections">
        <div className="rail-mark">
          <Grain />
          <span>truegrain</span>
        </div>

        <div className="rail-nav">
          {groups.map((g) => (
            <div key={g.group}>
              <div className="rail-group">{g.group}</div>
              {g.links.map((l) => (
                <NavLink key={l.to} to={l.to} className="rail-link">
                  <l.icon />
                  {l.label}
                </NavLink>
              ))}
            </div>
          ))}
        </div>

        <div className="rail-foot">
          <span className="avatar" aria-hidden="true">
            {initials(user?.name || user?.email || "?")}
          </span>
          <NavLink
            to="/account"
            className="truncate"
            style={{ flex: 1, textDecoration: "none", color: "var(--ink)" }}
          >
            <div className="truncate small">{user?.name || user?.email}</div>
            <div className="faint" style={{ fontSize: "var(--t-10)" }}>
              {user?.role}
            </div>
          </NavLink>
          <ColorSchemeToggle />
          <Tooltip label="Sign out" position="top">
            <Button
              variant="subtle"
              color="gray"
              size="compact-xs"
              onClick={() => void signOut()}
              aria-label="Sign out"
            >
              Out
            </Button>
          </Tooltip>
        </div>
      </nav>

      <div className="main">
        {/* The same links for a narrow screen. The labels differ so a
            screen reader is not told about two landmarks with one name. */}
        <nav className="rail-strip" aria-label="Sections, compact">
          {flat.map((l) => (
            <NavLink key={l.to} to={l.to} className="rail-link">
              <l.icon />
              {l.label}
            </NavLink>
          ))}
          <NavLink to="/account" className="rail-link">
            <IconAccount />
            Account
          </NavLink>
          <span className="row" style={{ marginLeft: "auto", paddingRight: "var(--s2)" }}>
            <ColorSchemeToggle />
          </span>
        </nav>

        <header className="bar">
          <span className="fact">
            <span>Workspace</span>
            <b>{orgName ?? "truegrain"}</b>
          </span>
          <div className="bar-facts">
            <NamespacePicker />
            {modelVersion ? (
              <span className="fact" title="The content hash of the loaded model">
                <span>Model</span>
                <b>{modelVersion.slice(0, 12)}</b>
              </span>
            ) : null}
            {!hasEngine ? <span className="tag tag-refused">No model loaded</span> : null}
          </div>
        </header>

        {/*
          The one main landmark, and the one scroll container.

          Both belong here rather than in each page. As a landmark it
          lets a screen reader skip the rail and the bar in one jump;
          as the scroll container it needs tabindex so a keyboard-only
          reader can scroll a page that has nothing focusable in it,
          which is most of Deployment.
        */}
        <main className="canvas" tabIndex={0}>
          <Outlet />
        </main>
      </div>
    </div>
  );
}

function initials(who: string): string {
  const parts = who
    .trim()
    .split(/[\s@._-]+/)
    .filter(Boolean);
  const first = parts[0]?.[0] ?? "?";
  const second = parts.length > 1 ? (parts[1]?.[0] ?? "") : "";
  return (first + second).toUpperCase();
}
