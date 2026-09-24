import { useEffect, useMemo } from "react";
import { ToastProvider, type Tenant as PickerTenant } from "../ds/index.ts";
import { api, isApiError, setRole, useQuery, useSession } from "../api/index.ts";
import { matchPath, usePath } from "./router.tsx";
import { ScopeProvider } from "./scope.tsx";
import { Shell } from "./Shell.tsx";
import { SignIn } from "./SignIn.tsx";
import { StyleGuide } from "../styleguide/StyleGuide.tsx";
import { Overview } from "./pages/Overview.tsx";
import { Runs } from "./pages/Runs.tsx";
import { RunPage } from "./pages/RunPage.tsx";
import { Hosts } from "./pages/Hosts.tsx";
import { HostPage } from "./pages/HostPage.tsx";
import { Pools } from "./pages/Pools.tsx";
import { Tenants } from "./pages/Tenants.tsx";
import { NotFound } from "./pages/NotFound.tsx";
import { ErrorBlock } from "./pages/common.tsx";

interface Route {
  pattern: string;
  title: string;
  render: (params: Record<string, string>) => React.ReactNode;
}

const ROUTES: Route[] = [
  { pattern: "/", title: "Overview", render: () => <Overview /> },
  { pattern: "/runs", title: "Runs", render: () => <Runs /> },
  { pattern: "/runs/:id", title: "Run", render: (p) => <RunPage id={p.id!} /> },
  { pattern: "/hosts", title: "Hosts", render: () => <Hosts /> },
  { pattern: "/hosts/:id", title: "Host", render: (p) => <HostPage id={p.id!} /> },
  { pattern: "/pools", title: "Pools", render: () => <Pools /> },
  { pattern: "/tenants", title: "Tenants", render: () => <Tenants /> },
  { pattern: "/styleguide", title: "Style guide", render: () => <StyleGuide /> },
];

function Router() {
  const path = usePath();
  const session = useSession();
  // The tenant list doubles as the role probe: 403 means a tenant key.
  const tenants = useQuery("tenants", (signal) => api.tenants(signal).catch((e: unknown) => {
    if (isApiError(e) && e.status === 403) return null;
    throw e;
  }), { interval: session.role === "unknown" ? 5000 : 60_000 });
  useEffect(() => {
    if (tenants.data === undefined) return;
    setRole(tenants.data === null ? "tenant" : "operator");
  }, [tenants.data]);

  const picker = useMemo<PickerTenant[]>(
    () => (tenants.data ?? []).map((t) => ({ id: t.id, name: t.name, hint: t.activeRuns > 0 ? `${t.activeRuns} active` : undefined })),
    [tenants.data],
  );
  const operator = session.role === "operator";

  // Until the probe answers, the role is unknown: do not guess "tenant".
  if (session.role === "unknown" && tenants.error && path !== "/styleguide") {
    return (
      <Shell tenants={[]} operator={false} title="Cannot reach luxd">
        <div className="page">
          <ErrorBlock error={`Could not check this key's access: ${tenants.error}. Retrying every few seconds.`} onRetry={tenants.refetch} />
        </div>
      </Shell>
    );
  }

  for (const r of ROUTES) {
    const m = matchPath(r.pattern, path);
    if (!m) continue;
    if (r.pattern === "/tenants" && session.role === "tenant") break;
    return (
      <Shell tenants={picker} operator={operator} title={r.title}>
        {r.render(m.params)}
      </Shell>
    );
  }
  return (
    <Shell tenants={picker} operator={operator} title="Not found">
      <NotFound path={path} />
    </Shell>
  );
}

export function App() {
  const session = useSession();
  return (
    <ScopeProvider>
      <ToastProvider>{session.key ? <Router key={session.key} /> : <SignIn />}</ToastProvider>
    </ScopeProvider>
  );
}
