// The people routes of the mock luxd (mockPeople.ts holds the data):
// /v1/me, /me/keys, /me/invitations, /members, /invitations (and by-code),
// /service-accounts, /people, /costs?by=member|owner, POST /tenants.
import { activeKeys, ago, atLeast, DAY, err, INVITES, invJson, invStatus, KEYS, keyJson, memberJson, MEMBERSHIPS, membershipsOf, ok, OWNER_COST, person, SAS, saJson, seq, TENANTS, tenantName, USERS, type Caller, type Inv, type Key, type MembershipRow, type Role, type SA } from "./mockPeople.ts";

let codeSeq = 0;
const newCode = () => `inv_${(Date.now() + ++codeSeq).toString(36).slice(-6)}${Math.random().toString(36).slice(2, 6)}`;
const ALPHABET = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789";
const secret = (prefix: string) => `${prefix}${Array.from({ length: 30 }, () => ALPHABET[Math.floor(Math.random() * ALPHABET.length)]).join("")}`;
const EMAIL_RE = /^[^\s<>@,;"]+@[^\s<>@,;".]+(\.[^\s<>@,;".]+)+$/;
const SA_NAME_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;
/* A key's expires: seconds from now (mockPeople.ts). */
const secondsUntil = (iso: string) => (Date.parse(iso) - Date.now()) / 1000;

type Res = { status: number; body: unknown } | null;

/**
 * The people routes. null: not one of them (mock.ts answers). A tenant
 * route's tenant is the caller's (an operator's ?tenant=, else their first
 * membership, else acme).
 */
export async function peopleApi(req: Request, u: URL, c: Caller | null, consoleAuth: string): Promise<Res> {
  const p = u.pathname;
  const method = req.method;
  const body = async <T>() => (method === "GET" || method === "DELETE" ? ({} as T) : ((await req.json().catch(() => ({}))) as T));
  const origin = u.origin;
  if (!c) return null;
  const tenantId = c.tenantId ?? MEMBERSHIPS.find((m) => m.userId === c.user?.id)?.tenantId ?? "ten_acme";
  const member = c.operator || c.sa != null || c.role != null;

  if (p === "/v1/me") {
    if (!c.user) return err(404, "not_found", "a service account has no profile");
    const ms = membershipsOf(c.user.id);
    return ok({ ...person(c.user.id), memberships: ms, lastSignInAt: ago(3 * 3600) });
  }
  if (p === "/v1/me/invitations") {
    const mine = INVITES.filter((i) => c.user && i.email === c.user.email && invStatus(i) === "pending");
    return ok({ invitations: mine.map((i) => ({ code: i.code, tenant: tenantName(i.tenantId), role: i.role, invitedBy: { name: person(i.by)?.name ?? "" }, createdAt: ago(i.created), expiresAt: ago(i.expires) })) });
  }
  const byCode = p.match(/^\/v1\/invitations\/by-code\/([^/]+)(\/accept)?$/);
  if (byCode) {
    if (consoleAuth !== "cloudflare-access") return err(409, "invitations_unavailable", "invitations need console.auth = cloudflare-access");
    const i = INVITES.find((x) => x.code === byCode[1]);
    if (!i) return err(410, "invitation_unusable", "this invitation can't be used");
    const st = invStatus(i);
    const matches = c.user?.email === i.email;
    const inviter = (by: string) => ({ name: person(by)?.name || person(by)?.email || "" });
    if (!byCode[2]) {
      if (!matches) return ok({ status: st, emailMatches: false });
      return ok({ tenant: tenantName(i.tenantId), role: i.role, invitedBy: inviter(i.by), expiresAt: ago(i.expires), status: st, emailMatches: true });
    }
    if (st !== "pending") return err(410, "invitation_unusable", "this invitation can't be used");
    if (!matches) return err(403, "invitation_email_mismatch", "this invitation is for someone else");
    i.accepted = 0;
    if (!MEMBERSHIPS.some((m) => m.tenantId === i.tenantId && m.userId === c.user!.id)) MEMBERSHIPS.push({ tenantId: i.tenantId, userId: c.user!.id, role: i.role, joined: 0 });
    return ok({ tenantId: i.tenantId, tenant: tenantName(i.tenantId), role: i.role });
  }
  if (p === "/v1/people") {
    if (!c.operator) return err(404, "not_found", "not found");
    return ok({
      people: USERS.filter((x) => x.operator || MEMBERSHIPS.some((m) => m.userId === x.id)).map((x) => ({
        ...person(x.id),
        memberships: membershipsOf(x.id),
        keyCount: KEYS.filter((k) => k.userId === x.id && activeKeys(k)).length,
        lastSeenAt: ago(x.lastSeen),
      })),
    });
  }

  const isTenantRoute = /^\/v1\/(me\/keys|members|invitations|service-accounts)(\/|$)/.test(p) || (p === "/v1/costs" && ["member", "owner"].includes(u.searchParams.get("by") ?? ""));
  if (!isTenantRoute) return null;
  if (!member) return err(403, "not_a_member", "You're not part of any tenant yet. Contact your admin to invite this email.");

  // ---- my keys
  if (p === "/v1/me/keys") {
    if (!c.user) return err(404, "not_found", "a service account has no personal keys");
    const mine = KEYS.filter((k) => k.userId === c.user!.id && k.tenantId === tenantId && activeKeys(k));
    if (method === "GET") return ok({ keys: mine.map(keyJson) });
    const b = await body<{ name?: string; readOnly?: boolean; expiresAt?: string }>();
    if (!b.name?.trim()) return err(400, "invalid", "a key needs a name");
    if (mine.length >= 5) return err(409, "key_limit", "5 active keys is the limit: revoke one first");
    const value = secret("lux_u_");
    const k: Key = { id: `key_u_${++seq.key}`, tenantId, userId: c.user.id, name: b.name.trim(), prefix: value.slice(0, 10), readOnly: !!b.readOnly, created: 0, ...(b.expiresAt ? { expires: secondsUntil(b.expiresAt) } : {}) };
    KEYS.push(k);
    return ok({ ...keyJson(k), apiKey: value }, 201);
  }
  const myKey = p.match(/^\/v1\/me\/keys\/([^/]+)$/);
  if (myKey) {
    const k = KEYS.find((x) => x.id === myKey[1] && x.userId === c.user?.id && activeKeys(x));
    if (!k) return err(404, "not_found", "key not found");
    if (method === "DELETE") {
      k.revoked = true;
      return ok(null, 204);
    }
    const b = await body<{ name?: string }>();
    if (b.name?.trim()) k.name = b.name.trim();
    return ok(keyJson(k));
  }

  // ---- members
  const inTenant = () => MEMBERSHIPS.filter((m) => m.tenantId === tenantId);
  if (p === "/v1/members") {
    if (method === "GET") return ok({ members: inTenant().map((m) => memberJson(m, c)) });
    if (!c.operator) return err(403, "forbidden", "only operators add members directly; admins invite");
    const b = await body<{ email: string; role: Role }>();
    let user = USERS.find((x) => x.email === b.email.toLowerCase());
    if (!user) USERS.push((user = { id: `usr_${++seq.key}`, email: b.email.toLowerCase(), name: "", operator: false, lastSeen: 0 }));
    const m: MembershipRow = { tenantId, userId: user.id, role: b.role, joined: 0 };
    MEMBERSHIPS.push(m);
    return ok(memberJson(m, c), 201);
  }
  const mm = p.match(/^\/v1\/members\/([^/]+)(?:\/keys(?:\/([^/]+))?)?$/);
  if (mm) {
    const target = inTenant().find((m) => m.userId === mm[1] || USERS.find((x) => x.id === m.userId)?.email === mm[1]);
    if (!target) return err(404, "not_found", "member not found");
    const self = target.userId === c.user?.id;
    if (p.includes("/keys")) {
      if (mm[2] && method === "DELETE") {
        // members.go revokeMemberKey: admins+; an owner's keys owners and operators; 404 for a key not theirs here.
        if (!atLeast(c, "admin")) return err(403, "forbidden", "only admins and owners revoke a member's keys");
        if (target.role === "owner" && !atLeast(c, "owner")) return err(403, "forbidden", "only owners and operators revoke an owner's keys");
        const k = KEYS.find((x) => (x.id === mm[2] || x.prefix === mm[2]) && x.userId === target.userId && x.tenantId === tenantId && activeKeys(x));
        if (!k) return err(404, "not_found", `no active key ${mm[2]}`);
        k.revoked = true;
        return ok(null, 204);
      }
      if (!self && !atLeast(c, "admin")) return err(404, "not_found", "not found");
      return ok({ keys: KEYS.filter((k) => k.userId === target.userId && k.tenantId === tenantId && activeKeys(k)).map(keyJson) });
    }
    if (method === "GET") return ok(memberJson(target, c));
    if (!atLeast(c, "admin")) return err(403, "forbidden", "only admins and owners change members");
    const owners = inTenant().filter((m) => m.role === "owner");
    if (method === "DELETE") {
      if (target.role === "owner" && !atLeast(c, "owner")) return err(403, "forbidden", "only owners remove an Owner");
      if (target.role === "owner" && owners.length === 1 && !c.operator) return err(409, "last_owner", "the last owner cannot be removed");
      MEMBERSHIPS.splice(MEMBERSHIPS.indexOf(target), 1);
      for (const k of KEYS) if (k.userId === target.userId && k.tenantId === tenantId) k.revoked = true;
      return ok(null, 204);
    }
    const b = await body<{ role: Role }>();
    if ((b.role === "owner" || target.role === "owner") && !atLeast(c, "owner")) return err(403, "forbidden", "only owners grant or change Owner");
    if (target.role === "owner" && b.role !== "owner" && owners.length === 1 && !c.operator) return err(409, "last_owner", "the last owner cannot be demoted");
    target.role = b.role;
    return ok(memberJson(target, c));
  }

  // ---- invitations
  if (p.startsWith("/v1/invitations")) {
    if (consoleAuth !== "cloudflare-access") return err(409, "invitations_unavailable", "invitations need console.auth = cloudflare-access; operators add members with lux members add");
    if (!atLeast(c, "admin")) return err(403, "forbidden", "only admins and owners see invitations");
    const here = INVITES.filter((i) => i.tenantId === tenantId);
    if (p === "/v1/invitations/counts") {
      const n = { pending: 0, expired: 0, accepted: 0, revoked: 0 };
      for (const i of here) n[invStatus(i)]++;
      return ok(n);
    }
    if (p === "/v1/invitations" && method === "GET") {
      const st = u.searchParams.get("status");
      const q = u.searchParams.get("q")?.toLowerCase();
      return ok({ invitations: here.filter((i) => (!st || st === "all" || invStatus(i) === st) && (!q || i.email.includes(q))).sort((a, b) => a.created - b.created).map((i) => invJson(i, origin)) });
    }
    if (p === "/v1/invitations") {
      const b = await body<{ invitations: { email: string; name?: string }[]; role: Role }>();
      if (b.role === "owner" && !atLeast(c, "owner")) return err(403, "forbidden", "only owners invite Owners");
      const results = (b.invitations ?? []).map(({ email: raw }) => {
        const email = raw.trim().toLowerCase();
        if (!EMAIL_RE.test(email)) return { email: raw, result: "invalid", message: "not an email" };
        const user = USERS.find((x) => x.email === email);
        if (user && inTenant().some((m) => m.userId === user.id)) return { email, result: "member", message: `already in ${tenantName(tenantId)}` };
        const live = here.find((i) => i.email === email && (invStatus(i) === "pending" || invStatus(i) === "expired"));
        if (live && invStatus(live) === "pending") return { email, result: "existing", invitation: invJson(live, origin) };
        if (live) {
          live.expires = -14 * DAY;
          live.renewed = 0;
          return { email, result: "renewed", invitation: invJson(live, origin) };
        }
        const i: Inv = { id: `inv_${++seq.inv}`, tenantId, email, role: b.role, code: newCode(), by: c.user?.id ?? "usr_mar", created: 0, expires: -14 * DAY };
        INVITES.push(i);
        return { email, result: "created", invitation: invJson(i, origin) };
      });
      return ok({ results });
    }
    const act = p.match(/^\/v1\/invitations\/([^/]+)\/(renew|revoke)$/);
    if (act) {
      const i = here.find((x) => x.id === act[1]);
      if (!i) return err(404, "not_found", "invitation not found");
      if (i.role === "owner" && !atLeast(c, "owner")) return err(403, "forbidden", "only owners manage Owner invitations");
      if (act[2] === "renew") {
        i.expires = -14 * DAY;
        i.renewed = 0;
      } else {
        i.revoked = 0;
        i.revokedBy = c.user?.id;
      }
      return ok(invJson(i, origin));
    }
    return err(404, "not_found", "not found");
  }

  // ---- service accounts (serviceaccounts.go: create/patch admin+, ownerEmail a member's email, "" clears)
  const ownsSA = (s: SA) => s.ownerId != null && s.ownerId === c.user?.id;
  const SA_ROLES = ["viewer", "member", "admin"];
  /** A member of the tenant by email (any case), or the server's 422. */
  const ownerByEmail = (email: string): { id: string } | { error: Res } => {
    const u = USERS.find((x) => x.email.toLowerCase() === email.toLowerCase() && inTenant().some((m) => m.userId === x.id));
    return u ? { id: u.id } : { error: err(422, "invalid_request", `${email} is not a member of this tenant`) };
  };
  if (p === "/v1/service-accounts") {
    if (method === "GET") return ok({ serviceAccounts: SAS.filter((s) => s.tenantId === tenantId).sort((a, b) => a.name.localeCompare(b.name)).map(saJson) });
    if (!atLeast(c, "admin")) return err(403, "forbidden", "this needs the admin scope");
    const b = await body<{ name: string; description?: string; role: SA["role"]; ownerEmail?: string }>();
    if (!SA_NAME_RE.test(b.name ?? "")) return err(422, "invalid_request", `name ${JSON.stringify(b.name ?? "")}: lowercase letters, digits and dashes, starting and ending with a letter or digit, at most 63`);
    if (!SA_ROLES.includes(b.role)) return err(422, "invalid_request", "role: viewer, member or admin");
    let ownerId: string | undefined;
    if (b.ownerEmail) {
      const o = ownerByEmail(b.ownerEmail);
      if ("error" in o) return o.error;
      ownerId = o.id;
    }
    if (SAS.some((s) => s.tenantId === tenantId && s.name === b.name)) return err(409, "conflict", `a service account ${b.name} exists in this tenant`);
    const s: SA = { id: `sa_${(++seq.key).toString(36)}x${Math.random().toString(36).slice(2, 7)}`, tenantId, name: b.name, description: b.description ?? "", role: b.role, ownerId, created: 0, createdBy: c.user?.id };
    SAS.push(s);
    return ok(saJson(s), 201);
  }
  const sm = p.match(/^\/v1\/service-accounts\/([^/]+)(?:\/keys(?:\/([^/]+))?)?$/);
  if (sm) {
    const s = SAS.find((x) => x.tenantId === tenantId && (x.id === sm[1] || x.name === sm[1]));
    if (!s) return err(404, "not_found", `no service account ${sm[1]}`);
    if (p.includes("/keys")) {
      // Its owner and admins only; anyone else is told it is not there (accountKeyHolder).
      if (!atLeast(c, "admin") && !ownsSA(s)) return err(404, "not_found", `no service account ${sm[1]}`);
      const ks = KEYS.filter((k) => k.saId === s.id && activeKeys(k));
      if (method === "GET") return ok({ keys: ks.map(keyJson) });
      if (method === "DELETE") {
        const k = ks.find((x) => x.id === sm[2] || x.prefix === sm[2]);
        if (!k) return err(404, "not_found", "key not found");
        k.revoked = true;
        return ok(null, 204);
      }
      if (ks.length >= 2) return err(409, "key_limit", "2 active keys is the limit: revoke the old one first");
      const b = await body<{ name?: string; readOnly?: boolean; expiresAt?: string }>();
      if (!b.name?.trim()) return err(422, "invalid_request", "a key needs a name");
      const value = secret("lux_s_");
      const k: Key = { id: `key_s_${++seq.key}`, tenantId, saId: s.id, name: b.name.trim(), prefix: value.slice(0, 10), readOnly: !!b.readOnly, created: 0, createdBy: c.user?.id, ...(b.expiresAt ? { expires: secondsUntil(b.expiresAt) } : {}) };
      KEYS.push(k);
      return ok({ ...keyJson(k), apiKey: value }, 201);
    }
    if (method === "GET") return ok(saJson(s));
    if (!atLeast(c, "admin")) return err(403, "forbidden", "this needs the admin scope");
    const b = await body<{ description?: string; role?: SA["role"]; ownerEmail?: string; disabled?: boolean }>();
    if (b.role != null && !SA_ROLES.includes(b.role)) return err(422, "invalid_request", "role: viewer, member or admin");
    let ownerId = s.ownerId;
    if (b.ownerEmail != null) {
      if (b.ownerEmail === "") ownerId = undefined;
      else {
        const o = ownerByEmail(b.ownerEmail);
        if ("error" in o) return o.error;
        ownerId = o.id;
      }
    }
    if (b.description != null) s.description = b.description;
    if (b.role) s.role = b.role;
    s.ownerId = ownerId;
    if (b.disabled != null) s.disabled = b.disabled ? (s.disabled ?? 0) : undefined;
    return ok(saJson(s));
  }

  // ---- cost per member / owner
  if (p === "/v1/costs") return ok(costByOwner(u.searchParams.get("by") === "owner", u.searchParams.get("runs") === "true"));
  return null;
}

/**
 * GET /v1/costs?by=member|owner (costread.go): a CostSummary whose totals
 * are keyed user:<id> | sa:<id> | (none), with group {by: key}, owner (not
 * on (none)), and runs only with runs=true. Only owners with cost have a
 * row; by owner, service accounts fold into their owner.
 */
export function costByOwner(byOwner: boolean, withRuns = false) {
  const by = byOwner ? "owner" : "member";
  const rows = new Map<string, { owner?: unknown; cents: number; runs: number }>();
  const add = (key: string, owner: unknown, cents: number, runs: number) => {
    const r = rows.get(key) ?? { owner, cents: 0, runs: 0 };
    r.cents += cents;
    r.runs += runs;
    rows.set(key, r);
  };
  const userOwner = (id: string) => {
    const x = USERS.find((y) => y.id === id)!;
    return { kind: "user", id: x.id, name: x.name || x.email, email: x.email };
  };
  for (const [key, v] of Object.entries(OWNER_COST)) {
    const cents = Math.round(Number(v.amount) * 100);
    if (key === "(none)") add(key, undefined, cents, v.runs);
    else if (key.startsWith("user:")) add(key, userOwner(key.slice(5)), cents, v.runs);
    else {
      const s = SAS.find((x) => x.id === key.slice(3))!;
      if (byOwner && s.ownerId) add(`user:${s.ownerId}`, userOwner(s.ownerId), cents, v.runs);
      else add(key, { kind: "serviceAccount", id: s.id, name: s.name }, cents, v.runs);
    }
  }
  return {
    from: ago(30 * DAY),
    to: ago(0),
    basis: "list",
    totals: [...rows]
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([key, r]) => ({ key, ...(r.owner ? { owner: r.owner } : {}), group: { [by]: key }, currency: "USD", amount: (r.cents / 100).toFixed(2), ...(withRuns ? { runs: r.runs } : {}) })),
  };
}

/** GET /v1/tenants' people columns (status.go): owners' names (else email) by email, and the counts. */
export function tenantPeople(id: string) {
  const owners = MEMBERSHIPS.filter((m) => m.tenantId === id && m.role === "owner")
    .map((m) => USERS.find((u) => u.id === m.userId)!)
    .sort((a, b) => a.email.localeCompare(b.email));
  return { ownersCount: owners.length, owners: owners.map((u) => u.name || u.email), membersCount: MEMBERSHIPS.filter((m) => m.tenantId === id).length };
}

/** POST /v1/tenants (invitation_accept.go): {id, tenantId, name}, and its first owner's invitation when asked. */
export function createTenant(name: string, ownerEmail: string | undefined, c: Caller, origin: string) {
  if (!name.trim()) return err(422, "invalid_request", "name is required");
  if (TENANTS.some((t) => t.name === name)) return err(409, "conflict", `tenant ${name} exists`);
  const t = { id: `ten_${name}`, name };
  TENANTS.push(t);
  let invitation: unknown;
  if (ownerEmail) {
    const i: Inv = { id: `inv_${++seq.inv}`, tenantId: t.id, email: ownerEmail.toLowerCase(), role: "owner", code: newCode(), by: c.user?.id ?? "usr_mar", created: 0, expires: -14 * DAY };
    INVITES.push(i);
    invitation = invJson(i, origin);
  }
  return ok({ id: t.id, tenantId: t.id, name: t.name, ...(invitation ? { invitation } : {}) }, 201);
}