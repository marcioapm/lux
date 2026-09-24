import type { ReactNode } from "react";
import { IconButton, TenantPicker, TimeRangePicker, useTheme, type Tenant } from "../ds/index.ts";
import { IconGrid, IconLayers, IconLogout, IconMoon, IconPalette, IconPlay, IconServer, IconSun, IconUsers } from "../ds/icons.tsx";
import { signOut } from "../api/index.ts";
import { Link, usePath } from "./router.tsx";
import { useScope } from "./scope.tsx";

interface NavItem {
  to: string;
  label: string;
  icon: ReactNode;
  match: (p: string) => boolean;
  operator?: boolean;
}

const NAV: NavItem[] = [
  { to: "/", label: "Overview", icon: <IconGrid />, match: (p) => p === "/" },
  { to: "/runs", label: "Runs", icon: <IconPlay />, match: (p) => p.startsWith("/runs") },
  { to: "/hosts", label: "Hosts", icon: <IconServer />, match: (p) => p.startsWith("/hosts") },
  { to: "/pools", label: "Pools", icon: <IconLayers />, match: (p) => p.startsWith("/pools") },
  { to: "/tenants", label: "Tenants", icon: <IconUsers />, match: (p) => p.startsWith("/tenants"), operator: true },
];

export interface ShellProps {
  tenants: Tenant[];
  /** Operator key: show the tenant picker and the Tenants page. */
  operator: boolean;
  /** Page title shown in the top bar. */
  title?: ReactNode;
  children: ReactNode;
}

export function Shell({ tenants, operator, title, children }: ShellProps) {
  const path = usePath();
  const scope = useScope();
  const { resolved, toggle } = useTheme();
  return (
    <div className="shell">
      <aside className="sidebar">
        <Link to="/" className="brand">
          <span className="brand-mark" aria-hidden="true" />
          <span className="brand-name">lux</span>
          <span className="brand-sub">console</span>
        </Link>
        <nav className="nav" aria-label="Primary">
          {NAV.filter((n) => !n.operator || operator).map((n) => (
            <Link key={n.to} to={n.to} className={n.match(path) ? "nav-item is-active" : "nav-item"} aria-current={n.match(path) ? "page" : undefined}>
              <span className="nav-icon">{n.icon}</span>
              {n.label}
            </Link>
          ))}
        </nav>
        <div className="sidebar-foot">
          <Link to="/styleguide" className={path === "/styleguide" ? "nav-item is-active" : "nav-item"}>
            <span className="nav-icon">
              <IconPalette />
            </span>
            Style guide
          </Link>
        </div>
      </aside>
      <div className="main">
        <header className="topbar">
          <h1 className="topbar-title">{title}</h1>
          <div className="topbar-controls">
            {operator && <TenantPicker tenants={tenants} value={scope.tenant} onChange={scope.setTenant} />}
            <TimeRangePicker value={scope.range} onChange={scope.setRange} />
            <IconButton size="sm" label={resolved === "dark" ? "Switch to light theme" : "Switch to dark theme"} onClick={toggle}>
              {resolved === "dark" ? <IconSun size={15} /> : <IconMoon size={15} />}
            </IconButton>
            <IconButton size="sm" label="Sign out" onClick={signOut}>
              <IconLogout size={15} />
            </IconButton>
          </div>
        </header>
        <main className="content">{children}</main>
      </div>
    </div>
  );
}
