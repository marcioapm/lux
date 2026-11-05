import { useState } from "react";
import { Avatar, Badge, Button, Card, CENTS, ConfirmDialog, Dialog, IconButton, IdChip, KeyValue, Money, PageHeader, Person, RelativeTime, RoleBadge, ROLE_DESC, ROLE_LABEL, Select, StatTile, Table, useToast, type Column, type TenantRole } from "@lux/design-system";
import { IconPencil, IconPlus, IconStop } from "@lux/design-system/icons";
import { api, errorText, useQuery, type ApiKey, type CreatedApiKey, type Member, type Run, type ServiceAccount } from "../../api/index.ts";
import { atLeast, useViewer } from "../identity.ts";
import { go } from "../router.tsx";
import { ErrorBlock, ErrorStrip, PageSkeleton, runColumns, runPath } from "./common.tsx";
import { useCostByOwner, usePeopleTenant } from "./Members.tsx";
import { KeyRevealDialog, KeysTable, NewKeyDialog, PersonInline, RoleField, serviceAccountPath } from "./peopleParts.tsx";

/** At most this many active keys per service account (luxd: 409 key_limit), so one can rotate without downtime. */
export const SA_KEY_CAP = 2;

const ROLE_WHAT: Record<string, string> = { viewer: "reads only", member: "starts Runs, manages its own", admin: "manages the tenant's Runs and settings" };

