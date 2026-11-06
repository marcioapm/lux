import { useCallback, useEffect, useMemo, useState } from "react";
import { Badge, Button, Card, CENTS, ConfirmDialog, Dialog, EmptyState, IconButton, MoneyList, PageHeader, Person, RelativeTime, RoleBadge, ROLE_LABEL, ROLES, SegmentedControl, Table, Tabs, useToast, type Column, type TenantRole } from "@lux/design-system";
import { IconClose, IconCopy, IconPencil, IconPlus, IconRefresh, IconTrash } from "@lux/design-system/icons";
import { api, errorText, useQuery, useSession, type Invitation, type InvitationStatus, type InviteResult, type Member } from "../../api/index.ts";
import { atLeast, useViewer, type Viewer } from "../identity.ts";
import { go, setSearchParams, useSearchParams } from "../router.tsx";
import { useScope } from "../scope.tsx";
import { ErrorBlock, ErrorStrip } from "./common.tsx";
import { InviteDialog, InviteLinksDialog } from "./InviteDialog.tsx";
import { memberPath, PersonInline, RoleField, roleOptions } from "./peopleParts.tsx";
import { costsByKey, keysCost, sortAmount } from "./memberMoney.ts";

/** The tenant a people page is about: the scope's, else the viewer's own. */
export function usePeopleTenant(): { tenant: string | undefined; name: string } {
  const { apiTenant } = useScope();
  const { whoami } = useSession();
  const v = useViewer();
  const m = v.memberships.find((x) => x.tenantId === apiTenant || x.tenant === apiTenant);
  return { tenant: apiTenant, name: m?.tenant ?? (apiTenant || whoami?.tenant || v.memberships[0]?.tenant || "") };
}

/** Admins act on members below Owner; owners and operators on anyone; nobody on themselves. */
export function canManage(v: Viewer, m: Pick<Member, "id" | "role">): boolean {
  if (m.id === v.userId) return false;
  if (!atLeast(v, "admin")) return false;
  return m.role !== "owner" || atLeast(v, "owner");
}

/**
 * Cost · 30d and Runs · 30d per member key ("user:<id>", "sa:<id>"): each
 * key's rows, one per currency (never only the first: a person with USD and
 * EUR cost has both). cost(key) is that per currency.
 */
export function useCostByOwner(by: "member" | "owner") {
  const { tenant } = usePeopleTenant();
  const q = useQuery(`costs:${by}@${tenant ?? ""}`, (s) => api.costsByOwner(tenant, by, "30d", s), { interval: 60_000 });
  const byKey = useMemo(() => costsByKey(q.data?.totals ?? []), [q.data]);
  const cost = useCallback((...keys: string[]) => keysCost(byKey, keys), [byKey]);
  return { ...q, byKey, cost };
}

type Tab = "members" | "invitations";

