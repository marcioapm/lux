import { useMemo, useState } from "react";
import { Badge, Button, Dialog, InviteLinkList, Card, formatBytes, formatCount, formatDuration, IdChip, PageHeader, Table, type Column, RelativeTime } from "@lux/design-system";
import { api, errorText, useQuery, type Invitation, type Tenant } from "../../api/index.ts";
import { go } from "../router.tsx";
import { useViewer } from "../identity.ts";
import { useScope } from "../scope.tsx";
import { ErrorBlock, ErrorStrip, UsageBar } from "./common.tsx";

// expiry: how long a stopped, lost, failed or succeeded Run of the tenant may rest
// before it is terminated; 0 is never.
export function expiry(days: number): string {
  return days === 0 ? "never" : formatDuration(days * 86_400);
}

export function Tenants() {
  const scope = useScope();
  const viewer = useViewer();
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [link, setLink] = useState<Invitation | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const q = useQuery("tenants:page", (s) => api.tenants(s), { interval: 15_000 });

  const cols = useMemo<Column<Tenant>[]>(
    () => [
      { key: "name", header: "Tenant", cell: (t) => t.name, sortValue: (t) => t.name, lead: true, width: 180 },
      { key: "owners", header: "Owners", cell: (t) => t.owners == null ? "–" : t.owners.length ? t.owners.map((p) => p.name || p.email).join(", ") : <Badge tone="warn">No owner</Badge>, sortValue: (t) => t.owners?.length, width: 220 },
      { key: "members", header: "Members", cell: (t) => t.members ?? "–", sortValue: (t) => t.members, align: "right", width: 90 },
      { key: "id", header: "Id", cell: (t) => <IdChip value={t.id} truncate={18} />, sortValue: (t) => t.id, mono: true, width: 200, optional: true },
      { key: "active", header: "Active runs", cell: (t) => (t.maxConcurrentRuns != null ? `${t.activeRuns} / ${t.maxConcurrentRuns}` : String(t.activeRuns)), sortValue: (t) => t.activeRuns, align: "right", mono: true, width: 120 },
      { key: "runs", header: "Total runs", cell: (t) => formatCount(t.runs), sortValue: (t) => t.runs, align: "right", mono: true, width: 110 },
      { key: "hosts", header: "Hosts", cell: (t) => (t.maxHosts != null ? `${t.hosts} / ${t.maxHosts}` : String(t.hosts)), sortValue: (t) => t.hosts, align: "right", mono: true, width: 100 },
      { key: "stored", header: "Stored", cell: (t) => (t.maxStorageBytes != null ? <UsageBar used={t.storedBytes} total={t.maxStorageBytes} unit="bytes" /> : <span className="num">{formatBytes(t.storedBytes)}</span>), sortValue: (t) => t.storedBytes },
      { key: "retention", header: "Retention", cell: (t) => `${t.retentionDays}d`, sortValue: (t) => t.retentionDays, align: "right", mono: true, width: 100, optional: true },
      { key: "expiry", header: "Expiry", cell: (t) => expiry(t.expireAfterDays), sortValue: (t) => t.expireAfterDays || Infinity, align: "right", mono: true, width: 100, optional: true },
      { key: "created", header: "Created", cell: (t) => <RelativeTime at={t.createdAt} />, sortValue: (t) => Date.parse(t.createdAt), align: "right", width: 104 },
    ],
    [],
  );

  const open = (t: Tenant) => {
    scope.setTenant(t.id);
    go("/");
  };

  return (
    <div className="page page-list">
      <PageHeader title="Tenants" description={`${q.data?.length ?? 0} tenants · click one to scope the console to it`} actions={<Button variant="primary" onClick={() => setCreating(true)}>New tenant</Button>} />
      <Card flush>
        <ErrorStrip error={(q.data?.length ?? 0) > 0 ? q.error : null} />
        {q.error && !q.data && !q.loading ? (
          <ErrorBlock error={q.error} onRetry={q.refetch} />
        ) : (
          <Table columns={cols} rows={q.data ?? []} rowKey={(t) => t.id} loading={q.loading} defaultSort={{ key: "name", dir: "asc" }} onRowClick={open} selected={scope.tenant} empty="No tenants." />
        )}
      </Card>
      <Dialog open={creating} title="New tenant" description="Creates the tenant and, optionally, an Owner invitation for its first owner." confirmLabel={viewer.invitationsEnabled && email.trim() ? "Create and get link" : "Create tenant"} disabled={!name.trim()} loading={busy} onCancel={() => setCreating(false)} onConfirm={() => { setBusy(true); setError(null); void api.createTenant({ name: name.trim(), ...(viewer.invitationsEnabled && email.trim() ? { ownerEmail: email.trim() } : {}) }).then(async (r) => { setCreating(false); setLink(r.invitation ?? null); setName(""); setEmail(""); await q.refetch(); }).catch((e) => setError(errorText(e))).finally(() => setBusy(false)); }}>
        <label className="field"><span className="field-label">Name</span><input className="input" value={name} onChange={(e) => setName(e.target.value)} autoFocus /></label>
        {viewer.invitationsEnabled && <label className="field"><span className="field-label">First owner's email</span><input className="input" type="email" value={email} onChange={(e) => setEmail(e.target.value)} /><span className="field-hint">Optional. Empty: the tenant starts with no owner, and you invite one later.</span></label>}
        {error && <div className="error-strip" role="alert">{error}</div>}
      </Dialog>
      <Dialog open={link != null} title="Send this Owner invitation" confirmLabel="Done" onConfirm={() => setLink(null)} onCancel={() => setLink(null)}>{link && <InviteLinkList links={[{ email: link.email, url: link.url }]} />}</Dialog>
    </div>
  );
}