/** /service-accounts: the tenant's machine principals. */
export function ServiceAccounts() {
  const v = useViewer();
  const toast = useToast();
  const { tenant, name } = usePeopleTenant();
  const q = useQuery(`sas@${tenant ?? ""}`, (s) => api.serviceAccounts(tenant, s), { interval: 30_000 });
  const members = useQuery(`members@${tenant ?? ""}`, (s) => api.members(tenant, s));
  const cost = useCostByOwner("member");
  const [editing, setEditing] = useState<ServiceAccount | "new" | null>(null);
  const [disabling, setDisabling] = useState<ServiceAccount | null>(null);
  const [created, setCreated] = useState<CreatedApiKey | null>(null);
  const [busy, setBusy] = useState(false);
  const admin = atLeast(v, "admin");

  const cols: Column<ServiceAccount>[] = [
    { key: "name", header: "Service account", label: "Service account", cell: (s) => <Person name={s.name} sub={s.disabledAt ? "disabled" : s.description} serviceAccount />, sortValue: (s) => s.name },
    { key: "owner", header: "Owner", cell: (s) => (s.owner ? <PersonInline person={s.owner} /> : <span className="muted">no owner</span>), sortValue: (s) => s.owner?.name, width: 170 },
    { key: "role", header: "Role", cell: (s) => <RoleBadge role={s.role} />, sortValue: (s) => ["viewer", "member", "admin"].indexOf(s.role), width: 100 },
    { key: "keys", header: "Keys", cell: (s) => `${s.activeKeys} / ${SA_KEY_CAP}`, sortValue: (s) => s.activeKeys, align: "right", mono: true, width: 70 },
    { key: "runs", header: "Runs · 30d", cell: (s) => cost.byKey.get(`sa:${s.id}`)?.runs?.toLocaleString("en-US") ?? "–", sortValue: (s) => cost.byKey.get(`sa:${s.id}`)?.runs ?? null, align: "right", mono: true, width: 104 },
    {
      key: "cost",
      header: "Cost · 30d",
      cell: (s) => {
        const r = cost.byKey.get(`sa:${s.id}`);
        return r ? <Money amount={r.amount} currency={r.currency} decimals={CENTS} /> : "–";
      },
      sortValue: (s) => (cost.byKey.has(`sa:${s.id}`) ? Number(cost.byKey.get(`sa:${s.id}`)!.amount) : null),
      align: "right",
      mono: true,
      width: 110,
    },
    { key: "used", header: "Last used", cell: (s) => <RelativeTime at={s.lastUsedAt} label="Last used" />, sortValue: (s) => (s.lastUsedAt ? Date.parse(s.lastUsedAt) : null), sortKind: "time", align: "right", width: 104 },
  ];
  if (admin)
    cols.push({
      key: "actions",
      header: "",
      label: "Actions",
      cell: (s) => (
        <span className="row-actions">
          <IconButton size="sm" label={`Edit ${s.name}`} onClick={(e) => (e.stopPropagation(), setEditing(s))} onKeyDown={(e) => e.stopPropagation()}>
            <IconPencil size={14} />
          </IconButton>
          {!s.disabledAt && (
            <IconButton size="sm" label={`Disable ${s.name}`} onClick={(e) => (e.stopPropagation(), setDisabling(s))} onKeyDown={(e) => e.stopPropagation()}>
              <IconStop size={14} />
            </IconButton>
          )}
        </span>
      ),
      align: "right",
      width: 84,
    });

  const disable = async (s: ServiceAccount) => {
    setBusy(true);
    try {
      await api.updateServiceAccount(tenant, s.id, { disabled: true });
      toast({ title: `Disabled ${s.name}`, tone: "success" });
      setDisabling(null);
      await q.refetch();
    } catch (e) {
      toast({ title: "Not done", description: errorText(e), tone: "danger" });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="page page-list">
      <PageHeader
        title="Service accounts"
        description={`${name} · machines that call lux: CI, bots, dashboards. Each has an owner, a role and up to ${SA_KEY_CAP} keys.`}
        actions={
          admin ? (
            <Button variant="primary" icon={<IconPlus size={14} />} onClick={() => setEditing("new")}>
              New service account
            </Button>
          ) : undefined
        }
      />
      <Card flush>
        <ErrorStrip error={q.data ? q.error : null} />
        {q.error && !q.data && !q.loading ? <ErrorBlock error={q.error} onRetry={q.refetch} /> : <Table columns={cols} rows={q.data ?? []} rowKey={(s) => s.id} loading={q.loading} onRowClick={(s) => go(serviceAccountPath(s.id))} defaultSort={{ key: "name", dir: "asc" }} empty="No service accounts." minWidth={880} />}
      </Card>
      {!admin && <p className="page-note">Only admins and owners create, edit or disable service accounts. An account's owner rotates its keys.</p>}
      <ServiceAccountDialog
        target={editing}
        tenant={tenant}
        tenantName={name}
        members={members.data ?? []}
        onCancel={() => setEditing(null)}
        onSaved={async (s, key) => {
          setEditing(null);
          await q.refetch();
          if (key) setCreated(key);
          else toast({ title: `Saved ${s.name}`, tone: "success" });
        }}
      />
      <KeyRevealDialog created={created} onDone={() => setCreated(null)} />
      <ConfirmDialog
        open={disabling != null}
        title={`Disable ${disabling?.name ?? ""}?`}
        description="Its keys stop working at once; its running Runs keep running. An admin can enable it again."
        confirmLabel="Disable"
        tone="danger"
        loading={busy}
        onConfirm={() => disabling && void disable(disabling)}
        onCancel={() => setDisabling(null)}
      />
    </div>
  );
}

/** New (name, description, owner, role; creates its first key) or Edit (description, owner, role). */
export function ServiceAccountDialog({ target, tenant, tenantName, members, onSaved, onCancel }: { target: ServiceAccount | "new" | null; tenant: string | undefined; tenantName: string; members: Member[]; onSaved: (s: ServiceAccount, key?: CreatedApiKey) => void | Promise<void>; onCancel: () => void }) {
  const isNew = target === "new";
  const sa = target && target !== "new" ? target : null;
  // The owner by email: the server takes ownerEmail ("" on a patch: no owner).
  const [form, setForm] = useState<{ for: string; name: string; description: string; ownerEmail: string; role: Exclude<TenantRole, "owner"> } | null>(null);
  const key = isNew ? "new" : (sa?.id ?? "");
  const f = form?.for === key ? form : { for: key, name: "", description: sa?.description ?? "", ownerEmail: sa?.owner?.email ?? "", role: sa?.role ?? "member" };
  const set = (p: Partial<typeof f>) => setForm({ ...f, ...p });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const valid = !isNew || /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/.test(f.name);
  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      if (isNew) {
        const s = await api.createServiceAccount(tenant, { name: f.name, description: f.description || undefined, role: f.role, ownerEmail: f.ownerEmail || undefined });
        const k = await api.createServiceAccountKey(tenant, s.id, { name: `${s.name}-${new Date().toISOString().slice(0, 7)}` });
        setForm(null);
        await onSaved(s, k);
      } else if (sa) {
        const s = await api.updateServiceAccount(tenant, sa.id, { description: f.description, role: f.role, ownerEmail: f.ownerEmail });
        setForm(null);
        await onSaved(s);
      }
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(false);
    }
  };
  const ownerOptions = [{ value: "", label: <span className="muted">No owner</span>, text: "No owner" }, ...members.map((m) => ({ value: m.email, label: `${m.name || m.email} · ${ROLE_LABEL[m.role]}`, text: m.name || m.email }))];
  return (
    <Dialog
      open={target != null}
      title={isNew ? "New service account" : `Edit ${sa?.name ?? ""}`}
      description={isNew ? "For a machine, not a person: it cannot sign in to the console, only use its keys." : undefined}
      confirmLabel={isNew ? "Create and make a key" : "Save"}
      disabled={!valid}
      loading={busy}
      width={520}
      onConfirm={() => void submit()}
      onCancel={() => {
        setForm(null);
        setError(null);
        onCancel();
      }}
    >
      {isNew && (
        <label className="field">
          <span className="field-label">Name</span>
          <input className="input mono" value={f.name} onChange={(e) => set({ name: e.target.value })} placeholder="release-bot" autoComplete="off" spellCheck={false} aria-invalid={f.name !== "" && !valid} autoFocus />
          <span className="field-hint">Lowercase letters, digits and dashes; unique in {tenantName}.</span>
        </label>
      )}
      <label className="field">
        <span className="field-label">Description</span>
        <input className="input" value={f.description} onChange={(e) => set({ description: e.target.value })} placeholder="Tags and publishes releases" autoComplete="off" />
      </label>
      <div className="field">
        <span className="field-label">Owner</span>
        <Select value={f.ownerEmail} onChange={(ownerEmail) => set({ ownerEmail })} options={ownerOptions} searchable={members.length > 8} />
        <span className="field-hint">Answers for it: rotates its keys and acts on its Runs. Its cost also rolls up to them under “By owner”.</span>
      </div>
      <RoleField value={f.role} onChange={(r) => set({ role: r === "owner" ? "admin" : r })} options={(["viewer", "member", "admin"] as const).map((r) => ({ value: r, label: ROLE_LABEL[r], text: ROLE_LABEL[r] }))} />
      {error && (
        <div className="error-strip" role="alert">
          {error}
        </div>
      )}
    </Dialog>
  );
}

