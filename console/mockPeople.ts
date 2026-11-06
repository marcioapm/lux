// People for the mock luxd: persons, memberships, service accounts, keys,
// invitations, Run owners and cost per member, as GET/POST /v1/me,
// /members, /invitations, /service-accounts, /people and /costs?by=member
// answer them (the API contract of docs/openapi.yaml). Who is asking comes
// from the key: luxk_mock_<role> signs in as one fixed person per role
// (viewer, member, admin, owner, operator, notenant, invited, sa); in Access
// mode, MOCK_AS=<role>. Mutations change this module's state until restart.
//
// Nothing here is real.

export const ago = (s: number) => new Date(Date.now() - s * 1000).toISOString();
export const DAY = 86400;

export type Role = "viewer" | "member" | "admin" | "owner";
export const RANK: Record<Role, number> = { viewer: 0, member: 1, admin: 2, owner: 3 };

export interface User {
  id: string;
  email: string;
  name: string;
  picture?: string;
  operator: boolean;
  lastSeen: number;
  costs?: number;
}
export const USERS: User[] = [
  { id: "usr_eva", name: "Eva Moreira", email: "eva@acme.dev", operator: false, lastSeen: 2 * 3600 },
  { id: "usr_mar", name: "Márcio Martins", email: "marcio@lux.dev", operator: true, lastSeen: 30 },
  { id: "usr_ana", name: "Ana Ribeiro", email: "ana@acme.dev", operator: false, lastSeen: 5 * 60 },
  { id: "usr_car", name: "Carla Santos", email: "carla@acme.dev", operator: false, lastSeen: 12 * 60 },
  { id: "usr_bru", name: "Bruno Costa", email: "bruno@acme.dev", operator: false, lastSeen: DAY },
  { id: "usr_dio", name: "Diogo Lopes", email: "diogo@acme.dev", operator: false, lastSeen: 3 * DAY },
  { id: "usr_fil", name: "Filipa Rocha", email: "filipa@acme.dev", operator: false, lastSeen: 60 },
  { id: "usr_sam", name: "Sam Lee", email: "sam@globex.io", operator: false, lastSeen: 60 },
  { id: "usr_rui", name: "Rui Alves", email: "rui@acme.dev", operator: false, lastSeen: 600 },
];

export interface MembershipRow {
  tenantId: string;
  userId: string;
  role: Role;
  joined: number;
}
export const MEMBERSHIPS: MembershipRow[] = [
  { tenantId: "ten_acme", userId: "usr_eva", role: "owner", joined: 247 * DAY },
  { tenantId: "ten_acme", userId: "usr_mar", role: "owner", joined: 297 * DAY },
  { tenantId: "ten_acme", userId: "usr_ana", role: "admin", joined: 247 * DAY },
  { tenantId: "ten_acme", userId: "usr_car", role: "member", joined: 201 * DAY },
  { tenantId: "ten_acme", userId: "usr_bru", role: "member", joined: 149 * DAY },
  { tenantId: "ten_acme", userId: "usr_dio", role: "viewer", joined: 45 * DAY },
  { tenantId: "ten_globex", userId: "usr_mar", role: "admin", joined: 258 * DAY },
  // Rui: a non-operator in two tenants (viewer in globex, admin in initech).
  { tenantId: "ten_globex", userId: "usr_rui", role: "viewer", joined: 20 * DAY },
  { tenantId: "ten_initech", userId: "usr_rui", role: "admin", joined: 10 * DAY },
];

export const TENANTS = [
  { id: "ten_acme", name: "acme" },
  { id: "ten_globex", name: "globex" },
  { id: "ten_initech", name: "initech" },
];
export const tenantName = (id: string) => TENANTS.find((t) => t.id === id)?.name ?? id;

/** The person each mock role signs in as. */
export const AS: Record<string, string | null> = {
  viewer: "usr_dio",
  member: "usr_car",
  admin: "usr_ana",
  owner: "usr_eva",
  operator: "usr_mar",
  notenant: "usr_sam",
  invited: "usr_fil",
  sa: null,
  bruno: "usr_bru",
  multi: "usr_rui",
  // Read-only keys (whoami readOnly): an admin's and an operator's.
  readonly: "usr_ana",
  opreadonly: "usr_mar",
};

