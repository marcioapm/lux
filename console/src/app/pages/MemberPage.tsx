import { useState } from "react";
import { Avatar, Badge, Button, Card, CENTS, ConfirmDialog, MoneyList, PageHeader, Person, RelativeTime, RoleBadge, ROLE_LABEL, StatTile, Table, type Column } from "@lux/design-system";
import { api, useQuery, type ApiKey, type Run, type ServiceAccount } from "../../api/index.ts";
import { atLeast, useViewer } from "../identity.ts";
import { go } from "../router.tsx";
import { ErrorBlock, PageSkeleton, QueryBody, runColumns, runPath, useAct } from "./common.tsx";
import { ChangeRoleDialog, lastOwnerReason, memberActions, refreshWhoAmI, RemoveMemberDialog, useCostByOwner, usePeopleTenant } from "./Members.tsx";
import { KeysTable, serviceAccountPath } from "./peopleParts.tsx";

/** /members/:id: one person's role, spend, service accounts, Runs and (to them and admins) keys. */
export function MemberPage({ id }: { id: string }) {
  const v = useViewer();
  const { tenant, name: tenantName } = usePeopleTenant();
  const m = useQuery(`member:${id}@${tenant ?? ""}`, (s) => api.member(tenant, id, s));
  const all = useQuery(`members@${tenant ?? ""}`, (s) => api.members(tenant, s), { enabled: atLeast(v, "admin") });
  const sas = useQuery(`sas@${tenant ?? ""}`, (s) => api.serviceAccounts(tenant, s));
  const cost = useCostByOwner("member");
  const self = id === v.userId;
  const seesKeys = self || atLeast(v, "admin");
  const keys = useQuery(`member:${id}:keys@${tenant ?? ""}`, (s) => api.memberKeys(tenant, id, s), { enabled: seesKeys });
  // Theirs and their service accounts': GET /v1/runs?owner= takes several owners (user:<id>, sa:<id>).
  const ownedIds = (sas.data ?? []).filter((s) => s.owner?.id === id || s.owner?.email === id).map((s) => `sa:${s.id}`);
  const ownerParam = m.data ? [`user:${m.data.id}`, ...ownedIds].join(",") : "";
  const runs = useQuery(`member:${id}:runs@${tenant ?? ""}:${ownerParam}`, (s) => api.runsPage(tenant, { owner: ownerParam, limit: 50 }, s).then((p) => p.rows), { enabled: m.data != null && sas.data != null });
  const [changing, setChanging] = useState(false);
  const [removing, setRemoving] = useState(false);
  const [revoking, setRevoking] = useState<ApiKey | null>(null);
  const { busy, act } = useAct();

  if (m.error && !m.data && !m.loading) return <div className="page"><ErrorBlock error={m.error} onRetry={m.refetch} /></div>;
  if (!m.data) return <PageSkeleton />;
  const p = m.data;
  const who = p.name || p.email;
  const first = who.split(" ")[0];
  const owned = (sas.data ?? []).filter((s) => s.owner?.id === p.id);
  // Per currency, never added across them: theirs, and their accounts' together.
  const own = cost.cost(`user:${p.id}`);
  const viaSa = cost.cost(...owned.map((s) => `sa:${s.id}`));
  const viaSaRuns = viaSa.runs ?? 0;
  const owners = (all.data ?? []).filter((x) => x.role === "owner").length || 1;
  const actions = memberActions(v, p, owners);
  // The last-owner rule: only an operator demotes or removes the only owner.
  const lastOwner = lastOwnerReason(v, p, owners);

  const saCols: Column<ServiceAccount>[] = [
    { key: "name", header: "Service account", cell: (s) => <Person name={s.name} sub={s.description} serviceAccount />, sortValue: (s) => s.name, label: "Service account" },
    { key: "role", header: "Role", cell: (s) => <RoleBadge role={s.role} />, width: 110 },
    {
      key: "cost",
      header: "Cost · 30d",
      cell: (s) => <MoneyList amounts={cost.cost(`sa:${s.id}`).amounts} decimals={CENTS} />,
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
          actions.change || actions.remove ? (
            <>
              {actions.change && (
                <Button disabled={actions.changeReason != null || (self && !all.data)} title={actions.changeReason ?? undefined} onClick={() => setChanging(true)}>
                  Change role
                </Button>
              )}
              {actions.remove && (
                <Button variant="danger" disabled={actions.removeReason != null} title={actions.removeReason ?? undefined} onClick={() => setRemoving(true)}>
                  {self ? "Leave tenant" : "Remove"}
                </Button>
              )}
            </>
          ) : undefined
        }
      />
      <div className="stack">
        <div className="grid grid-stats">
          <StatTile label="Running now" value={String(p.runningRuns)} />
          <StatTile label="Runs · 30d" value={own.runs != null ? String(own.runs) : "–"} unit={viaSaRuns ? `+ ${viaSaRuns} by ${owned.map((s) => s.name).join(", ")}` : undefined} loading={cost.loading} />
          <StatTile label="Cost · 30d" value={own.amounts.length ? <MoneyList amounts={own.amounts} decimals={CENTS} /> : "–"} unit={viaSa.amounts.length ? <><MoneyList amounts={viaSa.amounts} decimals={CENTS} /> more via service accounts</> : "list prices"} loading={cost.loading} />
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
            <QueryBody q={keys} compact>
              <KeysTable keys={keys.data ?? []} loading={keys.loading} role={p.role} whose={self ? "my" : "their"} onRevoke={canRevokeKeys(v, p, self) ? setRevoking : undefined} empty={`${first} has no API keys.`} />
            </QueryBody>
          </Card>
        )}
      </div>
      <ChangeRoleDialog member={changing ? p : null} self={self} lastOwner={lastOwner} tenantName={tenantName} owner={atLeast(v, "owner")} busy={busy} onCancel={() => setChanging(false)} onConfirm={(x, role) => void act(self ? `You are now ${ROLE_LABEL[role]}` : `${who} is now ${ROLE_LABEL[role]}`, () => api.setMemberRole(tenant, x.id, role), () => (setChanging(false), self ? refreshWhoAmI(tenant) : undefined, void m.refetch(), void all.refetch()))} />
      <RemoveMemberDialog member={removing ? p : null} self={self} tenantName={tenantName} busy={busy} onCancel={() => setRemoving(false)} onConfirm={(x) => void act(self ? `You left ${tenantName}` : `Removed ${who}`, () => api.removeMember(tenant, x.id), () => (setRemoving(false), self ? refreshWhoAmI(tenant) : undefined, go("/members")))} />
      {revoking && (
        <KeyRevokeConfirm name={revoking.name} busy={busy} onCancel={() => setRevoking(null)} onConfirm={() => void act(`Revoked ${revoking.name}`, () => api.revokeMemberKey(tenant, p.id, revoking.id), () => (setRevoking(null), void keys.refetch()))} />
      )}
    </div>
  );
}

/** DELETE /v1/members/{user}/keys/{key}: admins+; an owner's keys only owners and operators. Your own: on your profile. */
function canRevokeKeys(v: ReturnType<typeof useViewer>, m: { role: string }, self: boolean): boolean {
  // atLeast is false above viewer with a read-only key: it revokes nothing.
  return !self && atLeast(v, "admin") && (m.role !== "owner" || atLeast(v, "owner"));
}

function KeyRevokeConfirm({ name, busy, onConfirm, onCancel }: { name: string; busy: boolean; onConfirm: () => void; onCancel: () => void }) {
  return <ConfirmDialog open title={`Revoke ${name}?`} description="Whatever uses it stops working at once." confirmLabel="Revoke key" tone="danger" loading={busy} onConfirm={onConfirm} onCancel={onCancel} />;
}
