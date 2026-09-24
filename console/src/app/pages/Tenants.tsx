import { useMemo } from "react";
import { Card, formatBytes, formatCount, IdChip, Table, type Column } from "../../ds/index.ts";
import { api, useQuery, type Tenant } from "../../api/index.ts";
import { go } from "../router.tsx";
import { useScope } from "../scope.tsx";
import { ErrorBlock, ErrorStrip, RelativeTime, UsageBar } from "./common.tsx";

export function Tenants() {
  const scope = useScope();
  const q = useQuery("tenants:page", (s) => api.tenants(s), { interval: 15_000 });

  const cols = useMemo<Column<Tenant>[]>(
    () => [
      { key: "name", header: "Tenant", cell: (t) => t.name, sortValue: (t) => t.name, width: 160, nowrap: true },
      { key: "id", header: "Id", cell: (t) => <IdChip value={t.id} truncate={18} />, sortValue: (t) => t.id, mono: true, width: 200 },
      { key: "active", header: "Active runs", cell: (t) => (t.maxConcurrentRuns != null ? `${t.activeRuns} / ${t.maxConcurrentRuns}` : String(t.activeRuns)), sortValue: (t) => t.activeRuns, align: "right", mono: true, width: 110 },
      { key: "runs", header: "Total runs", cell: (t) => formatCount(t.runs), sortValue: (t) => t.runs, align: "right", mono: true, width: 100 },
      { key: "hosts", header: "Hosts", cell: (t) => (t.maxHosts != null ? `${t.hosts} / ${t.maxHosts}` : String(t.hosts)), sortValue: (t) => t.hosts, align: "right", mono: true, width: 90 },
      { key: "stored", header: "Stored", cell: (t) => (t.maxStorageBytes != null ? <UsageBar used={t.storedBytes} total={t.maxStorageBytes} unit="bytes" width={200} /> : <span className="mono">{formatBytes(t.storedBytes)}</span>), sortValue: (t) => t.storedBytes, width: 220 },
      { key: "retention", header: "Retention", cell: (t) => `${t.retentionDays}d`, sortValue: (t) => t.retentionDays, align: "right", mono: true, width: 90 },
      { key: "created", header: "Created", cell: (t) => <RelativeTime at={t.createdAt} />, sortValue: (t) => Date.parse(t.createdAt), align: "right", width: 96 },
    ],
    [],
  );

  const open = (t: Tenant) => {
    scope.setTenant(t.id);
    go("/");
  };

  return (
    <div className="page">
      <Card flush title="Tenants" subtitle="click a tenant to scope the console to it">
        <ErrorStrip error={(q.data?.length ?? 0) > 0 ? q.error : null} />
        {q.error && !q.data && !q.loading ? (
          <ErrorBlock error={q.error} onRetry={q.refetch} />
        ) : (
          <Table columns={cols} rows={q.data ?? []} rowKey={(t) => t.id} loading={q.loading} defaultSort={{ key: "name", dir: "asc" }} onRowClick={open} selected={scope.tenant} empty="No tenants." />
        )}
      </Card>
    </div>
  );
}