/** /members: who is in the tenant; for admins+, the Invitations tab and Invite people. */
export function Members() {
  const v = useViewer();
  const session = useSession();
  const tabParam = useSearchParams().get("tab");
  const invites = v.invitationsEnabled && atLeast(v, "admin");
  const tab: Tab = invites && tabParam === "invitations" ? "invitations" : "members";
  const { tenant, name } = usePeopleTenant();
  const members = useQuery(`members@${tenant ?? ""}`, (s) => api.members(tenant, s), { interval: 30_000 });
  const sas = useQuery(`sas@${tenant ?? ""}`, (s) => api.serviceAccounts(tenant, s));
  const counts = useQuery(`invitations:counts@${tenant ?? ""}`, (s) => api.invitationCounts(tenant, s), { enabled: invites });
  const live = useQuery(`invitations:live@${tenant ?? ""}`, (s) => api.invitations(tenant, { status: "all" }, s).then((r) => r.invitations), { enabled: invites });
  const [inviting, setInviting] = useState(false);
  const [results, setResults] = useState<{ results: InviteResult[]; role: TenantRole } | null>(null);
  const [refresh, setRefresh] = useState(0);
  if (v.operator && !tenant && !session.whoami?.tenant && v.memberships.length === 0) {
    return (
      <div className="page page-list">
        <PageHeader title="Members" />
        <EmptyState title="Pick a tenant" description="Members are per tenant: pick one in the top bar." />
      </div>
    );
  }
  const people = members.data?.length ?? 0;
  const admins = (members.data ?? []).filter((m) => m.role === "admin" || m.role === "owner").slice(0, 2).map((m) => m.name || m.email);

  return (
    <div className="page page-list">
      <PageHeader
        title="Members"
        description={
          tab === "invitations" ? (
            <span>{name} · invitations are links: lux sends no email, you send the link</span>
          ) : (
            <span>
              {name} · {people} {people === 1 ? "person" : "people"}
              {sas.data ? ` · ${sas.data.length} service accounts` : ""}
            </span>
          )
        }
        actions={
          invites ? (
            <>
              <Tabs
                value={tab}
                onChange={(t) => setSearchParams({ tab: t === "members" ? null : t })}
                items={[
                  { key: "members", label: "Members", count: members.data?.length },
                  { key: "invitations", label: "Invitations", count: counts.data?.pending },
                ]}
              />
              <Button variant="primary" icon={<IconPlus size={14} />} onClick={() => setInviting(true)}>
                Invite people
              </Button>
            </>
          ) : undefined
        }
      />
      {tab === "members" ? <MembersTable members={members} tenantName={name} /> : <InvitationsTab key={refresh} tenant={tenant} onChanged={() => void Promise.all([counts.refetch(), live.refetch()])} />}
      {!atLeast(v, "admin") && tab === "members" && admins.length > 0 && <p className="page-note">Only admins and owners add or change members. Ask {admins.join(" or ")}.</p>}
      {invites && (
        <>
          <InviteDialog
            open={inviting}
            tenant={tenant}
            tenantName={name}
            members={members.data ?? []}
            invitations={live.data ?? []}
            canInviteOwner={atLeast(v, "owner")}
            onCancel={() => setInviting(false)}
            onDone={(r, role) => {
              setInviting(false);
              setResults({ results: r, role });
              void Promise.all([counts.refetch(), live.refetch()]);
            }}
          />
          <InviteLinksDialog
            results={results?.results ?? null}
            role={results?.role ?? "member"}
            tenantName={name}
            onDone={() => {
              setResults(null);
              setRefresh((n) => n + 1);
              setSearchParams({ tab: "invitations", status: null });
            }}
          />
        </>
      )}
    </div>
  );
}