/** The mock roles that sign in with a read-only key: they read as their role and change nothing (luxd: 403 on every write). */
export const READ_ONLY_AS = new Set(["readonly", "opreadonly"]);

export interface SA {
  id: string;
  tenantId: string;
  name: string;
  description: string;
  role: Exclude<Role, "owner">;
  ownerId?: string;
  created: number;
  createdBy?: string;
  lastUsed?: number;
  disabled?: number;
}
export const SAS: SA[] = [
  { id: "sa_4kq2v8m1", tenantId: "ten_acme", name: "gha-runners", description: "lux-gha JIT runners for acme/* workflows", role: "member", ownerId: "usr_eva", created: 178 * DAY, createdBy: "usr_eva", lastUsed: 20 },
  { id: "sa_7cr3b2x9", tenantId: "ten_acme", name: "ci-review-bot", description: "PR review agents", role: "member", ownerId: "usr_car", created: 98 * DAY, createdBy: "usr_ana", lastUsed: 4 * 60 },
  { id: "sa_2ne9v1q5", tenantId: "ten_acme", name: "nightly-evals", description: "Model eval sweep, 02:00 UTC", role: "member", ownerId: "usr_ana", created: 83 * DAY, createdBy: "usr_ana", lastUsed: 13 * 3600 },
  { id: "sa_9gr1d7k3", tenantId: "ten_acme", name: "grafana-reader", description: "Read-only, for the Grafana datasource", role: "viewer", ownerId: "usr_ana", created: 34 * DAY, createdBy: "usr_ana", lastUsed: 60 },
  { id: "sa_5dd0p2x8", tenantId: "ten_acme", name: "dude-prod", description: "Migrated from the API key dude-prod", role: "admin", created: 300 * DAY },
];

export interface Key {
  id: string;
  tenantId: string;
  userId?: string;
  saId?: string;
  name: string;
  prefix: string;
  readOnly: boolean;
  created: number;
  lastUsed?: number;
  expires?: number;
  createdBy?: string;
  revoked?: boolean;
}
export const seq = { key: 100, inv: 10 };
export const KEYS: Key[] = [
  { id: "key_u_dio1", tenantId: "ten_acme", userId: "usr_dio", name: "grafana-laptop", prefix: "lux_u_Qe7b", readOnly: true, created: 45 * DAY, lastUsed: 3 * DAY, expires: 320 * DAY },
  { id: "key_u_car1", tenantId: "ten_acme", userId: "usr_car", name: "laptop", prefix: "lux_u_7Hc2", readOnly: false, created: 201 * DAY, lastUsed: 12 * 60 },
  { id: "key_u_car2", tenantId: "ten_acme", userId: "usr_car", name: "dude-agent", prefix: "lux_u_k9Pw", readOnly: false, created: 94 * DAY, lastUsed: 3600, expires: 90 * DAY },
  { id: "key_u_ana1", tenantId: "ten_acme", userId: "usr_ana", name: "laptop", prefix: "lux_u_3mTz", readOnly: false, created: 247 * DAY, lastUsed: 5 * 60 },
  { id: "key_u_ana2", tenantId: "ten_acme", userId: "usr_ana", name: "ops-scripts", prefix: "lux_u_Vb81", readOnly: false, created: 169 * DAY, lastUsed: 2 * DAY },
  { id: "key_u_ana3", tenantId: "ten_acme", userId: "usr_ana", name: "status-board", prefix: "lux_u_Lr0e", readOnly: true, created: 127 * DAY, lastUsed: 60 },
  { id: "key_u_eva1", tenantId: "ten_acme", userId: "usr_eva", name: "laptop", prefix: "lux_u_N2sd", readOnly: false, created: 247 * DAY, lastUsed: 2 * 3600 },
  { id: "key_u_bru1", tenantId: "ten_acme", userId: "usr_bru", name: "laptop", prefix: "lux_u_Bx41", readOnly: false, created: 149 * DAY, lastUsed: DAY },
  { id: "key_u_mar1", tenantId: "ten_acme", userId: "usr_mar", name: "xps", prefix: "lux_u_Zp4a", readOnly: false, created: 297 * DAY, lastUsed: 30 },
  { id: "key_u_mar2", tenantId: "ten_acme", userId: "usr_mar", name: "threadctl", prefix: "lux_u_Gq7x", readOnly: false, created: 10 * DAY, lastUsed: 120 },
  { id: "key_s_ci1", tenantId: "ten_acme", saId: "sa_7cr3b2x9", name: "ci-2026-07", prefix: "lux_s_Rt5m", readOnly: false, created: 98 * DAY, lastUsed: 4 * 60, createdBy: "usr_car" },
  { id: "key_s_gha1", tenantId: "ten_acme", saId: "sa_4kq2v8m1", name: "gha-2026-05", prefix: "lux_s_8bYc", readOnly: false, created: 178 * DAY, lastUsed: 6 * 3600, expires: 6 * DAY, createdBy: "usr_eva" },
  { id: "key_s_gha2", tenantId: "ten_acme", saId: "sa_4kq2v8m1", name: "gha-2026-11", prefix: "lux_s_Wf3k", readOnly: false, created: DAY, lastUsed: 20, expires: 180 * DAY, createdBy: "usr_eva" },
  { id: "key_s_ne1", tenantId: "ten_acme", saId: "sa_2ne9v1q5", name: "evals", prefix: "lux_s_Ne2v", readOnly: false, created: 83 * DAY, lastUsed: 13 * 3600, createdBy: "usr_ana" },
  { id: "key_s_gr1", tenantId: "ten_acme", saId: "sa_9gr1d7k3", name: "grafana", prefix: "lux_s_Gr9d", readOnly: false, created: 34 * DAY, lastUsed: 60, createdBy: "usr_ana" },
  { id: "key_dude", tenantId: "ten_acme", saId: "sa_5dd0p2x8", name: "dude-prod", prefix: "lux_Dd0p", readOnly: false, created: 300 * DAY, lastUsed: 300 },
];

