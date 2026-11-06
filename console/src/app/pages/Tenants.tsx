import { useMemo, useState } from "react";
import { Badge, Button, Dialog, InviteLinkList, Card, formatBytes, formatCount, formatDuration, IdChip, isEmail, PageHeader, PersonPicker, RoleBadge, ROLE_LABEL, Table, useToast, type Column, RelativeTime } from "@lux/design-system";
import { api, errorText, useQuery, type Invitation, type Tenant } from "../../api/index.ts";
import { go, linkTo } from "../router.tsx";
import { operatorActs, useViewer } from "../identity.ts";
import { useScope } from "../scope.tsx";
import { ErrorBlock, ErrorStrip, UsageBar } from "./common.tsx";
import { memberByEmail, memberOptions, memberPath } from "./peopleParts.tsx";

// expiry: how long a stopped, lost, failed or succeeded Run of the tenant may rest
// before it is terminated; 0 is never.
export function expiry(days: number): string {
  return days === 0 ? "never" : formatDuration(days * 86_400);
}

/** The Owners cell: each owner's name (else email), linked to their page in that tenant; "No owner" at ownersCount 0 (a button to Set owner, for operators); – from an older luxd. */
export function ownersCell(t: Tenant, onSetOwner?: (t: Tenant) => void) {
  if (t.ownersCount == null) return "–";
  if (t.ownersCount === 0)
    return onSetOwner ? (
      <button type="button" className="badge-button" aria-label={`No owner: set ${t.name}'s owner`} title="Set an owner" onClick={(e) => (e.stopPropagation(), onSetOwner(t))} onKeyDown={(e) => e.stopPropagation()}>
        <Badge tone="warn">No owner</Badge>
      </button>
    ) : (
      <Badge tone="warn">No owner</Badge>
    );
  const owners = t.owners ?? [];
  if (owners.length === 0) return String(t.ownersCount);
  return (
    <span className="tenant-owners">
      {owners.map((o, i) => (
        <span key={o.id}>
          {i > 0 && ", "}
          <a {...linkTo(`${memberPath(o.id)}?tenant=${encodeURIComponent(t.id)}`)} title={o.email}>{o.name || o.email}</a>
        </span>
      ))}
    </span>
  );
}

