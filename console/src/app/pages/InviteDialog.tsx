// The Invite people dialog (EmailInput + role) and the links it hands back.
import { useMemo, useState } from "react";
import { Badge, classifyEmails, CopyAllLinks, Dialog, emailCounts, EmailInput, InviteLinkList, inviteButtonText, isTaken, RoleBadge, ROLE_LABEL, useToast, type EmailEntry, type EmailVerdict, type TenantRole } from "@lux/design-system";
import { IconCopy, IconUser } from "@lux/design-system/icons";
import { api, errorText, type Invitation, type InviteResult, type Member } from "../../api/index.ts";
import { ErrorStrip } from "./common.tsx";
import { RoleField, roleOptions } from "./peopleParts.tsx";

/** Copies an invitation's link from the left-out list. */
function useCopyLink() {
  const toast = useToast();
  return (u: string) => {
    void navigator.clipboard?.writeText(u).then(() => toast({ title: "Link copied", tone: "success" }));
  };
}

/** Invite people: addresses checked against members and live invitations as they are typed; Owner only for owners. */
export function InviteDialog({ open, tenant, tenantName, members, invitations, canInviteOwner, onDone, onCancel }: { open: boolean; tenant: string | undefined; tenantName: string; members: Member[]; invitations: Invitation[]; canInviteOwner: boolean; onDone: (results: InviteResult[], role: TenantRole) => void; onCancel: () => void }) {
  const [entries, setEntries] = useState<EmailEntry[]>([]);
  const [role, setRole] = useState<TenantRole>("member");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const copyLink = useCopyLink();
  const memberEmails = useMemo(() => new Set(members.map((m) => m.email.toLowerCase())), [members]);
  const classify = (e: { email: string }): EmailVerdict => {
    if (memberEmails.has(e.email)) return { status: "skip", reason: `already in ${tenantName}`, icon: <IconUser size={12} /> };
    const live = invitations.find((i) => i.email.toLowerCase() === e.email && (i.status === "pending" || i.status === "expired"));
    if (live?.status === "pending") return { status: "skip", reason: "already invited", icon: <IconCopy size={12} />, action: { label: "Copy their link", onClick: () => copyLink(live.url) } };
    if (live?.status === "expired") return { status: "renew", reason: "invitation expired: Invite renews it, same link" };
    return { status: "invite" };
  };
  const views = classifyEmails(entries, classify);
  const { taken } = emailCounts(views);
  const reset = () => {
    setEntries([]);
    setRole("member");
    setError(null);
  };
  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      const list = entries.filter((_, i) => isTaken(views[i]!)).map((e) => ({ email: e.email!, ...(e.name ? { name: e.name } : {}) }));
      const results = await api.invite(tenant, { invitations: list, role });
      reset();
      onDone(results, role);
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog
      open={open}
      title={`Invite people to ${tenantName}`}
      description="You get one link per person to send them. lux sends no email; a link works only for its address."
      confirmLabel={inviteButtonText(taken)}
      disabled={taken === 0}
      loading={busy}
      width={520}
      onConfirm={() => void submit()}
      onCancel={() => {
        reset();
        onCancel();
      }}
    >
      <div className="field">
        <label className="field-label" htmlFor="invite-emails">
          Emails
        </label>
        <EmailInput id="invite-emails" entries={entries} onChange={setEntries} classify={classify} hint="Type or paste. Enter, comma or space adds one; “Name <email>” and spreadsheet columns keep the name." />
      </div>
      <RoleField value={role} onChange={setRole} options={roleOptions({ owner: canInviteOwner })} />
      {!canInviteOwner && <span className="field-hint">Only owners invite Owners.</span>}
      <ErrorStrip error={error} />
    </Dialog>
  );
}

/**
 * The links an invite produced: created, renewed or existing ones, each
 * with the role its own invitation carries (a renewed or existing one keeps
 * the role it was sent with, which need not be the role just picked);
 * members and invalid addresses are said in words.
 */
export function InviteLinksDialog({ results, tenantName, onDone }: { results: InviteResult[] | null; role?: TenantRole; tenantName: string; onDone: () => void }) {
  const withLink = (results ?? []).filter((r) => r.invitation);
  const links = withLink.map((r) => ({
    email: r.email,
    url: r.invitation!.url,
    note: (
      <>
        <RoleBadge role={r.invitation!.role} />
        {r.result === "existing" ? (
          <Badge tone="info" title={`Already invited as ${ROLE_LABEL[r.invitation!.role]}: this is the same link`}>
            already invited
          </Badge>
        ) : r.result === "renewed" ? (
          <Badge tone="info" title={`The expired invitation (as ${ROLE_LABEL[r.invitation!.role]}) was renewed: same link, 14 more days`}>
            renewed
          </Badge>
        ) : null}
      </>
    ),
  }));
  const refused = (results ?? []).filter((r) => !r.invitation);
  return (
    <Dialog open={results != null} title="Send these links" description={`Each opens lux, signs the person in through Cloudflare Access, and joins them to ${tenantName} with the role beside it. Valid 14 days.`} confirmLabel="Done" cancelLabel="Close" width={520} onConfirm={onDone} onCancel={onDone}>
      {links.length > 0 ? <InviteLinkList links={links} /> : <p className="muted">No links: nobody was invited.</p>}
      {refused.length > 0 && (
        <ul className="emailinput-left" aria-label="Not invited">
          {refused.map((r) => (
            <li key={r.email}>
              <span className="emailinput-left-addr">{r.email}</span>
              <span className="emailinput-left-why">{r.message ?? (r.result === "member" ? `already in ${tenantName}` : "not an email")}</span>
            </li>
          ))}
        </ul>
      )}
      {links.length > 1 && (
        <div className="row">
          <CopyAllLinks links={links} />
        </div>
      )}
    </Dialog>
  );
}