export interface Inv {
  id: string;
  tenantId: string;
  email: string;
  role: Role;
  code: string;
  by: string;
  created: number;
  expires: number;
  accepted?: number;
  revoked?: number;
  revokedBy?: string;
  renewed?: number;
}

export const INVITES: Inv[] = [
  { id: "inv_01", tenantId: "ten_acme", email: "filipa@acme.dev", role: "member", by: "usr_ana", created: 2 * 3600, expires: -(14 * DAY - 2 * 3600), code: "inv_7Rk2Qm9xPa" },
  { id: "inv_02", tenantId: "ten_acme", email: "joao.p@acme.dev", role: "viewer", by: "usr_eva", created: 3 * DAY, expires: -11 * DAY, code: "inv_Hc4Ws0Lp2e" },
  { id: "inv_03", tenantId: "ten_acme", email: "rita@acme.dev", role: "admin", by: "usr_eva", created: 16 * DAY, expires: 2 * DAY, code: "inv_Zt8Yq3Vb6n" },
  { id: "inv_04", tenantId: "ten_acme", email: "nuno@acme.dev", role: "member", by: "usr_ana", created: 20 * DAY, expires: 6 * DAY, code: "inv_Pp1Mm5Kk7j", accepted: 19 * DAY },
  { id: "inv_05", tenantId: "ten_acme", email: "temp@contractor.io", role: "member", by: "usr_ana", created: 30 * DAY, expires: 16 * DAY, code: "inv_Qq9Ff2Dd4s", revoked: 25 * DAY, revokedBy: "usr_eva" },
  { id: "inv_06", tenantId: "ten_initech", email: "cto@initech.dev", role: "owner", by: "usr_mar", created: 5 * DAY, expires: -9 * DAY, code: "inv_Ic7Tt2Cc0o" },
];

/** n more pending invitations to acme (MOCK_INVITES=<n>: enough for the Invitations list to page, 100 at a time). */
export function seedInvitations(n: number) {
  for (let k = 0; k < n; k++) {
    const id = `inv_x${String(k).padStart(3, "0")}`;
    if (INVITES.some((i) => i.id === id)) continue;
    INVITES.push({ id, tenantId: "ten_acme", email: `guest${k}@contractor.io`, role: "viewer", by: "usr_ana", created: k * 600, expires: -(14 * DAY - k * 600), code: `inv_X${k.toString(36)}q${k}` });
  }
}