export function Tenants() {
  const scope = useScope();
  const viewer = useViewer();
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  /** The new tenant (its name, from POST /v1/tenants) and its first owner's invitation. */
  const [link, setLink] = useState<{ tenant: string; invitation: Invitation } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const q = useQuery("tenants:page", (s) => api.tenants(s), { interval: 15_000 });
  const ops = operatorActs(viewer);
  const [settingOwner, setSettingOwner] = useState<Tenant | null>(null);

  const cols = useMemo<Column<Tenant>[]>(
    () => [
      { key: "name", header: "Tenant", cell: (t) => t.name, sortValue: (t) => t.name, lead: true, width: 180 },
      { key: "owners", header: "Owners", cell: (t) => ownersCell(t, ops ? setSettingOwner : undefined), sortValue: (t) => t.ownersCount, width: 220 },
      { key: "members", header: "Members", cell: (t) => t.membersCount ?? "–", sortValue: (t) => t.membersCount, align: "right", width: 90 },
      { key: "id", header: "Id", cell: (t) => <IdChip value={t.id} truncate={18} />, sortValue: (t) => t.id, mono: true, width: 200, optional: true },
      { key: "active", header: "Active runs", cell: (t) => (t.maxConcurrentRuns != null ? `${t.activeRuns} / ${t.maxConcurrentRuns}` : String(t.activeRuns)), sortValue: (t) => t.activeRuns, align: "right", mono: true, width: 120 },
      { key: "runs", header: "Total runs", cell: (t) => formatCount(t.runs), sortValue: (t) => t.runs, align: "right", mono: true, width: 110 },
      { key: "hosts", header: "Hosts", cell: (t) => (t.maxHosts != null ? `${t.hosts} / ${t.maxHosts}` : String(t.hosts)), sortValue: (t) => t.hosts, align: "right", mono: true, width: 100 },
      { key: "stored", header: "Stored", cell: (t) => (t.maxStorageBytes != null ? <UsageBar used={t.storedBytes} total={t.maxStorageBytes} unit="bytes" /> : <span className="num">{formatBytes(t.storedBytes)}</span>), sortValue: (t) => t.storedBytes },
      { key: "retention", header: "Retention", cell: (t) => `${t.retentionDays}d`, sortValue: (t) => t.retentionDays, align: "right", mono: true, width: 100, optional: true },
      { key: "expiry", header: "Expiry", cell: (t) => expiry(t.expireAfterDays), sortValue: (t) => t.expireAfterDays || Infinity, align: "right", mono: true, width: 100, optional: true },
      { key: "created", header: "Created", cell: (t) => <RelativeTime at={t.createdAt} />, sortValue: (t) => Date.parse(t.createdAt), align: "right", width: 104 },
      ...(ops
        ? [
            {
              key: "actions",
              header: "",
              label: "Actions",
              cell: (t: Tenant) => (
                <span className="row-actions">
                  <Button size="sm" variant="ghost" aria-label={`Set ${t.name}'s owner`} onClick={(e) => (e.stopPropagation(), setSettingOwner(t))} onKeyDown={(e) => e.stopPropagation()}>
                    Set owner
                  </Button>
                </span>
              ),
              align: "right" as const,
              width: 110,
            },
          ]
        : []),
    ],
    [ops],
  );

  const open = (t: Tenant) => {
    scope.setTenant(t.id);
    go("/");
  };

  return (
    <div className="page page-list">
      <PageHeader title="Tenants" description={`${q.data?.length ?? 0} tenants · click one to scope the console to it`} actions={operatorActs(viewer) ? <Button variant="primary" onClick={() => setCreating(true)}>New tenant</Button> : undefined} />
      <Card flush>
        <ErrorStrip error={(q.data?.length ?? 0) > 0 ? q.error : null} />
        {q.error && !q.data && !q.loading ? (
          <ErrorBlock error={q.error} onRetry={q.refetch} />
        ) : (
          <Table columns={cols} rows={q.data ?? []} rowKey={(t) => t.id} loading={q.loading} defaultSort={{ key: "name", dir: "asc" }} onRowClick={open} selected={scope.tenant} empty="No tenants." />
        )}
      </Card>
      <Dialog open={creating} title="New tenant" description="Creates the tenant and, optionally, an Owner invitation for its first owner." confirmLabel={viewer.invitationsEnabled && email.trim() ? "Create and get link" : "Create tenant"} disabled={!name.trim()} loading={busy} onCancel={() => setCreating(false)} onConfirm={() => { setBusy(true); setError(null); void api.createTenant({ name: name.trim(), ...(viewer.invitationsEnabled && email.trim() ? { ownerEmail: email.trim() } : {}) }).then(async (r) => { setCreating(false); setLink(r.invitation ? { tenant: r.name, invitation: r.invitation } : null); setName(""); setEmail(""); await q.refetch(); }).catch((e) => setError(errorText(e))).finally(() => setBusy(false)); }}>
        <label className="field"><span className="field-label">Name</span><input className="input" value={name} onChange={(e) => setName(e.target.value)} autoFocus /></label>
        {viewer.invitationsEnabled && <label className="field"><span className="field-label">First owner's email</span><input className="input" type="email" value={email} onChange={(e) => setEmail(e.target.value)} /><span className="field-hint">Optional. Empty: the tenant starts with no owner, and you invite one later.</span></label>}
        <ErrorStrip error={error} />
      </Dialog>
      <Dialog open={link != null} title={`Send ${link?.tenant ?? ""}'s Owner invitation`} confirmLabel="Done" onConfirm={() => setLink(null)} onCancel={() => setLink(null)}>{link && <InviteLinkList links={[{ email: link.invitation.email, url: link.invitation.url }]} />}</Dialog>
      {settingOwner && <SetOwnerDialog tenant={settingOwner} onCancel={() => setSettingOwner(null)} onDone={() => (setSettingOwner(null), void q.refetch())} />}
    </div>
  );
}

/**
 * Set owner (operators): an email, suggested from the tenant's members.
 * One call, POST /v1/members {email, role: owner, upsert: true}: a member
 * becomes Owner (their role changes), anyone else joins as Owner. No
 * invitation, so it works in key console mode too.
 */
export function SetOwnerDialog({ tenant, onCancel, onDone }: { tenant: Tenant | null; onCancel: () => void; onDone: () => void }) {
  const toast = useToast();
  const members = useQuery(`members@${tenant?.id ?? ""}`, (s) => api.members(tenant!.id, s), { enabled: tenant != null });
  const [email, setEmail] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const list = members.data ?? [];
  const already = memberByEmail(list, email);
  const valid = isEmail(email) && already?.role !== "owner";
  const close = () => (setEmail(""), setError(null));
  const submit = async () => {
    if (!tenant) return;
    setBusy(true);
    setError(null);
    try {
      const m = await api.addMember(tenant.id, { email: email.trim(), role: "owner", upsert: true });
      toast({ title: `${m.name || m.email} is now an owner of ${tenant.name}`, tone: "success" });
      close();
      onDone();
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(false);
    }
  };
  const note = !isEmail(email) ? "Becomes Owner of " + (tenant?.name ?? "") : already ? `${already.name || already.email} is ${ROLE_LABEL[already.role]} here: ${already.role === "owner" ? "already an owner" : "becomes Owner"}` : `Not a member of ${tenant?.name ?? ""} yet: joins and becomes Owner, no invitation needed`;
  return (
    <Dialog
      open={tenant != null}
      title={`Set ${tenant?.name ?? ""}'s owner`}
      description="Owners manage everyone in the tenant, owners included. Existing owners stay owners."
      confirmLabel="Make owner"
      disabled={!valid}
      loading={busy}
      width={480}
      onConfirm={() => void submit()}
      onCancel={() => (close(), onCancel())}
    >
      <PersonPicker people={memberOptions(list, (m) => (m.role === "owner" ? "already an owner" : undefined))} value={email} onChange={setEmail} label="Email" hint={note} autoFocus />
      <div className="row" style={{ gap: 6 }}>
        <span className="muted">Role</span> <RoleBadge role="owner" />
      </div>
      <ErrorStrip error={error} />
    </Dialog>
  );
}
