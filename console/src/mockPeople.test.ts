import { expect, test } from "bun:test";
import { callerOf, canAct, ownerFilter, resetPeople, whoamiFor } from "../mockPeople.ts";
import { createTenant, peopleApi, tenantPeople } from "../mockPeopleApi.ts";

// GET/POST /v1/tenants as luxd answers (internal/server/contract_test.go TestTenantPeopleContract).
test("tenants: owners are names by email with ownersCount/membersCount; create answers {id, tenantId, name, invitation?}", () => {
  resetPeople();
  expect(tenantPeople("ten_acme")).toEqual({ ownersCount: 2, owners: ["Eva Moreira", "Márcio Martins"], membersCount: 6 });
  expect(tenantPeople("ten_initech")).toEqual({ ownersCount: 0, owners: [], membersCount: 1 });
  const op = callerOf(new Request("http://mock/", { headers: { authorization: "Bearer luxk_mock_operator" } }), new URL("http://mock/"), false)!;
  const r = createTenant("newco", "CTO@newco.dev", op, "http://mock");
  expect(r.status).toBe(201);
  expect(Object.keys(r.body as object).sort()).toEqual(["id", "invitation", "name", "tenantId"]);
  expect(r.body).toMatchObject({ id: "ten_newco", tenantId: "ten_newco", name: "newco", invitation: { email: "cto@newco.dev", role: "owner" } });
  expect(Object.keys(createTenant("plain", undefined, op, "http://mock").body as object).sort()).toEqual(["id", "name", "tenantId"]);
  expect(createTenant("plain", undefined, op, "http://mock").status).toBe(409);
  resetPeople();
});

// The dev mock answers the people routes to the API contract, per role, so
// every screen can be shown as each role (luxk_mock_<role>).
const as = (role: string, path: string, init: RequestInit = {}, auth = "cloudflare-access") => {
  const req = new Request(`http://mock${path}`, { ...init, headers: { authorization: `Bearer luxk_mock_${role}`, "content-type": "application/json", ...(init.headers ?? {}) } });
  const u = new URL(req.url);
  return peopleApi(req, u, callerOf(req, u, auth === "cloudflare-access"), auth);
};

test("whoami per role: role, memberships, invitations only with Access", () => {
  const req = (r: string) => new Request("http://mock/v1/whoami", { headers: { authorization: `Bearer luxk_mock_${r}` } });
  const who = (r: string, auth = "cloudflare-access") => whoamiFor(callerOf(req(r), new URL("http://mock/v1/whoami"), false)!, auth, null);
  expect(who("viewer")).toMatchObject({ role: "viewer", tenant: "acme", memberships: [{ tenant: "acme", role: "viewer" }], invitationsEnabled: true });
  expect(who("admin", "key").invitationsEnabled).toBe(false);
  expect(who("notenant")).toMatchObject({ operator: false, memberships: [] });
  expect(who("operator")).toMatchObject({ operator: true, memberships: [{ tenant: "acme", role: "owner" }, { tenant: "globex", role: "admin" }] });
  expect(who("sa")).toMatchObject({ serviceAccount: { name: "ci-review-bot" }, role: "member" });
});

test("members: keyCount only for self and admins+; a non-member gets 403 not_a_member", async () => {
  const asMember = (await as("member", "/v1/members"))!.body as { members: { email: string; keyCount?: number }[] };
  expect(asMember.members.find((m) => m.email === "carla@acme.dev")!.keyCount).toBe(2);
  expect(asMember.members.find((m) => m.email === "ana@acme.dev")!.keyCount).toBeUndefined();
  const asAdmin = (await as("admin", "/v1/members"))!.body as { members: { email: string; keyCount?: number }[] };
  expect(asAdmin.members.find((m) => m.email === "carla@acme.dev")!.keyCount).toBe(2);
  expect((await as("member", "/v1/members/usr_ana/keys"))!.status).toBe(404);
  expect((await as("notenant", "/v1/members"))).toMatchObject({ status: 403, body: { error: { code: "not_a_member" } } });
});