const INITIAL = structuredClone({ USERS, MEMBERSHIPS, TENANTS, SAS, KEYS, INVITES });
/** Back to the fixtures (tests change them). */
export function resetPeople() {
  const fresh = structuredClone(INITIAL);
  for (const [arr, init] of [
    [USERS, fresh.USERS],
    [MEMBERSHIPS, fresh.MEMBERSHIPS],
    [TENANTS, fresh.TENANTS],
    [SAS, fresh.SAS],
    [KEYS, fresh.KEYS],
    [INVITES, fresh.INVITES],
  ] as [unknown[], unknown[]][])
    arr.splice(0, arr.length, ...init);
}

/** Run owners, by Run id or name (mock.ts's and mockSessions.ts's Runs). */
const RUN_OWNERS: Record<string, { user?: string; sa?: string }> = {
  "agent-refactor-42": { sa: "sa_7cr3b2x9" },
  "billing-backoff-418": { user: "usr_bru" },
  "hosts-probe-claude": { user: "usr_car" },
  "hosts-probe-codex": { sa: "sa_2ne9v1q5" },
  "notes-count-acp": { user: "usr_ana" },
  "jervasion-pr-45": { sa: "sa_4kq2v8m1" },
};

/** Cost · 30d per owner (USD, list prices), Runs · 30d and CPU-hours. */
export const OWNER_COST: Record<string, { amount: string; runs: number; cpuh: number }> = {
  "user:usr_eva": { amount: "3.10", runs: 2, cpuh: 1.4 },
  "user:usr_mar": { amount: "18.22", runs: 9, cpuh: 22 },
  "user:usr_ana": { amount: "41.80", runs: 31, cpuh: 64.2 },
  "user:usr_car": { amount: "212.40", runs: 148, cpuh: 402.5 },
  "user:usr_bru": { amount: "96.12", runs: 57, cpuh: 180.3 },
  "sa:sa_4kq2v8m1": { amount: "1284.10", runs: 4210, cpuh: 9120.4 },
  "sa:sa_7cr3b2x9": { amount: "388.77", runs: 612, cpuh: 1310 },
  "sa:sa_2ne9v1q5": { amount: "142.05", runs: 90, cpuh: 455.2 },
  "sa:sa_5dd0p2x8": { amount: "77.40", runs: 41, cpuh: 98.1 },
  "(none)": { amount: "61.33", runs: 211, cpuh: 140.8 },
};

/** Who is calling, from the key (luxk_mock_<role>) or, in Access mode, MOCK_AS. */
export interface Caller {
  as: string;
  user: User | null;
  sa: SA | null;
  operator: boolean;
  /** The tenant the call is resolved to. */
  tenantId: string | null;
  role: Role | null;
  /** A read-only key: reads as role, changes nothing. */
  readOnly?: boolean;
}

export function callerOf(req: Request, url: URL, accessMode: boolean): Caller | null {
  const auth = req.headers.get("authorization") ?? "";
  const m = /^Bearer luxk_mock_([a-z]+)$/.exec(auth);
  const cookie = /(?:^|;\s*)lux_mock_as=([a-z]+)/.exec(req.headers.get("cookie") ?? "")?.[1];
  const as = m ? m[1]! : accessMode ? (cookie ?? process.env.MOCK_AS ?? "operator") : null;
  if (!as || !(as in AS)) return null;
  if (as === "sa") {
    const sa = SAS.find((s) => s.name === "ci-review-bot")!;
    return { as, user: null, sa, operator: false, tenantId: sa.tenantId, role: sa.role };
  }
  const user = USERS.find((u) => u.id === AS[as])!;
  const mine = MEMBERSHIPS.filter((x) => x.userId === user.id);
  const asked = url.searchParams.get("tenant");
  const askedId = asked ? (TENANTS.find((t) => t.id === asked || t.name === asked)?.id ?? asked) : null;
  const ms = askedId ? mine.find((x) => x.tenantId === askedId) : mine.length >= 1 ? mine[0] : undefined;
  // An operator with no ?tenant= sees every tenant; for member pages the console narrows to one.
  const tenantId = askedId ?? (user.operator ? null : (ms?.tenantId ?? null));
  return { as, user, sa: null, operator: user.operator, tenantId, role: ms?.role ?? null, ...(READ_ONLY_AS.has(as) ? { readOnly: true } : {}) };
}

