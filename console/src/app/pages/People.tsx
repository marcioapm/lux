import { Badge, Card, PageHeader, Person, RelativeTime, ROLE_LABEL, Table, type Column } from "@lux/design-system";
import { api, useQuery, type PlatformPerson } from "../../api/index.ts";
import { ErrorBlock } from "./common.tsx";

export function People() {
  const q = useQuery("people", (s) => api.people(s));
  const cols: Column<PlatformPerson>[] = [
    { key: "name", header: "Person", cell: (p) => <Person name={p.name || p.email} sub={p.email} picture={p.picture} />, sortValue: (p) => p.name || p.email },
    { key: "tenants", header: "Tenants", cell: (p) => p.memberships.map((m) => `${m.tenant} · ${ROLE_LABEL[m.role]}`).join(", "), sortValue: (p) => p.memberships.length },
    { key: "operator", header: "Platform", cell: (p) => p.operator ? <Badge tone="warn">Operator</Badge> : "–", width: 150 },
    { key: "keys", header: "Keys", cell: (p) => p.keyCount, sortValue: (p) => p.keyCount, align: "right", mono: true, width: 80 },
    { key: "seen", header: "Last seen", cell: (p) => <RelativeTime at={p.lastSeenAt} />, sortValue: (p) => p.lastSeenAt, width: 110 },
  ];
  return <div className="page page-list"><PageHeader title="People" description="Everyone with a lux account, across tenants · operators only" /><Card flush>{q.error ? <ErrorBlock error={q.error} onRetry={q.refetch} /> : <Table columns={cols} rows={q.data ?? []} rowKey={(p) => p.id} loading={q.loading} defaultSort={{ key: "name", dir: "asc" }} />}</Card><p className="page-note">Operators are the emails in console.cloudflare_access.operators. luxd keeps their flag in step with the configured list at startup; their memberships stay if removed.</p></div>;
}