test("invitations: batch results, owner rules, key mode 409", async () => {
  const r = (await as("admin", "/v1/invitations", { method: "POST", body: JSON.stringify({ role: "member", invitations: [{ email: "New@acme.dev" }, { email: "ana@acme.dev" }, { email: "filipa@acme.dev" }, { email: "rita@acme.dev" }, { email: "nope" }] }) }))!;
  const results = (r.body as { results: { email: string; result: string; invitation?: { url: string } }[] }).results;
  expect(results.map((x) => [x.email, x.result])).toEqual([
    ["new@acme.dev", "created"],
    ["ana@acme.dev", "member"],
    ["filipa@acme.dev", "existing"],
    ["rita@acme.dev", "renewed"],
    ["nope", "invalid"],
  ]);
  expect(results[0]!.invitation!.url).toMatch(/^http:\/\/mock\/invite\/inv_/);
  expect((await as("admin", "/v1/invitations", { method: "POST", body: JSON.stringify({ role: "owner", invitations: [{ email: "x@acme.dev" }] }) }))!.status).toBe(403);
  expect((await as("admin", "/v1/invitations", {}, "key"))).toMatchObject({ status: 409, body: { error: { code: "invitations_unavailable" } } });
  expect((await as("member", "/v1/invitations"))!.status).toBe(403);
});

test("invite by code: minimal fields; wrong email reveals nothing", async () => {
  expect((await as("invited", "/v1/invitations/by-code/inv_7Rk2Qm9xPa"))!.body).toMatchObject({ tenant: "acme", role: "member", status: "pending", emailMatches: true });
  expect((await as("notenant", "/v1/invitations/by-code/inv_7Rk2Qm9xPa"))!.body).toEqual({ status: "pending", emailMatches: false });
  expect((await as("notenant", "/v1/invitations/by-code/inv_7Rk2Qm9xPa/accept", { method: "POST" }))!.status).toBe(403);
});

test("keys: cap of 5 per person answers 409 key_limit", async () => {
  const create = () => as("admin", "/v1/me/keys", { method: "POST", body: JSON.stringify({ name: "k" }) });
  expect((await create())!.status).toBe(201);
  expect((await create())!.status).toBe(201);
  expect((await create())).toMatchObject({ status: 409, body: { error: { code: "key_limit" } } });
});

test("cost by member and by owner: service accounts fold into their owner, (no owner) stays", async () => {
  type Rows = { totals: { key: string; amount: string }[] };
  const byMember = (await as("viewer", "/v1/costs?by=member"))!.body as Rows;
  const byOwner = (await as("viewer", "/v1/costs?by=owner"))!.body as Rows;
  const amt = (b: Rows, k: string) => b.totals.find((r) => r.key === k)?.amount;
  expect(amt(byMember, "user:usr_car")).toBe("212.40");
  expect(amt(byMember, "sa:sa_7cr3b2x9")).toBe("388.77");
  expect(amt(byOwner, "user:usr_car")).toBe("601.17");
  expect(amt(byOwner, "sa:sa_7cr3b2x9")).toBeUndefined();
  expect(amt(byOwner, "sa:sa_5dd0p2x8")).toBe("77.40");
  expect(amt(byOwner, "(none)")).toBe("61.33");
  // As luxd: runs only with runs=true; group {member: key}; only owners with cost.
  expect(byMember.totals.every((r) => !("runs" in r))).toBe(true);
  const withRuns = (await as("viewer", "/v1/costs?by=member&runs=true"))!.body as { totals: Record<string, unknown>[] };
  expect(withRuns.totals.find((r) => r.key === "user:usr_car")).toEqual({ key: "user:usr_car", owner: { kind: "user", id: "usr_car", name: "Carla Santos", email: "carla@acme.dev" }, group: { member: "user:usr_car" }, currency: "USD", amount: "212.40", runs: 148 });
  expect(withRuns.totals.find((r) => r.key === "(none)")).toEqual({ key: "(none)", group: { member: "(none)" }, currency: "USD", amount: "61.33", runs: 211 });
  expect(withRuns.totals.some((r) => r.key === "user:usr_dio")).toBe(false);
});

// Field sets as luxd's structs marshal them (me.go Person: picture always; whoami.go: no role for an operator; MembershipRef with joinedAt).
test("people shapes: Person always has picture, operators have no role, memberships carry joinedAt", async () => {
  resetPeople();
  const keys = (o: unknown) => Object.keys(o as object).sort();
  const me = (await as("member", "/v1/me"))!.body as Record<string, unknown> & { memberships: Record<string, unknown>[] };
  expect(keys(me)).toEqual(["email", "id", "lastSignInAt", "memberships", "name", "operator", "picture"]);
  expect(me.picture).toBe("");
  expect(keys(me.memberships[0])).toEqual(["joinedAt", "role", "tenant", "tenantId"]);
  const members = ((await as("admin", "/v1/members"))!.body as { members: Record<string, unknown>[] }).members;
  expect(keys(members[0])).toEqual(["email", "id", "joinedAt", "keyCount", "lastSeenAt", "name", "operator", "picture", "role", "runningRuns"]);
  const sa = ((await as("admin", "/v1/service-accounts"))!.body as { serviceAccounts: Record<string, unknown>[] }).serviceAccounts.find((s) => s.name === "gha-runners")!;
  expect(keys(sa.owner)).toEqual(["email", "id", "name", "operator", "picture"]);
  const req = (r: string) => new Request("http://mock/v1/whoami", { headers: { authorization: `Bearer luxk_mock_${r}` } });
  const who = (r: string, tenant = "") => whoamiFor(callerOf(req(r), new URL(`http://mock/v1/whoami${tenant}`), false)!, "cloudflare-access", null) as Record<string, unknown>;
  expect(who("operator", "?tenant=acme")).not.toHaveProperty("role");
  expect((who("member").memberships as Record<string, unknown>[])[0]).toHaveProperty("joinedAt");
  expect(who("member")).toMatchObject({ previews: false, serverInject: true });
});