/** /service-accounts/:id: facts, spend, keys (max 2, rotation) and Runs. */
export function ServiceAccountPage({ id }: { id: string }) {
  const v = useViewer();
  const toast = useToast();
  const { tenant, name: tenantName } = usePeopleTenant();
  const q = useQuery(`sa:${id}@${tenant ?? ""}`, (s) => api.serviceAccount(tenant, id, s));
  const keys = useQuery(`sa:${id}:keys@${tenant ?? ""}`, (s) => api.serviceAccountKeys(tenant, id, s));
  const members = useQuery(`members@${tenant ?? ""}`, (s) => api.members(tenant, s));
  const cost = useCostByOwner("member");
  const runs = useQuery(`sa:${id}:runs@${tenant ?? ""}`, (s) => api.runsPage(tenant, { owner: q.data?.name ?? id, limit: 50 }, s).then((p) => p.rows), { enabled: q.data != null });
  const [editing, setEditing] = useState(false);
  const [disabling, setDisabling] = useState(false);
  const [newKey, setNewKey] = useState(false);
  const [created, setCreated] = useState<CreatedApiKey | null>(null);
  const [revoking, setRevoking] = useState<ApiKey | null>(null);
  const [busy, setBusy] = useState(false);

  if (q.error && !q.data && !q.loading) return <div className="page"><ErrorBlock error={q.error} onRetry={q.refetch} /></div>;
  if (!q.data) return <PageSkeleton />;
  const s = q.data;
  const admin = atLeast(v, "admin");
  const manageKeys = admin || (s.owner != null && s.owner.id === v.userId);
  const active = keys.data ?? [];
  const full = active.length >= SA_KEY_CAP;
  const mine = cost.byKey.get(`sa:${s.id}`);
  const total = [...cost.byKey.values()].reduce((n, r) => n + Number(r.amount), 0);

  const act = async (title: string, fn: () => Promise<unknown>, after: () => void) => {
    setBusy(true);
    try {
      await fn();
      toast({ title, tone: "success" });
      after();
      await Promise.all([q.refetch(), keys.refetch()]);
    } catch (e) {
      toast({ title: "Not done", description: errorText(e), tone: "danger" });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="page">
      <PageHeader
        title={
          <span className="profile-head">
            <Avatar name={s.name} serviceAccount size="lg" />
            <span>{s.name}</span>
          </span>
        }
        badges={
          <>
            <Badge outline>Service account</Badge>
            <RoleBadge role={s.role} />
            {s.disabledAt && <Badge tone="warn">Disabled</Badge>}
          </>
        }
        description={s.description || undefined}
        actions={
          admin ? (
            <>
              <Button onClick={() => setEditing(true)}>Edit</Button>
              {!s.disabledAt && (
                <Button variant="danger" onClick={() => setDisabling(true)}>
                  Disable
                </Button>
              )}
            </>
          ) : undefined
        }
      />
      <div className="stack">
        <KeyValue
          columns={3}
          items={[
            { key: "Owner", value: s.owner ? <PersonInline person={s.owner} /> : <span className="muted">no owner: an admin can assign one</span> },
            { key: "Role", value: `${ROLE_LABEL[s.role]} · ${ROLE_WHAT[s.role] ?? ROLE_DESC[s.role]}` },
            { key: "Status", value: s.disabledAt ? <span>disabled <RelativeTime at={s.disabledAt} /></span> : "Active" },
            { key: "Created", value: <span><RelativeTime at={s.createdAt} />{s.createdBy ? ` by ${s.createdBy.name || s.createdBy.email}` : ""}</span> },
            { key: "Last used", value: <RelativeTime at={s.lastUsedAt} /> },
            { key: "Id", value: <IdChip value={s.id} /> },
          ]}
        />
        <div className="grid grid-stats">
          <StatTile label="Runs · 30d" value={mine?.runs != null ? mine.runs.toLocaleString("en-US") : "–"} loading={cost.loading} />
          <StatTile label="Cost · 30d" value={mine ? <Money amount={mine.amount} currency={mine.currency} decimals={CENTS} /> : "–"} unit={mine && total > 0 ? `${Math.round((Number(mine.amount) / total) * 100)}% of ${tenantName}` : "list prices"} loading={cost.loading} />
          <StatTile label="Keys" value={`${active.length} / ${SA_KEY_CAP}`} unit="active" loading={keys.loading} />
        </div>
        <Card
          flush
          title="Keys"
          subtitle={`At most ${SA_KEY_CAP} active, so a key can be rotated without downtime: create the new one, switch the client, revoke the old.`}
          actions={
            manageKeys ? (
              <Button variant="primary" size="sm" icon={<IconPlus size={14} />} disabled={full || keys.loading || s.disabledAt != null} title={full ? `${SA_KEY_CAP} active keys is the limit: revoke the old one first` : undefined} onClick={() => setNewKey(true)}>
                New key
              </Button>
            ) : undefined
          }
        >
          {manageKeys && full && <div className="card-note muted">{SA_KEY_CAP} active keys is the limit: revoke the old one first.</div>}
          <KeysTable keys={active} loading={keys.loading} access={false} createdBy onRevoke={manageKeys ? setRevoking : undefined} empty="No keys: nothing can use this account." />
        </Card>
        <Card flush title="Runs" subtitle="this account's, newest first">
          <Table<Run> columns={runColumns({ tenant: false, owner: true, adapter: false })} rows={runs.data ?? []} rowKey={(r) => r.id} loading={runs.loading} onRowClick={(r) => go(runPath(r.id))} defaultSort={{ key: "created", dir: "desc" }} empty={`No Runs by ${s.name}.`} />
        </Card>
      </div>
      <ServiceAccountDialog target={editing ? s : null} tenant={tenant} tenantName={tenantName} members={members.data ?? []} onCancel={() => setEditing(false)} onSaved={(x) => (setEditing(false), toast({ title: `Saved ${x.name}`, tone: "success" }), void q.refetch())} />
      <NewKeyDialog
        open={newKey}
        who={`The key acts as ${s.name} (${ROLE_LABEL[s.role]} in ${tenantName}).`}
        onCreate={async (body) => {
          try {
            const k = await api.createServiceAccountKey(tenant, s.id, body);
            setNewKey(false);
            setCreated(k);
            await Promise.all([keys.refetch(), q.refetch()]);
          } catch (e) {
            throw new Error(errorText(e));
          }
        }}
        onCancel={() => setNewKey(false)}
      />
      <KeyRevealDialog created={created} onDone={() => setCreated(null)} />
      <ConfirmDialog open={revoking != null} title={`Revoke ${revoking?.name ?? ""}?`} description={`Clients still using it stop working at once. Switch them to the other key first.`} confirmLabel="Revoke key" tone="danger" loading={busy} onConfirm={() => revoking && void act(`Revoked ${revoking.name}`, () => api.revokeServiceAccountKey(tenant, s.id, revoking.id), () => setRevoking(null))} onCancel={() => setRevoking(null)} />
      <ConfirmDialog open={disabling} title={`Disable ${s.name}?`} description="Its keys stop working at once; its running Runs keep running." confirmLabel="Disable" tone="danger" loading={busy} onConfirm={() => void act(`Disabled ${s.name}`, () => api.updateServiceAccount(tenant, s.id, { disabled: true }), () => setDisabling(false))} onCancel={() => setDisabling(false)} />
    </div>
  );
}