export const person = (id: string | undefined) => {
  const u = USERS.find((x) => x.id === id);
  // As me.go's Person: picture is always there ("" without one).
  return u ? { id: u.id, email: u.email, name: u.name, picture: u.picture ?? "", operator: u.operator } : undefined;
};
/** As luxd's Principal.atLeast: a read-only key holds nothing above viewer, operators' included. */
export const atLeast = (c: Caller, r: Role) => (c.readOnly && r !== "viewer" ? false : c.operator || (c.role != null && RANK[c.role] >= RANK[r]));
export const err = (status: number, code: string, message: string) => ({ status, body: { error: { code, message } } });
export const ok = (body: unknown, status = 200) => ({ status, body });

export function invStatus(i: Inv): "pending" | "expired" | "accepted" | "revoked" {
  if (i.accepted != null) return "accepted";
  if (i.revoked != null) return "revoked";
  return i.expires > 0 ? "expired" : "pending";
}
export function invJson(i: Inv, origin: string) {
  return {
    id: i.id,
    email: i.email,
    role: i.role,
    status: invStatus(i),
    code: i.code,
    url: `${origin}/invite/${i.code}`,
    invitedBy: person(i.by),
    createdAt: ago(i.created),
    expiresAt: ago(i.expires),
    ...(i.renewed != null ? { renewedAt: ago(i.renewed) } : {}),
    ...(i.accepted != null ? { acceptedAt: ago(i.accepted) } : {}),
    ...(i.revoked != null ? { revokedAt: ago(i.revoked), revokedBy: person(i.revokedBy) } : {}),
  };
}
export const activeKeys = (k: Key) => !k.revoked && (k.expires == null || k.expires > 0);
export function keyJson(k: Key) {
  return {
    id: k.id,
    name: k.name,
    prefix: k.prefix,
    readOnly: k.readOnly,
    createdAt: ago(k.created),
    ...(k.lastUsed != null ? { lastUsedAt: ago(k.lastUsed) } : {}),
    ...(k.expires != null ? { expiresAt: ago(-k.expires) } : {}),
    ...(k.createdBy ? { createdBy: person(k.createdBy) } : {}),
  };
}
export function saJson(s: SA) {
  return {
    id: s.id,
    name: s.name,
    description: s.description,
    role: s.role,
    ...(s.ownerId ? { owner: person(s.ownerId) } : {}),
    createdAt: ago(s.created),
    ...(s.createdBy ? { createdBy: person(s.createdBy) } : {}),
    ...(s.lastUsed != null ? { lastUsedAt: ago(s.lastUsed) } : {}),
    ...(s.disabled != null ? { disabledAt: ago(s.disabled) } : {}),
    activeKeys: KEYS.filter((k) => k.saId === s.id && activeKeys(k)).length,
  };
}
const RUNNING: Record<string, number> = { usr_car: 1, usr_bru: 1 };
export function memberJson(m: MembershipRow, c: Caller) {
  const u = USERS.find((x) => x.id === m.userId)!;
  const seesKeys = c.user?.id === u.id || atLeast(c, "admin");
  return {
    ...person(u.id)!,
    role: m.role,
    joinedAt: ago(m.joined),
    lastSeenAt: ago(u.lastSeen),
    runningRuns: RUNNING[u.id] ?? 0,
    ...(seesKeys ? { keyCount: KEYS.filter((k) => k.userId === u.id && k.tenantId === m.tenantId && activeKeys(k)).length } : {}),
  };
}

