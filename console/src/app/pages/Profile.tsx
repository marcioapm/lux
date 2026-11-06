import { useState } from "react";
import { Avatar, Badge, Button, Callout, Card, ConfirmDialog, formatCount, Money, PageHeader, RelativeTime, RoleBadge, ROLE_LABEL, StatTile, Table, useToast, CENTS, type Column } from "@lux/design-system";
import { IconPlus } from "@lux/design-system/icons";
import { api, errorText, useQuery, useSession, type ApiKey, type CreatedApiKey, type Membership } from "../../api/index.ts";
import { atLeast, canManageOwnKeys, useViewer } from "../identity.ts";
import { useScope } from "../scope.tsx";
import { ErrorBlock, ErrorStrip, PageSkeleton } from "./common.tsx";
import { KeyRevealDialog, KeysTable, NewKeyDialog } from "./peopleParts.tsx";

/** At most this many active personal keys per person per tenant (luxd enforces it: 409 key_limit). */
export const PERSONAL_KEY_CAP = 5;

/** /me: who I am, my role in each tenant, my API keys. */
export function Profile() {
  const viewer = useViewer();
  const session = useSession();
  const { apiTenant } = useScope();
  const toast = useToast();
  const me = useQuery("me", (s) => api.me(s));
  const keys = useQuery(`me:keys@${apiTenant ?? ""}`, (s) => api.myKeys(apiTenant, s));
  const members = useQuery(`members@${apiTenant ?? ""}`, (s) => api.members(apiTenant, s));
  const cost = useQuery(`costs:member@${apiTenant ?? ""}`, (s) => api.costsByOwner(apiTenant, "member", "30d", s));
  const [newKey, setNewKey] = useState(false);
  const [created, setCreated] = useState<CreatedApiKey | null>(null);
  const [renaming, setRenaming] = useState<ApiKey | null>(null);
  const [revoking, setRevoking] = useState<ApiKey | null>(null);
  const [busy, setBusy] = useState(false);

  if (me.error && !me.data && !me.loading) return <div className="page"><ErrorBlock error={me.error} onRetry={me.refetch} /></div>;
  if (!me.data) return <PageSkeleton />;
  const p = me.data;
  const tenantName = viewer.memberships.find((m) => m.tenantId === apiTenant || m.tenant === apiTenant)?.tenant ?? session.whoami?.tenant ?? p.memberships[0]?.tenant ?? "";
  const role = viewer.role;
  const active = keys.data ?? [];
  const full = active.length >= PERSONAL_KEY_CAP;
  // A read-only key changes nothing, its own keys included (luxd refuses it).
  const manage = canManageOwnKeys(viewer);
  const self = members.data?.find((m) => m.id === p.id);
  const mine = cost.data?.totals.filter((r) => r.key === `user:${p.id}`) ?? [];

  const act = async (what: string, fn: () => Promise<unknown>, done: () => void) => {
    setBusy(true);
    try {
      await fn();
      toast({ title: what, tone: "success" });
      done();
      await keys.refetch();
    } catch (e) {
      toast({ title: `${what.split(" ")[0]} failed`, description: errorText(e), tone: "danger" });
    } finally {
      setBusy(false);
    }
  };

  const tenantCols: Column<Membership>[] = [
    { key: "tenant", header: "Tenant", cell: (m) => m.tenant, sortValue: (m) => m.tenant, lead: true },
    { key: "role", header: "Role", cell: (m) => <RoleBadge role={m.role} />, width: 120 },
    { key: "since", header: "Member since", cell: (m) => <RelativeTime at={m.joinedAt} />, sortValue: (m) => (m.joinedAt ? Date.parse(m.joinedAt) : null), align: "right", width: 140 },
  ];

  return (
    <div className="page">
      <PageHeader
        title={
          <span className="profile-head">
            <Avatar name={p.name || p.email} picture={p.picture} size="lg" />
            <span>{p.name || p.email}</span>
          </span>
        }
        badges={
          <>
            {role && <RoleBadge role={role} />}
            {p.operator && (
              <Badge tone="warn" title="Platform operator: every tenant">
                Operator
              </Badge>
            )}
          </>
        }
        description={
          <>
            <span>{p.email}</span>
            <span>{session.consoleAuth === "cloudflare-access" ? "signed in through Cloudflare Access" : "signed in with an API key"}</span>
            {p.lastSignInAt && (
              <span>
                last sign-in <RelativeTime at={p.lastSignInAt} />
              </span>
            )}
          </>
        }
      />
      <div className="stack">
        <div className="grid grid-stats">
          <StatTile label="Running now" value={self ? String(self.runningRuns) : "–"} unit={tenantName ? `in ${tenantName}` : undefined} loading={members.loading} />
          <StatTile label="Runs · 30d" value={mine[0]?.runs != null ? formatCount(mine[0].runs) : "–"} unit="mine, not my service accounts'" loading={cost.loading} />
          <StatTile label="Cost · 30d" value={mine.length ? <Money amount={mine[0]!.amount} currency={mine[0]!.currency} decimals={CENTS} /> : "–"} unit="list prices" loading={cost.loading} />
          <StatTile label="API keys" value={keys.data ? `${active.length} / ${PERSONAL_KEY_CAP}` : "–"} unit="active" loading={keys.loading} />
        </div>
        <Card
          flush
          title="API keys"
          subtitle={keys.data ? `Keys act as you. ${active.length} of ${PERSONAL_KEY_CAP} active.` : "Keys act as you."}
          actions={
            manage ? (
              <Button variant="primary" size="sm" icon={<IconPlus size={14} />} disabled={full || !keys.data} title={full ? `${PERSONAL_KEY_CAP} active keys is the limit: revoke one first` : undefined} onClick={() => setNewKey(true)}>
                New key
              </Button>
            ) : undefined
          }
        >
          {!manage && viewer.readOnly && <div className="card-note muted">Signed in with a read-only key: it cannot create, rename or revoke keys.</div>}
          {manage && full && <div className="card-note muted">{PERSONAL_KEY_CAP} active keys is the limit: revoke one first.</div>}
          <ErrorStrip error={keys.data ? keys.error : null} />
          {keys.error && !keys.data && !keys.loading ? (
            <ErrorBlock error={keys.error} onRetry={keys.refetch} compact />
          ) : (
            <KeysTable keys={active} loading={keys.loading} role={role} onRename={manage ? setRenaming : undefined} onRevoke={manage ? setRevoking : undefined} empty="No API keys yet. The console works without one; the CLI and scripts need one." />
          )}
        </Card>
        <Card flush title="Tenants" subtitle={p.memberships.length > 1 ? "Your role in each. Switch with the Tenant picker in the top bar." : "Your role in each."}>
          <Table columns={tenantCols} rows={p.memberships} rowKey={(m) => m.tenantId} defaultSort={{ key: "tenant", dir: "asc" }} empty="You are not a member of any tenant." />
        </Card>
        {p.operator && (
          <Callout>
            You are an <b>operator</b> of this lux installation (your email is in luxd's operators config): you see and act on every tenant, including ones you are not a member of.
          </Callout>
        )}
      </div>
      <NewKeyDialog
        open={newKey}
        who={`The key acts as you (${p.name || p.email}${role ? `, ${ROLE_LABEL[role]}` : ""}${tenantName ? ` in ${tenantName}` : ""}). It never does more than you can.`}
        readOnlyChoice={atLeast(viewer, "member") || viewer.operator ? { roleLabel: role ? ROLE_LABEL[role] : "Operator" } : undefined}
        onCreate={async (body) => {
          try {
            const k = await api.createMyKey(apiTenant, body);
            setNewKey(false);
            setCreated(k);
            await keys.refetch();
          } catch (e) {
            throw new Error(errorText(e));
          }
        }}
        onCancel={() => setNewKey(false)}
      />
      <KeyRevealDialog created={created} onDone={() => setCreated(null)} />
      <ConfirmDialog
        open={renaming != null}
        title={`Rename ${renaming?.name ?? ""}`}
        confirmLabel="Rename key"
        input={{ label: "New name", placeholder: renaming?.name, required: true }}
        loading={busy}
        onConfirm={(name) => renaming && void act(`Renamed ${renaming.name} to ${name?.trim()}`, () => api.renameMyKey(apiTenant, renaming.id, (name ?? "").trim()), () => setRenaming(null))}
        onCancel={() => setRenaming(null)}
      />
      <ConfirmDialog
        open={revoking != null}
        title={`Revoke ${revoking?.name ?? ""}?`}
        description="Whatever uses it stops working at once. A revoked key cannot be restored."
        confirmLabel="Revoke key"
        tone="danger"
        loading={busy}
        onConfirm={() => revoking && void act(`Revoked ${revoking.name}`, () => api.revokeMyKey(apiTenant, revoking.id), () => setRevoking(null))}
        onCancel={() => setRevoking(null)}
      />
    </div>
  );
}
