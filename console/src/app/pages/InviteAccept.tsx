import { useState } from "react";
import { Button, RelativeTime, ROLE_DESC, ROLE_LABEL } from "@lux/design-system";
import { api, errorText, setWhoAmI, signOut, useQuery, useSession } from "../../api/index.ts";
import { AuthScreen } from "../SignIn.tsx";
import { go } from "../router.tsx";

function SignOutButton() {
  const s = useSession();
  return <Button variant="ghost" onClick={() => s.user ? window.location.assign("/cdn-cgi/access/logout") : signOut()}>Sign out</Button>;
}

/** /invite/:code: reachable before any tenant membership, bare and behind Access. */
export function InviteAccept({ code }: { code: string }) {
  const s = useSession();
  const q = useQuery(`invite:${code}`, (sig) => api.invitationByCode(code, sig));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const i = q.data;
  const unavailable = s.consoleAuth === "key";
  const unusable = !!q.error || (i && i.status !== "pending");
  return (
    <AuthScreen>
      {unavailable ? <><h2 className="dialog-title">Invitations need Cloudflare Access</h2><p className="secondary">This console signs in with keys. Ask an operator to add your email directly.</p></> : unusable ? <><h2 className="dialog-title">This invitation can't be used</h2><p className="secondary">It expired, was revoked or was already accepted. Ask whoever sent it for a new link; renewing keeps the same one.</p><Button onClick={() => go("/")}>Go to lux</Button></> : i && !i.emailMatches ? <><h2 className="dialog-title">This invitation is for someone else</h2><p className="secondary">You're signed in as <b>{s.user?.email ?? s.whoami?.email}</b>. The invitation was sent to a different email. Sign out and sign in with the invited one.</p><SignOutButton /></> : i ? <>
        <h2 className="dialog-title">Join {i.tenant}</h2>
        <p className="secondary">{i.invitedBy?.name ?? "An admin"} invited you as <b>{i.role ? ROLE_LABEL[i.role] : "a member"}</b>. {i.role ? ROLE_DESC[i.role] : ""}</p>
        <div className="signin-next">Signed in as <b>{s.user?.email ?? s.whoami?.email}</b> · expires <RelativeTime at={i.expiresAt} /></div>
        {error && <div className="error-strip" role="alert">{error}</div>}
        <div className="dialog-actions"><Button variant="ghost" onClick={() => go("/")}>Not now</Button><Button variant="primary" loading={busy} onClick={() => { setBusy(true); void api.acceptInvitation(code).then(async () => { setWhoAmI(await api.whoami()); go("/members"); }).catch((e) => setError(errorText(e))).finally(() => setBusy(false)); }}>Join {i.tenant}</Button></div>
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