/** A Run's owner as the API shows it, by Run name. */
export function runOwner(name: string | undefined) {
  const o = name ? RUN_OWNERS[name] : undefined;
  if (o?.user) {
    const u = USERS.find((x) => x.id === o.user)!;
    return { kind: "user", id: u.id, name: u.name, email: u.email };
  }
  if (o?.sa) {
    const s = SAS.find((x) => x.id === o.sa)!;
    return { kind: "serviceAccount", id: s.id, name: s.name };
  }
  return undefined;
}

/** Whether c may act on a Run owned by o (owner, its SA's owner, the SA itself, admins+). */
export function canAct(c: Caller | null, name: string | undefined): boolean {
  if (!c) return true;
  if (atLeast(c, "admin")) return true;
  if (!atLeast(c, "member")) return false;
  const o = name ? RUN_OWNERS[name] : undefined;
  if (!o) return false;
  if (o.user && o.user === c.user?.id) return true;
  if (o.sa && (o.sa === c.sa?.id || SAS.find((s) => s.id === o.sa)?.ownerId === c.user?.id)) return true;
  return false;
}

/** GET /v1/runs' mine= and owner= (api.go listRuns): owner= is comma-separated, any of them; user:<id> / sa:<id> exactly, else a person's id or email (any case), or an account's id or name. */
export function ownerFilter(c: Caller | null, u: URL): (name: string | undefined) => boolean {
  const mine = u.searchParams.get("mine") === "true";
  const owners = (u.searchParams.get("owner") ?? "").split(",").map((x) => x.trim()).filter(Boolean);
  const matches = (o: { user?: string; sa?: string }, v: string) => {
    if (v.startsWith("user:")) return o.user === v.slice(5);
    if (v.startsWith("sa:")) return o.sa === v.slice(3);
    if (o.user) return o.user === v || USERS.find((x) => x.id === o.user)?.email.toLowerCase() === v.toLowerCase();
    return o.sa === v || SAS.find((x) => x.id === o.sa)?.name === v;
  };
  return (name) => {
    const o = name ? RUN_OWNERS[name] : undefined;
    if (mine) {
      if (!c || !o) return false;
      if (o.user && o.user !== c.user?.id) return false;
      if (o.sa && !(o.sa === c.sa?.id || SAS.find((s) => s.id === o.sa)?.ownerId === c.user?.id)) return false;
    }
    if (owners.length && !(o && owners.some((v) => matches(o, v)))) return false;
    return true;
  };
}

/** A person's memberships as whoami, /me and /people send them (userMemberships: by tenant name, with joinedAt). */
export function membershipsOf(userId: string) {
  return MEMBERSHIPS.filter((m) => m.userId === userId)
    .map((m) => ({ tenantId: m.tenantId, tenant: tenantName(m.tenantId), role: m.role, joinedAt: ago(m.joined) }))
    .sort((a, b) => a.tenant.localeCompare(b.tenant));
}

export function whoamiFor(c: Caller, consoleAuth: string, previewDomain: string | null) {
  const memberships = c.user ? membershipsOf(c.user.id) : [];
  const key = c.user ? KEYS.find((k) => k.userId === c.user!.id) : KEYS.find((k) => k.saId === c.sa?.id);
  return {
    operator: c.operator,
    tenant: c.tenantId ? tenantName(c.tenantId) : "",
    tenantId: c.tenantId ?? "",
    ...(consoleAuth === "key" && key ? { keyId: key.id } : {}),
    ...(c.user ? { userId: c.user.id, email: c.user.email, name: c.user.name, ...(c.user.picture ? { picture: c.user.picture } : {}) } : {}),
    // whoami.go: no role for an operator.
    ...(c.role && !c.operator ? { role: c.role } : {}),
    ...(c.sa ? { serviceAccount: { id: c.sa.id, name: c.sa.name } } : {}),
    ...(c.readOnly ? { readOnly: true } : {}),
    memberships,
    scopes: atLeast(c, "admin") ? ["read", "run", "admin"] : atLeast(c, "member") ? ["read", "run"] : c.role || c.operator ? ["read"] : [],
    consoleAuth,
    invitationsEnabled: consoleAuth === "cloudflare-access",
    previews: previewDomain != null,
    serverInject: true,
    previewDomain,
  };
}