function MembersTable({ members, tenantName }: { members: ReturnType<typeof useQuery<Member[]>>; tenantName: string }) {
  const v = useViewer();
  const toast = useToast();
  const { tenant } = usePeopleTenant();
  const cost = useCostByOwner("member");
  const [changing, setChanging] = useState<Member | null>(null);
  const [removing, setRemoving] = useState<Member | null>(null);
  const [busy, setBusy] = useState(false);
  const admin = atLeast(v, "admin");
  const rows = members.data ?? [];
  const keysShown = rows.some((m) => m.keyCount != null);
  const owners = rows.filter((m) => m.role === "owner").length;

  const cols = useMemo<Column<Member>[]>(() => {
    const c: Column<Member>[] = [
      { key: "who", header: "Member", cell: (m) => <Person name={m.name || m.email} sub={m.email} picture={m.picture} note={m.id === v.userId ? "(you)" : undefined} />, sortValue: (m) => m.name || m.email, label: "Member" },
      {
        key: "role",
        header: "Role",
        cell: (m) => (
          <span className="row" style={{ gap: 4 }}>
            <RoleBadge role={m.role} />
            {m.operator && (
              <Badge tone="warn" title="Platform operator: every tenant">
                Operator
              </Badge>
            )}
          </span>
        ),
        sortValue: (m) => ROLES.indexOf(m.role),
        sortFirst: "desc",
        width: 170,
      },
      { key: "running", header: "Running", cell: (m) => m.runningRuns, sortValue: (m) => m.runningRuns, align: "right", mono: true, width: 90 },
      { key: "runs", header: "Runs · 30d", cell: (m) => cost.cost(`user:${m.id}`).runs ?? "–", sortValue: (m) => cost.cost(`user:${m.id}`).runs, align: "right", mono: true, width: 104 },
      {
        key: "cost",
        header: "Cost · 30d",
        cell: (m) => <MoneyList amounts={cost.cost(`user:${m.id}`).amounts} decimals={CENTS} />,
        sortValue: (m) => sortAmount(cost.cost(`user:${m.id}`)),
        align: "right",
        mono: true,
        width: 110,
      },
    ];
    if (keysShown) c.push({ key: "keys", header: "Keys", cell: (m) => (m.keyCount != null ? m.keyCount : <span className="muted" title="Only the person and admins see their keys">·</span>), sortValue: (m) => m.keyCount, align: "right", mono: true, width: 70 });
    c.push({ key: "seen", header: "Last seen", cell: (m) => <RelativeTime at={m.lastSeenAt} label="Last seen" />, sortValue: (m) => (m.lastSeenAt ? Date.parse(m.lastSeenAt) : null), sortKind: "time", align: "right", width: 104 });
    if (admin)
      c.push({
        key: "actions",
        header: "",
        label: "Actions",
        cell: (m) =>
          canManage(v, m) ? (
            <span className="row-actions">
              <IconButton size="sm" label={`Change ${m.name || m.email}'s role`} onClick={(e) => (e.stopPropagation(), setChanging(m))} onKeyDown={(e) => e.stopPropagation()}>
                <IconPencil size={14} />
              </IconButton>
              <IconButton size="sm" label={`Remove ${m.name || m.email} from ${tenantName}`} disabled={lastOwnerReason(v, m, owners) != null} title={lastOwnerReason(v, m, owners) ?? undefined} onClick={(e) => (e.stopPropagation(), setRemoving(m))} onKeyDown={(e) => e.stopPropagation()}>
                <IconTrash size={14} />
              </IconButton>
            </span>
          ) : null,
        align: "right",
        width: 84,
      });
    return c;
  }, [v, cost.cost, keysShown, admin, tenantName, owners]);

  const act = async (title: string, fn: () => Promise<unknown>, close: () => void) => {
    setBusy(true);
    try {
      await fn();
      toast({ title, tone: "success" });
      close();
      await members.refetch();
    } catch (e) {
      toast({ title: "Not done", description: errorText(e), tone: "danger" });
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <Card flush>
        <ErrorStrip error={rows.length > 0 ? members.error : null} />
        {members.error && !members.data && !members.loading ? (
          <ErrorBlock error={members.error} onRetry={members.refetch} />
        ) : (
          <Table columns={cols} rows={rows} rowKey={(m) => m.id} loading={members.loading} onRowClick={(m) => go(memberPath(m.id))} defaultSort={{ key: "role", dir: "desc" }} empty="No members yet." minWidth={860} />
        )}
      </Card>
      <ChangeRoleDialog member={changing} tenantName={tenantName} busy={busy} owner={atLeast(v, "owner")} lastOwner={changing ? lastOwnerReason(v, changing, owners) : null} onCancel={() => setChanging(null)} onConfirm={(m, role) => void act(`${m.name || m.email} is now ${ROLE_LABEL[role]}`, () => api.setMemberRole(tenant, m.id, role), () => setChanging(null))} />
      <RemoveMemberDialog member={removing} tenantName={tenantName} busy={busy} onCancel={() => setRemoving(null)} onConfirm={(m) => void act(`Removed ${m.name || m.email}`, () => api.removeMember(tenant, m.id), () => setRemoving(null))} />
    </>
  );
}

/**
 * The last-owner rule (members.go checkOwnerChange): only an operator may
 * demote or remove a tenant's only owner. The reason, or null when allowed.
 */
export function lastOwnerReason(v: Viewer, m: Pick<Member, "role">, owners: number): string | null {
  if (m.role !== "owner" || owners > 1 || v.operator) return null;
  return "The only owner: make someone else an owner first";
}

export function ChangeRoleDialog({ member, tenantName, owner, busy, onConfirm, onCancel, lastOwner = null }: { member: Member | null; tenantName: string; owner: boolean; busy: boolean; onConfirm: (m: Member, role: TenantRole) => void; onCancel: () => void; lastOwner?: string | null }) {
  // The pick is for one member, and ends when the dialog closes (after a change, or a cancel): the next open starts from the member's role.
  const [pick, setPick] = useState<{ for: string; role: TenantRole } | null>(null);
  useEffect(() => {
    if (!member) setPick(null);
  }, [member]);
  const role = pick && pick.for === member?.id ? pick.role : null;
  const shown = role ?? member?.role ?? "member";
  const who = member?.name || member?.email || "";
  const options = roleOptions({ owner }).map((o) => (lastOwner && o.value !== "owner" ? { ...o, disabled: lastOwner } : o));
  return (
    <Dialog
      open={member != null}
      title={`Change ${who}'s role`}
      description={lastOwner ?? `Takes effect on ${who.split(" ")[0]}'s next request, keys included.`}
      confirmLabel="Change role"
      disabled={shown === member?.role}
      loading={busy}
      onConfirm={() => member && onConfirm(member, shown)}
      onCancel={() => {
        setPick(null);
        onCancel();
      }}
    >
      <RoleField value={shown} onChange={(r) => member && setPick({ for: member.id, role: r })} options={options} label={`Role in ${tenantName}`} />
    </Dialog>
  );
}

export function RemoveMemberDialog({ member, tenantName, busy, onConfirm, onCancel }: { member: Member | null; tenantName: string; busy: boolean; onConfirm: (m: Member) => void; onCancel: () => void }) {
  const who = member?.name || member?.email || "";
  return (
    <ConfirmDialog
      open={member != null}
      title={`Remove ${who} from ${tenantName}?`}
      description={`Their API keys in ${tenantName} are revoked at once. Their running Runs keep running and stay theirs; service accounts they own are left without an owner.`}
      confirmLabel="Remove"
      tone="danger"
      loading={busy}
      onConfirm={() => member && onConfirm(member)}
      onCancel={onCancel}
    />
  );
}

const STATUS_PILL: Record<InvitationStatus, { hue: string; label: string }> = {
  pending: { hue: "blue", label: "Pending" },
  expired: { hue: "amber", label: "Expired" },
  accepted: { hue: "green", label: "Accepted" },
  revoked: { hue: "neutral", label: "Revoked" },
};
const STATUSES = ["pending", "expired", "accepted", "revoked", "all"] as const;
type StatusFilter = (typeof STATUSES)[number];

function InvitationStatusPill({ status }: { status: InvitationStatus }) {
  const s = STATUS_PILL[status];
  return (
    <span className={`pill pill-${s.hue}`} data-state={status}>
      <span className="pill-dot" aria-hidden="true" />
      <span className="pill-label">{s.label}</span>
    </span>
  );
}

/** Members › Invitations: by status (with counts), search by email; copy link, renew, revoke. */
function InvitationsTab({ tenant, onChanged }: { tenant: string | undefined; onChanged: () => void }) {
  const v = useViewer();
  const toast = useToast();
  const params = useSearchParams();
  const sp = params.get("status");
  const status: StatusFilter = STATUSES.includes(sp as StatusFilter) ? (sp as StatusFilter) : "pending";
  // The search is in the URL (?q=), so a link or a reload keeps it; typing replaces the entry.
  const urlQ = params.get("q") ?? "";
  const [q, setQ] = useState(urlQ);
  useEffect(() => setQ(urlQ), [urlQ]);
  useEffect(() => {
    if (q === urlQ) return;
    const t = setTimeout(() => setSearchParams({ q: q.trim() || null }, { replace: true }), 250);
    return () => clearTimeout(t);
  }, [q, urlQ]);
  const counts = useQuery(`invitations:counts@${tenant ?? ""}`, (s) => api.invitationCounts(tenant, s));
  const listKey = `invitations@${tenant ?? ""}:${status}:${urlQ}`;
  const ask = (cursor: string | undefined, s?: AbortSignal) => api.invitations(tenant, { status: status === "all" ? undefined : status, q: urlQ.trim() || undefined, cursor }, s);
  const list = useQuery(listKey, (s) => ask(undefined, s), { keep: true });
  // Load more: the pages after the first, by luxd's cursor ("" when there are no more); a new filter starts over.
  const [more, setMore] = useState<{ key: string; rows: Invitation[]; cursor: string } | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);
  const extra = more?.key === listKey ? more : null;
  const rows = [...(list.data?.invitations ?? []), ...(extra?.rows ?? [])];
  const cursor = extra ? extra.cursor : (list.data?.cursor ?? "");
  const loadMore = async () => {
    setLoadingMore(true);
    try {
      const r = await ask(cursor);
      setMore({ key: listKey, rows: [...(extra?.rows ?? []), ...r.invitations], cursor: r.cursor ?? "" });
    } catch (e) {
      toast({ title: "Could not load more invitations", description: errorText(e), tone: "danger" });
    } finally {
      setLoadingMore(false);
    }
  };
  const [revoking, setRevoking] = useState<Invitation | null>(null);
  const [busy, setBusy] = useState(false);
  const owner = atLeast(v, "owner");

  const run = async (title: string, fn: () => Promise<unknown>) => {
    setBusy(true);
    try {
      await fn();
      toast({ title, tone: "success" });
      setRevoking(null);
      setMore(null);
      await Promise.all([list.refetch(), counts.refetch()]);
      onChanged();
    } catch (e) {
      toast({ title: "Not done", description: errorText(e), tone: "danger" });
    } finally {
      setBusy(false);
    }
  };
  const copy = (i: Invitation) => void navigator.clipboard?.writeText(i.url).then(() => toast({ title: `Link for ${i.email} copied`, tone: "success" }));

  const cols: Column<Invitation>[] = [
    { key: "email", header: "Email", cell: (i) => <span className="person-name">{i.email}</span>, sortValue: (i) => i.email, lead: true },
    { key: "role", header: "Role", cell: (i) => <RoleBadge role={i.role} />, sortValue: (i) => ROLES.indexOf(i.role), width: 100 },
    { key: "by", header: "Invited by", cell: (i) => <PersonInline person={i.invitedBy} />, sortValue: (i) => i.invitedBy?.name, width: 170 },
    { key: "sent", header: "Sent", cell: (i) => <RelativeTime at={i.renewedAt ?? i.createdAt} label={i.renewedAt ? "Renewed" : "Sent"} />, sortValue: (i) => Date.parse(i.renewedAt ?? i.createdAt), sortKind: "time", align: "right", width: 110 },
    {
      key: "expires",
      header: "Expires",
      cell: (i) =>
        i.status === "accepted" ? (
          <span className="muted">
            accepted <RelativeTime at={i.acceptedAt} />
          </span>
        ) : i.status === "revoked" ? (
          <span className="muted">by {i.revokedBy?.name ?? "–"}</span>
        ) : i.status === "expired" ? (
          <Badge tone="warn">
            <RelativeTime at={i.expiresAt} />
          </Badge>
        ) : (
          <RelativeTime at={i.expiresAt} label="Expires" />
        ),
      sortValue: (i) => Date.parse(i.expiresAt),
      sortKind: "time",
      align: "right",
      width: 150,
    },
    { key: "status", header: "Status", cell: (i) => <InvitationStatusPill status={i.status} />, sortValue: (i) => i.status, width: 110 },
    {
      key: "actions",
      header: "",
      label: "Actions",
      cell: (i) =>
        i.role === "owner" && !owner ? null : (
          <span className="row-actions">
            {i.status === "pending" && (
              <IconButton size="sm" label={`Copy link for ${i.email}`} onClick={() => copy(i)}>
                <IconCopy size={14} />
              </IconButton>
            )}
            {(i.status === "pending" || i.status === "expired") && (
              <>
                <IconButton size="sm" label={i.status === "pending" ? `Renew ${i.email}: 14 more days, same link` : `Renew ${i.email}: the same link works again for 14 days`} onClick={() => void run(`Renewed ${i.email}: 14 more days`, () => api.renewInvitation(tenant, i.id))}>
                  <IconRefresh size={14} />
                </IconButton>
                <IconButton size="sm" label={`Revoke ${i.email}`} onClick={() => setRevoking(i)}>
                  <IconClose size={14} />
                </IconButton>
              </>
            )}
          </span>
        ),
      align: "right",
      width: 116,
    },
  ];
  const c = counts.data;
  return (
    <div className="stack">
      <div className="filter-row">
        <SegmentedControl<StatusFilter>
          label="Status"
          value={status}
          onChange={(s) => setSearchParams({ status: s === "pending" ? null : s })}
          options={STATUSES.map((s) => ({ value: s, label: s === "all" ? "All" : `${STATUS_PILL[s].label} ${c ? c[s] : ""}`.trim() }))}
        />
        <input className="input input-sm" type="search" placeholder="Search email" aria-label="Search email" value={q} onChange={(e) => setQ(e.target.value)} />
      </div>
      <Card flush>
        <ErrorStrip error={list.data ? list.error : null} />
        {list.error && !list.data && !list.loading ? (
          <ErrorBlock error={list.error} onRetry={list.refetch} />
        ) : (
          <Table columns={cols} rows={rows} rowKey={(i) => i.id} loading={list.loading} defaultSort={{ key: "sent", dir: "desc" }} empty={urlQ ? `No invitations match “${urlQ}”.` : "No invitations here."} minWidth={860} />
        )}
        {cursor !== "" && list.data && (
          <div className="card-foot">
            <span className="muted">{rows.length} shown</span>
            <Button size="sm" variant="ghost" loading={loadingMore} onClick={() => void loadMore()}>
              Load more
            </Button>
          </div>
        )}
      </Card>
      <p className="page-note">An invitation is for one email and works only when that email signs in. It lasts 14 days; Renew keeps the same link. Accepting creates the membership with the invited role.</p>
      <ConfirmDialog
        open={revoking != null}
        title={`Revoke the invitation for ${revoking?.email ?? ""}?`}
        description="Its link stops working at once. You can invite the address again later."
        confirmLabel="Revoke"
        tone="danger"
        loading={busy}
        onConfirm={() => revoking && void run(`Revoked the invitation for ${revoking.email}`, () => api.revokeInvitation(tenant, revoking.id))}
        onCancel={() => setRevoking(null)}
      />
    </div>
  );
}
