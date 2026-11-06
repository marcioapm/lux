// Typed calls, one per endpoint. Lists are unwrapped from their envelope.
import { download, request } from "./client.ts";
import type { AcceptedInvitation, ApiKey, Artifact, CostByOwner, CostLabels, CostSummary, CostSummaryParams, CreatedApiKey, Event, History, Host, HostCost, HostListParams, HostSummary, Invitation, InvitationCounts, InvitationLookup, InviteResult, LifecycleEvent, Me, Member, MigrateRequest, MyInvitation, NewKeyRequest, NewTenantResult, Page, PageParams, PlatformPerson, Pool, PoolCost, PoolMetrics, PoolStats, ResumeRequest, Run, RunCost, RunListParams, Server, ServerInput, ServerLogLine, ServiceAccount, TenantRole, TenantServer, TenantServerList, Snapshot, Status, StreamTicket, Tenant, WhoAmI } from "./types.ts";

type Sig = AbortSignal | undefined;
/** Tenant scope of a list call: a tenant id or name, or undefined for all the key sees. */
type Scope = string | undefined;

export const api = {
  /** tenant: a person in several tenants resolves to that membership (role, tenant). */
  whoami: (signal?: Sig, tenant?: Scope) => request<WhoAmI>("/whoami", { signal, tenant }),
  // Check a key without changing the session.
  whoamiAs: (key: string, signal?: Sig) => request<WhoAmI>("/whoami", { headers: { Authorization: `Bearer ${key}` }, signal }),
  status: (tenant: Scope, signal?: Sig) => request<Status>("/status", { tenant, signal }),
  history: (tenant: Scope, since: string, signal?: Sig, res?: number) => request<History>("/history", { tenant, query: { since, res }, signal }),

  runs: (tenant: Scope, p: RunListParams, signal?: Sig) =>
    request<{ runs: Run[] }>("/runs", { tenant, query: { state: p.state?.join(","), resumable: p.resumable, host: p.host, label: p.label, before: p.before, limit: p.limit }, signal }).then((r) => r.runs),
  /** A page of Runs in a sort's order (server-side, across every match). */
  runsPage: (tenant: Scope, p: RunListParams, signal?: Sig) =>
    request<{ runs: Run[] } & PageLinks>("/runs", { tenant, query: { state: p.state?.join(","), resumable: p.resumable, host: p.host, label: p.label, owner: p.owner, mine: p.mine, limit: p.limit, ...pageQuery(p) }, signal }).then((r) => toPage(r.runs, r)),
  run: (id: string, signal?: Sig) => request<Run>(`/runs/${enc(id)}`, { signal }),
  runHistory: (id: string, since: string, signal?: Sig) => request<History>(`/runs/${enc(id)}/history`, { query: { since }, signal }),
  runEvents: (id: string, after = 0, signal?: Sig) => request<{ events: Event[] }>(`/runs/${enc(id)}/events`, { query: { after }, signal }).then((r) => r.events),
  snapshots: (id: string, signal?: Sig) => request<{ snapshots: Snapshot[] }>(`/runs/${enc(id)}/snapshots`, { signal }).then((r) => r.snapshots),
  artifacts: (id: string, signal?: Sig) => request<{ artifacts: Artifact[] }>(`/runs/${enc(id)}/artifacts`, { signal }).then((r) => r.artifacts),
  downloadArtifact: (a: Artifact) => download(`/artifacts/${enc(a.id)}`, a.path.split("/").pop() || a.id),
  runCost: (id: string, signal?: Sig) => request<RunCost>(`/runs/${enc(id)}/cost`, { signal }),
  costs: (tenant: Scope, p: CostSummaryParams, signal?: Sig) =>
    request<CostSummary>("/costs", { tenant, query: { group: p.group, family: p.family, nofamily: p.nofamily, interval: p.interval, top: p.top, rank: p.rank, runs: p.runs, since: p.since, from: p.from, to: p.to, label: p.label, nolabel: p.nolabel }, signal }),
  /** The label keys on Runs with cost in the range (after the filters), most Runs first. */
  costLabels: (tenant: Scope, p: Pick<CostSummaryParams, "since" | "label" | "nolabel">, signal?: Sig) => request<CostLabels>("/costs/labels", { tenant, query: { since: p.since, label: p.label, nolabel: p.nolabel }, signal }),

  stopRun: (id: string) => request<Run>(`/runs/${enc(id)}/stop`, { method: "POST" }),
  terminateRun: (id: string) => request<Run>(`/runs/${enc(id)}/terminate`, { method: "POST" }),
  resumeRun: (id: string, body: ResumeRequest) => request<Run>(`/runs/${enc(id)}/resume`, { method: "POST", body }),
  migrateRun: (id: string, body: MigrateRequest) => request<Run>(`/runs/${enc(id)}/migrate`, { method: "POST", body }),
  inputRun: (id: string, text: string) => request<{ requestId: string }>(`/runs/${enc(id)}/input`, { method: "POST", body: { text } }),
  interruptRun: (id: string) => request<{ requestId: string }>(`/runs/${enc(id)}/input`, { method: "POST", body: { interrupt: true } }),

  /** A single-use ticket (60s) that lets a browser open the exec WebSocket or a preview without a header. */
  ticket: (id: string, kind: StreamTicket["kind"], signal?: Sig) => request<StreamTicket>(`/runs/${enc(id)}/tickets`, { method: "POST", body: { kind }, signal }),

  addServer: (id: string, body: ServerInput) => request<Server>(`/runs/${enc(id)}/servers`, { method: "POST", body }),
  startServer: (id: string, name: string) => request<Server>(`/runs/${enc(id)}/servers/${enc(name)}/start`, { method: "POST" }),
  stopServer: (id: string, name: string) => request<Server>(`/runs/${enc(id)}/servers/${enc(name)}/stop`, { method: "POST" }),
  restartServer: (id: string, name: string) => request<Server>(`/runs/${enc(id)}/servers/${enc(name)}/restart`, { method: "POST" }),
  removeServer: (id: string, name: string) => request<void>(`/runs/${enc(id)}/servers/${enc(name)}`, { method: "DELETE" }),
  /** The tenant's servers (state, label=k=v, wake, run, hostname filters). */
  servers: (tenant: Scope, params: { state?: string; label?: string[]; wake?: string; run?: string; hostname?: string } = {}, signal?: Sig) =>
    request<TenantServerList>(`/servers`, { tenant, query: params, signal }),
  server: (id: string, signal?: Sig) => request<TenantServer>(`/servers/${enc(id)}`, { signal }),
  deleteServer: (id: string) => request<void>(`/servers/${enc(id)}`, { method: "DELETE" }),
  attachServer: (id: string, runId: string) => request<TenantServer>(`/servers/${enc(id)}/attach`, { method: "POST", body: { runId } }),
  detachServer: (id: string) => request<TenantServer>(`/servers/${enc(id)}/detach`, { method: "POST" }),
  serverAction: (id: string, action: "start" | "stop" | "restart") => request<TenantServer>(`/servers/${enc(id)}/${action}`, { method: "POST" }),
  serverEvents: (id: string, signal?: Sig) => request<{ events: Event[] }>(`/servers/${enc(id)}/events`, { signal }).then((r) => r.events ?? []),
  tenantServerLog: (id: string, tail = 200, signal?: Sig) => request<{ lines: ServerLogLine[] }>(`/servers/${enc(id)}/log`, { query: { tail }, signal }).then((r) => r.lines ?? []),
  /** A preview ticket for one server (its sign-in at <url>/.lux/auth). */
  serverTicket: (id: string, signal?: Sig) => request<StreamTicket>(`/servers/${enc(id)}/tickets`, { method: "POST", signal }),
  serverLog: (id: string, name: string, tail = 200, signal?: Sig) => request<{ lines: ServerLogLine[] }>(`/runs/${enc(id)}/servers/${enc(name)}/log`, { query: { tail }, signal }).then((r) => r.lines ?? []),

  hosts: (tenant: Scope, p: HostListParams = {}, signal?: Sig) => request<{ hosts: Host[] }>("/hosts", { tenant, query: { all: p.all, pool: p.pool, state: p.state }, signal }).then((r) => r.hosts),
  /** A counted page of hosts in a sort's order (server-side). */
  hostsPage: (tenant: Scope, p: HostListParams, signal?: Sig) =>
    request<{ hosts: Host[] } & PageLinks>("/hosts", {
      tenant,
      query: { all: p.all, pool: p.pool, poolId: p.poolId, state: p.state, lifecycle: p.lifecycle, limit: p.limit, offset: p.offset, ...pageQuery(p) },
      signal,
    }).then((r) => toPage(r.hosts, r)),
  host: (id: string, signal?: Sig) => request<Host>(`/hosts/${enc(id)}`, { signal }),
  hostSummary: (tenant: Scope, signal?: Sig) => request<HostSummary>("/hosts/summary", { tenant, signal }),
  hostHistory: (id: string, since: string, signal?: Sig, res?: number) => request<History>(`/hosts/${enc(id)}/history`, { query: { since, res }, signal }),
  hostCost: (id: string, since: string, tenant: Scope, signal?: Sig) => request<HostCost>(`/hosts/${enc(id)}/cost`, { tenant, query: { since }, signal }),
  drainHost: (id: string, forceEvict = false) => request<{ draining: boolean; host: string }>(`/hosts/${enc(id)}/drain`, { method: "POST", body: { forceEvict } }),

  /** A page of a host's events in a sort's order. */
  hostEventsPage: (id: string, tenant: Scope, p: PageParams & { limit: number }, signal?: Sig) =>
    request<{ events: LifecycleEvent[] } & PageLinks>(`/hosts/${enc(id)}/events`, { tenant, query: { limit: p.limit, ...pageQuery(p) }, signal }).then((r) => toPage(r.events, r)),

  pools: (tenant: Scope, signal?: Sig) => request<{ pools: Pool[] }>("/pools", { tenant, signal }).then((r) => r.pools),
  /** Renames a pool; only its name changes. tenant: the pool's (an operator's tenant pool); platform: the platform's pool of that name. */
  renamePool: (tenant: Scope, name: string, newName: string, platform = false) =>
    request<Pool>(`/pools/${enc(name)}/rename`, { method: "POST", tenant, query: platform ? { owner: "platform" } : {}, body: { name: newName } }),
  /** Marks one of a tenant's pools as its default (tenant: the pool's, for an operator). */
  makePoolDefault: (tenant: Scope, name: string) => request<Pool>("/pools", { method: "POST", tenant, body: { name, isDefault: true } }),
  /** Every pool's figures over since, in one read. */
  poolStats: (tenant: Scope, since: string, signal?: Sig) => request<{ pools: PoolStats[] }>("/pools/stats", { tenant, query: { since }, signal }).then((r) => r.pools),
  poolMetrics: (name: string, tenant: Scope, owner: PoolOwner | undefined, since: string, signal?: Sig, res?: number) => request<PoolMetrics>(`/pools/${enc(name)}/metrics`, { tenant, query: { owner, since, res }, signal }),
  poolCost: (name: string, tenant: Scope, owner: PoolOwner | undefined, since: string, interval: "hour" | "day", signal?: Sig) =>
    request<PoolCost>(`/pools/${enc(name)}/cost`, { tenant, query: { owner, since, interval }, signal }),
  /** A page of a pool's events in a sort's order. owner: which pool of that name, where a tenant's and the platform's share it; undefined: the server's default. */
  poolEventsPage: (name: string, tenant: Scope, owner: PoolOwner | undefined, p: PageParams & { limit: number }, signal?: Sig) =>
    request<{ events: LifecycleEvent[] } & PageLinks>(`/pools/${enc(name)}/events`, { tenant, query: { owner, limit: p.limit, ...pageQuery(p) }, signal }).then((r) => toPage(r.events, r)),
  tenants: (signal?: Sig) => request<{ tenants: Tenant[] }>("/tenants", { signal }).then((r) => r.tenants),
  /** Operators: a tenant, and optionally its first owner's invitation. */
  createTenant: (body: { name: string; ownerEmail?: string }) => request<NewTenantResult>("/tenants", { method: "POST", body }),

  me: (signal?: Sig) => request<Me>("/me", { signal }),
  /** My keys in the tenant in scope (keys are per person per tenant). */
  myKeys: (tenant: Scope, signal?: Sig) => request<{ keys: ApiKey[] }>("/me/keys", { tenant, signal }).then((r) => r.keys ?? []),
  createMyKey: (tenant: Scope, body: NewKeyRequest) => request<CreatedApiKey>("/me/keys", { method: "POST", tenant, body }),
  renameMyKey: (tenant: Scope, id: string, name: string) => request<ApiKey>(`/me/keys/${enc(id)}`, { method: "PATCH", tenant, body: { name } }),
  revokeMyKey: (tenant: Scope, id: string) => request<void>(`/me/keys/${enc(id)}`, { method: "DELETE", tenant }),
  myInvitations: (signal?: Sig) => request<{ invitations: MyInvitation[] }>("/me/invitations", { signal }).then((r) => r.invitations ?? []),

  members: (tenant: Scope, signal?: Sig) => request<{ members: Member[] }>("/members", { tenant, signal }).then((r) => r.members ?? []),
  member: (tenant: Scope, user: string, signal?: Sig) => request<Member>(`/members/${enc(user)}`, { tenant, signal }),
  /** A member's keys: 404 unless it is me or I am an admin. */
  memberKeys: (tenant: Scope, user: string, signal?: Sig) => request<{ keys: ApiKey[] }>(`/members/${enc(user)}/keys`, { tenant, signal }).then((r) => r.keys ?? []),
  revokeMemberKey: (tenant: Scope, user: string, id: string) => request<void>(`/members/${enc(user)}/keys/${enc(id)}`, { method: "DELETE", tenant }),
  setMemberRole: (tenant: Scope, user: string, role: TenantRole) => request<Member>(`/members/${enc(user)}`, { method: "PATCH", tenant, body: { role } }),
  removeMember: (tenant: Scope, user: string) => request<void>(`/members/${enc(user)}`, { method: "DELETE", tenant }),
  /** Operators: add a person directly by email (key-mode installs). */
  addMember: (tenant: Scope, body: { email: string; role: TenantRole; upsert?: boolean }) => request<Member>("/members", { method: "POST", tenant, body }),

  /** cursor: luxd always sends it; "" when there are no more. */
  invitations: (tenant: Scope, p: { status?: string; q?: string; cursor?: string } = {}, signal?: Sig) => request<{ invitations: Invitation[]; cursor: string }>("/invitations", { tenant, query: p, signal }),
  invitationCounts: (tenant: Scope, signal?: Sig) => request<InvitationCounts>("/invitations/counts", { tenant, signal }),
  invite: (tenant: Scope, body: { invitations: { email: string; name?: string }[]; role: TenantRole }) => request<{ results: InviteResult[] }>("/invitations", { method: "POST", tenant, body }).then((r) => r.results ?? []),
  renewInvitation: (tenant: Scope, id: string) => request<Invitation>(`/invitations/${enc(id)}/renew`, { method: "POST", tenant }),
  revokeInvitation: (tenant: Scope, id: string) => request<Invitation>(`/invitations/${enc(id)}/revoke`, { method: "POST", tenant }),
  invitationByCode: (code: string, signal?: Sig) => request<InvitationLookup>(`/invitations/by-code/${enc(code)}`, { signal }),
  acceptInvitation: (code: string) => request<AcceptedInvitation>(`/invitations/by-code/${enc(code)}/accept`, { method: "POST" }),

  serviceAccounts: (tenant: Scope, signal?: Sig) => request<{ serviceAccounts: ServiceAccount[] }>("/service-accounts", { tenant, signal }).then((r) => r.serviceAccounts ?? []),
  serviceAccount: (tenant: Scope, id: string, signal?: Sig) => request<ServiceAccount>(`/service-accounts/${enc(id)}`, { tenant, signal }),
  /** ownerEmail: a member's email (the server resolves it). */
  createServiceAccount: (tenant: Scope, body: { name: string; description?: string; role: string; ownerEmail?: string }) => request<ServiceAccount>("/service-accounts", { method: "POST", tenant, body }),
  /** ownerEmail: a member's email, "" for no owner, absent to leave it. */
  updateServiceAccount: (tenant: Scope, id: string, body: { name?: string; description?: string; role?: string; ownerEmail?: string; disabled?: boolean }) =>
    request<ServiceAccount>(`/service-accounts/${enc(id)}`, { method: "PATCH", tenant, body }),
  serviceAccountKeys: (tenant: Scope, id: string, signal?: Sig) => request<{ keys: ApiKey[] }>(`/service-accounts/${enc(id)}/keys`, { tenant, signal }).then((r) => r.keys ?? []),
  createServiceAccountKey: (tenant: Scope, id: string, body: NewKeyRequest) => request<CreatedApiKey>(`/service-accounts/${enc(id)}/keys`, { method: "POST", tenant, body }),
  revokeServiceAccountKey: (tenant: Scope, id: string, key: string) => request<void>(`/service-accounts/${enc(id)}/keys/${enc(key)}`, { method: "DELETE", tenant }),

  /** Cost per member (each person and service account) or per owner (service accounts folded into their owner); runs=true: luxd counts Runs only when asked. */
  costsByOwner: (tenant: Scope, by: "member" | "owner", since: string, signal?: Sig) => request<CostByOwner>("/costs", { tenant, query: { by, since, runs: true }, signal }),

  /** Operators: every person, across tenants. */
  people: (signal?: Sig) => request<{ people: PlatformPerson[] }>("/people", { signal }).then((r) => r.people ?? []),
};

export type PoolOwner = "platform" | "tenant";

/** A paged list's envelope beside its rows. */
type PageLinks = Omit<Page<never>, "rows">;

function toPage<T>(rows: T[], r: PageLinks): Page<T> {
  return { rows, next: r.next, prev: r.prev, page: r.page, total: r.total, offset: r.offset };
}

function pageQuery(p: PageParams) {
  return { sort: p.sort, dir: p.dir, next: p.next, prev: p.prev, at: p.at };
}

function enc(s: string): string {
  return encodeURIComponent(s);
}
