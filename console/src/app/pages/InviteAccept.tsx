import { useState } from "react";
import { Button, RelativeTime, ROLE_DESC, ROLE_LABEL } from "@lux/design-system";
import { api, errorText, isApiError, setWhoAmI, signOut, useQuery, useSession } from "../../api/index.ts";
import { AuthScreen } from "../SignIn.tsx";
import { go } from "../router.tsx";

function SignOutButton() {
  const s = useSession();
  return <Button variant="ghost" onClick={() => s.user ? window.location.assign("/cdn-cgi/access/logout") : signOut()}>Sign out</Button>;
}

/**
 * After accepting: whoami as the joined tenant (its role, the new
 * membership), then its Members page, scoped to it: not the tenant the
 * person was in before.
 */
export async function joined(tenantId: string) {
  setWhoAmI(await api.whoami(undefined, tenantId));
  // go keeps the scope query, except what the path sets: tenant here.
  go(`/members?tenant=${encodeURIComponent(tenantId)}`);
}

/** /invite/:code: reachable before any tenant membership, bare and behind Access. */
export function InviteAccept({ code }: { code: string }) {
  const s = useSession();
  // luxd says a code is unusable with 410 invitation_unusable; anything else that fails (the network, a 5xx) is not the invitation's fault: retry.
  const q = useQuery(`invite:${code}`, (sig) =>
    api.invitationByCode(code, sig).catch((e) => {
      if (isApiError(e) && (e.status === 410 || e.status === 404)) return { unusable: true as const };
      throw e;
    }),
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Once accept answers, the membership exists: keep it. A failure after
  // that (whoami, navigation) retries only those, never accept again (a
  // second accept is 410 invitation_unusable).
  const [accepted, setAccepted] = useState<{ tenantId: string; tenant: string } | null>(null);
  const land = (r: { tenantId: string; tenant: string }) => {
    setBusy(true);
    setError(null);
    void joined(r.tenantId)
      .catch((e) => setError(errorText(e)))
      .finally(() => setBusy(false));
  };
  const join = () => {
    setBusy(true);
    setError(null);
    void api
      .acceptInvitation(code)
      .then((r) => {
        setAccepted({ tenantId: r.tenantId, tenant: r.tenant });
        return joined(r.tenantId);
      })
      .catch((e) => setError(errorText(e)))
      .finally(() => setBusy(false));
  };
  const i = q.data && !("unusable" in q.data) ? q.data : undefined;
  const unavailable = s.consoleAuth === "key";
  const unusable = (q.data != null && "unusable" in q.data) || (i && i.status !== "pending");
  const failed = q.error != null && q.data == null;
  return (
    <AuthScreen>
      {accepted ? <>
        <h2 className="dialog-title">You joined {accepted.tenant}</h2>
        {error ? <>
          <div className="error-strip" role="alert">You joined {accepted.tenant}. Couldn't load it yet: <Button variant="ghost" size="sm" loading={busy} onClick={() => land(accepted)}>Retry</Button></div>
          <p className="secondary">{error}</p>
        </> : <p className="secondary">Opening {accepted.tenant}…</p>}
      </> : unavailable ? <><h2 className="dialog-title">Invitations need Cloudflare Access</h2><p className="secondary">This console signs in with keys. Ask an operator to add your email directly.</p></> : failed ? <><h2 className="dialog-title">Couldn't check this invitation</h2><p className="secondary">{q.error} The link may still be fine.</p><div className="dialog-actions"><Button variant="primary" loading={q.loading} onClick={() => void q.refetch()}>Try again</Button></div></> : unusable ? <><h2 className="dialog-title">This invitation can't be used</h2><p className="secondary">It expired, was revoked or was already accepted. Ask whoever sent it for a new link; renewing keeps the same one.</p><Button onClick={() => go("/")}>Go to lux</Button></> : i && !i.emailMatches ? <><h2 className="dialog-title">This invitation is for someone else</h2><p className="secondary">You're signed in as <b>{s.user?.email ?? s.whoami?.email}</b>. The invitation was sent to a different email. Sign out and sign in with the invited one.</p><SignOutButton /></> : i ? <>
        <h2 className="dialog-title">Join {i.tenant}</h2>
        <p className="secondary">{i.invitedBy?.name ?? "An admin"} invited you as <b>{i.role ? ROLE_LABEL[i.role] : "a member"}</b>. {i.role ? ROLE_DESC[i.role] : ""}</p>
        <div className="signin-next">Signed in as <b>{s.user?.email ?? s.whoami?.email}</b> · expires <RelativeTime at={i.expiresAt} /></div>
        {error && <div className="error-strip" role="alert">{error}</div>}
        <div className="dialog-actions"><Button variant="ghost" onClick={() => go("/")}>Not now</Button><Button variant="primary" loading={busy} onClick={join}>Join {i.tenant}</Button></div>
      </> : <p className="secondary">Checking the invitation…</p>}
    </AuthScreen>
  );
}

/** Signed in, no tenant membership: pending offers for exactly this email. */
export function NoTenant() {
  const s = useSession();
  const q = useQuery("me:invitations", (sig) => api.myInvitations(sig), { enabled: s.consoleAuth === "cloudflare-access" });
  return <AuthScreen><h2 className="dialog-title">You're not part of any tenant yet</h2><p className="secondary">Signed in as <b>{s.user?.email ?? s.whoami?.email}</b>. Contact your admin to invite this email.</p>{(q.data ?? []).map((i) => <div className="inv-offer" key={i.code}><span className="person-text"><span className="person-name">{i.tenant} · {ROLE_LABEL[i.role]}</span><span className="person-sub">from {i.invitedBy?.name ?? "an admin"} · expires <RelativeTime at={i.expiresAt} /></span></span><Button variant="primary" size="sm" onClick={() => go(`/invite/${encodeURIComponent(i.code)}`)}>Join</Button></div>)}<div className="dialog-actions"><SignOutButton /></div></AuthScreen>;
}
