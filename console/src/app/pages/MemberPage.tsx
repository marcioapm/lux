import { useState } from "react";
import { Avatar, Badge, Button, Card, CENTS, ConfirmDialog, Money, PageHeader, Person, RelativeTime, RoleBadge, ROLE_LABEL, StatTile, Table, useToast, type Column } from "@lux/design-system";
import { api, errorText, useQuery, type ApiKey, type Run, type ServiceAccount } from "../../api/index.ts";
import { atLeast, useViewer } from "../identity.ts";
import { go } from "../router.tsx";
import { ErrorBlock, PageSkeleton, runColumns, runPath } from "./common.tsx";
import { canManage, ChangeRoleDialog, RemoveMemberDialog, useCostByOwner, usePeopleTenant } from "./Members.tsx";
import { KeysTable, serviceAccountPath } from "./peopleParts.tsx";

/** /members/:id: one person's role, spend, service accounts, Runs and (to them and admins) keys. */
export function MemberPage({ id }: { id: string }) {
  const v = useViewer();
  const toast = useToast();
  const { tenant, name: tenantName } = usePeopleTenant();
  const m = useQuery(`member:${id}@${tenant ?? ""}`, (s) => api.member(tenant, id, s));
  const sas = useQuery(`sas@${tenant ?? ""}`, (s) => api.serviceAccounts(tenant, s));
  const cost = useCostByOwner("member");
  const self = id === v.userId;
  const seesKeys = self || atLeast(v, "admin");
  const keys = useQuery(`member:${id}:keys@${tenant ?? ""}`, (s) => api.memberKeys(tenant, id, s), { enabled: seesKeys });
  const runs = useQuery(`member:${id}:runs@${tenant ?? ""}`, (s) => api.runsPage(tenant, { owner: m.data?.email ?? id, limit: 50 }, s).then((p) => p.rows), { enabled: m.data != null });
  const [changing, setChanging] = useState(false);
  const [removing, setRemoving] = useState(false);
  const [revoking, setRevoking] = useState<ApiKey | null>(null);
  const [busy, setBusy] = useState(false);

  if (m.error && !m.data && !m.loading) return <div className="page"><ErrorBlock error={m.error} onRetry={m.refetch} /></div>;
  if (!m.data) return <PageSkeleton />;
  const p = m.data;
  const who = p.name || p.email;
  const first = who.split(" ")[0];
  const owned = (sas.data ?? []).filter((s) => s.owner?.id === p.id);
  const own = cost.byKey.get(`user:${p.id}`);
  const viaSa = owned.map((s) => cost.byKey.get(`sa:${s.id}`)).filter((r) => r != null);
  const viaSaRuns = viaSa.reduce((n, r) => n + (r.runs ?? 0), 0);
  const viaSaCost = viaSa.reduce((n, r) => n + Number(r.amount), 0);
  const manage = canManage(v, p);

  const act = async (title: string, fn: () => Promise<unknown>, after: () => void) => {
    setBusy(true);
    try {
      await fn();
      toast({ title, tone: "success" });
      after();
    } catch (e) {
      toast({ title: "Not done", description: errorText(e), tone: "danger" });
    } finally {
      setBusy(false);
    }
  };

  const saCols: Column<ServiceAccount>[] = [
    { key: "name", header: "Service account", cell: (s) => <Person name={s.name} sub={s.description} serviceAccount />, sortValue: (s) => s.name, label: "Service account" },
    { key: "role", header: "Role", cell: (s) => <RoleBadge role={s.role} />, width: 110 },
    {
      key: "cost",
      header: "Cost · 30d",
      cell: (s) => {
        const r = cost.byKey.get(`sa:${s.id}`);
        return r ? <Money amount={r.amount} currency={r.currency} decimals={CENTS} /> : "–";
      },
      align: "right",
      mono: true,
      width: 120,
    },
  ];

  return (
    <div className="page">
      <PageHeader
        title={
          <span className="profile-head">
            <Avatar name={who} picture={p.picture} size="lg" />
            <span>{who}</span>
          </span>
        }
        badges={
          <>
            <RoleBadge role={p.role} />
            {p.operator && <Badge tone="warn">Operator</Badge>}
          </>
        }
        description={
          <>
            <span>{p.email}</span>
            <span>
              member since <RelativeTime at={p.joinedAt} />
            </span>
            <span>
              last seen <RelativeTime at={p.lastSeenAt} />
            </span>
          </>
        }
        actions={
          manage ? (
            <>
              <Button onClick={() => setChanging(true)}>Change role</Button>
              <Button variant="danger" onClick={() => setRemoving(true)}>
                Remove
              </Button>
            </>
          ) : undefined
        }
      />
      <div className="stack">
        <div className="grid grid-stats">
          <StatTile label="Running now" value={String(p.runningRuns)} />
          <StatTile label="Runs · 30d" value={own?.runs != null ? String(own.runs) : "–"} unit={viaSaRuns ? `+ ${viaSaRuns} by ${owned.map((s) => s.name).join(", ")}` : undefined} loading={cost.loading} />
          <StatTile label="Cost · 30d" value={own ? <Money amount={own.amount} currency={own.currency} decimals={CENTS} /> : "–"} unit={viaSaCost ? <><Money amount={viaSaCost.toFixed(2)} currency={viaSa[0]!.currency} decimals={CENTS} /> more via service accounts</> : "list prices"} loading={cost.loading} />
          <StatTile label="Service accounts" value={String(owned.length)} unit="owned" loading={sas.loading} />
        </div>
        {owned.length > 0 && (
          <Card flush title="Service accounts owned" subtitle={`${first} answers for these: rotates their keys and acts on their Runs.`}>
            <Table columns={saCols} rows={owned} rowKey={(s) => s.id} onRowClick={(s) => go(serviceAccountPath(s.id))} defaultSort={{ key: "name", dir: "asc" }} />
          </Card>
        )}
        <Card flush title="Runs" subtitle={`owned by ${first} or ${self ? "your" : "their"} service accounts`}>
          {runs.error && !runs.data ? (
            <ErrorBlock error={runs.error} onRetry={runs.refetch} compact />
          ) : (
            <Table<Run> columns={runColumns({ tenant: false, owner: true, me: v.userId, adapter: false })} rows={runs.data ?? []} rowKey={(r) => r.id} loading={runs.loading || m.loading} onRowClick={(r) => go(runPath(r.id))} defaultSort={{ key: "created", dir: "desc" }} empty={`No Runs owned by ${first}.`} />
          )}
        </Card>
        {seesKeys && (
          <Card flush title="API keys" subtitle={self ? "yours: manage them on your profile" : `visible to ${first} and admins`}>
            <KeysTable keys={keys.data ?? []} loading={keys.loading} role={p.role} whose={self ? "my" : "their"} onRevoke={atLeast(v, "admin") && !self ? setRevoking : undefined} empty={`${first} has no API keys.`} />
          </Card>
        )}
      </div>
      <ChangeRoleDialog member={changing ? p : null} tenantName={tenantName} owner={atLeast(v, "owner")} busy={busy} onCancel={() => setChanging(false)} onConfirm={(x, role) => void act(`${who} is now ${ROLE_LABEL[role]}`, () => api.setMemberRole(tenant, x.id, role), () => (setChanging(false), void m.refetch()))} />
      <RemoveMemberDialog member={removing ? p : null} tenantName={tenantName} busy={busy} onCancel={() => setRemoving(false)} onConfirm={(x) => void act(`Removed ${who}`, () => api.removeMember(tenant, x.id), () => (setRemoving(false), go("/members")))} />
      {revoking && (
        <KeyRevokeConfirm name={revoking.name} busy={busy} onCancel={() => setRevoking(null)} onConfirm={() => void act(`Revoked ${revoking.name}`, () => api.revokeMemberKey(tenant, p.id, revoking.id), () => (setRevoking(null), void keys.refetch()))} />
      )}
    </div>
  );
}

function KeyRevokeConfirm({ name, busy, onConfirm, onCancel }: { name: string; busy: boolean; onConfirm: () => void; onCancel: () => void }) {
  return <ConfirmDialog open title={`Revoke ${name}?`} description="Whatever uses it stops working at once." confirmLabel="Revoke key" tone="danger" loading={busy} onConfirm={onConfirm} onCancel={onCancel} />;
}
