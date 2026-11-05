// Who the console is signed in as, from whoami: operator, role in the
// tenant in scope, the person or service account, memberships, and
// whether invitations exist. Every field the people model added is
// optional, so a console in front of an older luxd falls back to scopes.
import { roleAtLeast, type TenantRole } from "@lux/design-system";
import { useSession, type Owner, type Run, type ServiceAccount, type WhoAmI } from "../api/index.ts";
import { useScope } from "./scope.tsx";

export interface Viewer {
  operator: boolean;
  /** The role in the tenant in scope; null when unknown (an older luxd: see legacy). */
  role: TenantRole | null;
  /** whoami has answered. Until then nobody may change anything: every mutating control is hidden or disabled. */
  resolved: boolean;
  /** whoami answered without people (no role, userId, memberships, service account): an older luxd; decide from scopes, as before people. */
  legacy: boolean;
  userId?: string;
  serviceAccountId?: string;
  email?: string;
  name?: string;
  memberships: NonNullable<WhoAmI["memberships"]>;
  invitationsEnabled: boolean;
  scopes: string[];
  /** Signed in, no membership, not an operator. */
  noTenant: boolean;
}

export function viewerOf(me: WhoAmI | null, tenant?: string): Viewer {
  const memberships = me?.memberships ?? [];
  const inScope = tenant ? memberships.find((m) => m.tenantId === tenant || m.tenant === tenant) : undefined;
  const role = inScope?.role ?? me?.role ?? null;
  return {
    operator: me?.operator ?? false,
    role,
    resolved: me != null,
    legacy: me != null && me.role == null && me.userId == null && me.memberships == null && me.serviceAccount == null,
    userId: me?.userId,
    serviceAccountId: me?.serviceAccount?.id,
    email: me?.email,
    name: me?.name,
    memberships,
    invitationsEnabled: me?.invitationsEnabled === true && me.consoleAuth === "cloudflare-access",
    scopes: me?.scopes ?? [],
    noTenant: me != null && !me.operator && !me.serviceAccount && me.memberships != null && me.memberships.length === 0,
  };
}

export function useViewer(): Viewer {
  const { whoami } = useSession();
  const { apiTenant } = useScope();
  return viewerOf(whoami, apiTenant);
}

/** Operators count as every role; an older luxd's key by its scopes; before whoami answers, reading only. */
export function atLeast(v: Viewer, r: TenantRole): boolean {
  if (!v.resolved) return r === "viewer";
  if (v.operator) return true;
  if (v.legacy) return r === "viewer" ? true : r === "member" ? v.scopes.includes("run") || v.scopes.includes("admin") : v.scopes.includes("admin");
  return roleAtLeast(v.role, r);
}

/** I own it: it is me, my service account, or a service account I own. */
export function ownsOwner(v: Viewer, o: Owner | undefined, sas: readonly Pick<ServiceAccount, "id" | "owner">[] = []): boolean {
  if (!o) return false;
  if (o.kind === "user") return v.userId != null && o.id === v.userId;
  if (v.serviceAccountId && o.id === v.serviceAccountId) return true;
  return v.userId != null && sas.some((s) => s.id === o.id && s.owner?.id === v.userId);
}

/**
 * Whether the viewer may change a Run (stop, input, terminal…): admins and
 * operators every Run; members their own and their service accounts';
 * viewers none. An older luxd (no roles, no owners): the run scope.
 */
export function canActOnRun(v: Viewer, run: Pick<Run, "owner">, sas: readonly Pick<ServiceAccount, "id" | "owner">[] = []): boolean {
  if (!v.resolved) return false;
  if (v.operator) return true;
  if (v.legacy) return v.scopes.includes("run") || v.scopes.includes("admin");
  if (atLeast(v, "admin")) return true;
  return atLeast(v, "member") && ownsOwner(v, run.owner, sas);
}

/**
 * Tenant infrastructure (decisions 2–5): a tenant's pools, hosts (drain,
 * price, network) and unattached servers are its admins' and owners'; the
 * platform's, operators' only. Viewers and members change none of it.
 */
export function canManageInfra(v: Viewer, platform = false): boolean {
  if (!v.resolved) return false;
  if (v.operator) return true;
  return !platform && atLeast(v, "admin");
}

/** A tenant server: attached to a Run, as that Run (canActOnRun); attached to none, an admin. */
export function canActOnServer(v: Viewer, run: Pick<Run, "owner"> | null, sas: readonly Pick<ServiceAccount, "id" | "owner">[] = []): boolean {
  return run ? canActOnRun(v, run, sas) : canManageInfra(v);
}

/** "Carla Santos (owner of ci-review-bot)", "Bruno Costa", or null with no owner: who besides admins may act on a Run. */
export function whoCanAct(run: Pick<Run, "owner">, sas: readonly Pick<ServiceAccount, "id" | "name" | "owner">[] = []): string | null {
  const o = run.owner;
  if (!o) return null;
  if (o.kind === "user") return o.name || o.email || o.id;
  const sa = sas.find((s) => s.id === o.id);
  return sa?.owner ? `${sa.owner.name || sa.owner.email} (owner of ${o.name})` : null;
}
