import { useMemo } from "react";
import { Badge, Card, Table, type Column } from "../../ds/index.ts";
import { api, useQuery, useSession, type Pool } from "../../api/index.ts";
import { useScope } from "../scope.tsx";
import { DASH, ErrorBlock, ErrorStrip } from "./common.tsx";

interface PoolRow extends Pool {
  key: string;
  hostCount: number;
  readyCount: number;
}

export function Pools() {
  const scope = useScope();
  const session = useSession();
  const pools = useQuery(`pools:${scope.tenant}`, (s) => api.pools(scope.apiTenant, s), { interval: 15_000 });
  const hosts = useQuery(`hosts:${scope.tenant}:all`, (s) => api.hosts(scope.apiTenant, {}, s), { interval: 15_000 });

  const rows = useMemo<PoolRow[]>(() => {
    const counts = new Map<string, { n: number; ready: number }>();
    for (const h of hosts.data ?? []) {
      // Host rows carry the tenant name (empty for platform), like pools do.
      const k = `${h.platform ? "" : h.tenant ?? ""}/${h.pool}`;
      const c = counts.get(k) ?? { n: 0, ready: 0 };
      c.n++;
      if (h.state === "ready") c.ready++;
      counts.set(k, c);
    }
    return (pools.data ?? []).map((p) => {
      const k = `${p.platform ? "" : p.tenant ?? ""}/${p.name}`;
      const c = counts.get(k);
      return { ...p, key: k, hostCount: c?.n ?? 0, readyCount: c?.ready ?? 0 };
    });
  }, [pools.data, hosts.data]);

  const showTenant = session.role === "operator";
  const cols = useMemo<Column<PoolRow>[]>(() => {
    const c: Column<PoolRow>[] = [{ key: "name", header: "Pool", cell: (p) => <span className="mono">{p.name}</span>, sortValue: (p) => p.name, width: 180 }];
    if (showTenant) c.push({ key: "tenant", header: "Tenant", cell: (p) => (p.platform ? <Badge outline>platform</Badge> : p.tenant || DASH), sortValue: (p) => (p.platform ? "" : p.tenant), width: 120 });
    c.push(
      { key: "provider", header: "Provider", cell: (p) => <Badge mono outline>{p.provider}</Badge>, sortValue: (p) => p.provider, width: 100 },
      { key: "hosts", header: "Hosts", cell: (p) => `${p.readyCount} ready / ${p.hostCount}`, sortValue: (p) => p.hostCount, align: "right", mono: true, width: 130 },
      { key: "min", header: "Min", cell: (p) => p.minHosts, sortValue: (p) => p.minHosts, align: "right", mono: true, width: 60 },
      { key: "warm", header: "Warm", cell: (p) => p.warmHosts, sortValue: (p) => p.warmHosts, align: "right", mono: true, width: 60 },
      { key: "max", header: "Max", cell: (p) => p.maxHosts, sortValue: (p) => p.maxHosts, align: "right", mono: true, width: 60 },
      { key: "shared", header: "Shared", cell: (p) => (p.shared ? <Badge tone="info">shared</Badge> : DASH), sortValue: (p) => (p.shared ? 1 : 0), width: 90 },
      { key: "template", header: "Template", cell: (p) => (p.template && Object.keys(p.template).length > 0 ? <span className="mono muted ellipsis">{templateText(p.template)}</span> : DASH), nowrap: true },
    );
    return c;
  }, [showTenant]);

  return (
    <div className="page">
      <Card flush title="Pools" subtitle="host counts from the hosts list (terminated excluded)">
        <ErrorStrip error={rows.length > 0 ? pools.error ?? hosts.error : hosts.error} />
        {pools.error && rows.length === 0 && !pools.loading ? (
          <ErrorBlock error={pools.error} onRetry={pools.refetch} />
        ) : (
          <Table columns={cols} rows={rows} rowKey={(p) => p.key} loading={pools.loading} defaultSort={{ key: "name", dir: "asc" }} empty="No pools." />
        )}
      </Card>
    </div>
  );
}

function templateText(t: Record<string, unknown>): string {
  return Object.entries(t)
    .map(([k, v]) => `${k}=${typeof v === "string" ? v : JSON.stringify(v)}`)
    .join(" ");
}
