import { expect, test } from "bun:test";
import { callerOf, canAct, whoamiFor } from "../mockPeople.ts";
import { peopleApi } from "../mockPeopleApi.ts";

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