// GET /v1/runs?owner= as luxd (ownership_test.go TestRunOwnerShownAndFiltered): exact, comma-separated, user:/sa: prefixes.
test("owner filter: several owners, user:/sa: exact, email any case, no substrings", () => {
  const keep = (q: string) => ["hosts-probe-claude", "agent-refactor-42", "billing-backoff-418", "notes-count-acp", "no-owner"].filter(ownerFilter(null, new URL(`http://mock/v1/runs?${q}`)));
  expect(keep("owner=user:usr_car,sa:sa_7cr3b2x9")).toEqual(["hosts-probe-claude", "agent-refactor-42"]);
  expect(keep("owner=CARLA@acme.dev")).toEqual(["hosts-probe-claude"]);
  expect(keep("owner=ci-review-bot")).toEqual(["agent-refactor-42"]);
  expect(keep("owner=carla")).toEqual([]);
  expect(keep("owner=user:carla@acme.dev")).toEqual([]);
});

// As luxd: removeMember leaves their service accounts ownerless; accountKeyHolder 404s anyone but the owner and admins; roles validate (422).
test("mock: removing a member clears the accounts they own; account keys hidden from non-owners; roles validated", async () => {
  resetPeople();
  const sa = async (role: string, id: string) => (await as(role, `/v1/service-accounts/${id}`))!.body as { owner?: { id: string } };
  expect((await sa("admin", "sa_7cr3b2x9")).owner?.id).toBe("usr_car");
  expect((await as("admin", "/v1/members/usr_car", { method: "DELETE" }))!.status).toBe(204);
  expect((await sa("admin", "sa_7cr3b2x9")).owner).toBeUndefined();
  resetPeople();
  expect((await as("bruno", "/v1/service-accounts/ci-review-bot/keys"))!.status).toBe(404);
  expect((await as("viewer", "/v1/service-accounts/ci-review-bot/keys"))!.status).toBe(404);
  expect((await as("member", "/v1/service-accounts/ci-review-bot/keys"))!.status).toBe(200);
  expect((await as("bruno", "/v1/service-accounts/ci-review-bot/keys", { method: "POST", body: JSON.stringify({ name: "x" }) }))!.status).toBe(404);
  expect((await as("admin", "/v1/service-accounts", { method: "POST", body: JSON.stringify({ name: "boss", role: "owner" }) }))).toMatchObject({ status: 422, body: { error: { code: "invalid_request" } } });
  expect((await as("admin", "/v1/service-accounts/ci-review-bot", { method: "PATCH", body: JSON.stringify({ role: "owner" }) }))!.status).toBe(422);
  expect((await as("admin", "/v1/service-accounts", { method: "POST", body: JSON.stringify({ name: "x", role: "member", ownerEmail: "sam@globex.io" }) }))!.status).toBe(422);
  expect((await as("admin", "/v1/service-accounts", { method: "POST", body: JSON.stringify({ name: "ci-review-bot", role: "member" }) }))).toMatchObject({ status: 409, body: { error: { code: "conflict" } } });
  resetPeople();
});

test("canAct: owners, service account owners and admins act; viewers never", () => {
  const c = (r: string) => {
    const req = new Request("http://mock/", { headers: { authorization: `Bearer luxk_mock_${r}` } });
    return callerOf(req, new URL(req.url), false);
  };
  expect(canAct(c("member"), "agent-refactor-42")).toBe(true); // Carla owns ci-review-bot
  expect(canAct(c("member"), "billing-backoff-418")).toBe(false);
  expect(canAct(c("bruno"), "billing-backoff-418")).toBe(true);
  expect(canAct(c("viewer"), "billing-backoff-418")).toBe(false);
  expect(canAct(c("admin"), "billing-backoff-418")).toBe(true);
});
