import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { callerOf, resetPeople, SAS, KEYS, MEMBERSHIPS } from "../mockPeople.ts";
import { peopleApi } from "../mockPeopleApi.ts";

// Every people operation luxd documents (docs/openapi.yaml, tag people) is
// one the dev mock answers: a new route cannot ship with a console the mock
// cannot show. Each operation resets state, then checks its HTTP status and
// successful response shape (a different route's non-null reply is not enough).
const FILL: Record<string, string> = { user: "usr_bru", sa: "grafana-reader", key: "key_s_gr1", id: "inv_01", code: "inv_7Rk2Qm9xPa" };

function peopleOperations(): { method: string; path: string }[] {
  const yaml = readFileSync(new URL("../../docs/openapi.yaml", import.meta.url), "utf8");
  const lines = yaml.split("\n");
  const out: { method: string; path: string }[] = [];
  let path = "";
  let method = "";
  for (const line of lines) {
    const p = /^ {2}(\/v1\/\S+):$/.exec(line);
    if (p) path = p[1]!;
    const m = /^ {4}(get|post|patch|put|delete):$/.exec(line);
    if (m) method = m[1]!.toUpperCase();
    if (/^ {8}- people$/.test(line) && path && method) out.push({ method, path });
  }
  return out;
}

test("unsupported verbs/subpaths never mutate people state", async () => {
  for (const [method, path] of [
    ["POST", "/v1/service-accounts/grafana-reader"],
    ["PUT", "/v1/service-accounts"],
    ["PATCH", "/v1/service-accounts/grafana-reader/convert"],
    ["PATCH", "/v1/service-accounts/grafana-reader/keys"],
    ["POST", "/v1/service-accounts/grafana-reader/keys/key_s_gr1"],
    ["PATCH", "/v1/service-accounts/grafana-reader/unknown"],
    ["PUT", "/v1/members/usr_bru"],
    ["POST", "/v1/members/usr_bru/keys"],
  ]) {
    resetPeople();
    const before = JSON.stringify({ SAS, KEYS, MEMBERSHIPS });
    const req = new Request(`http://mock${path}?tenant=ten_acme`, { method, headers: { authorization: "Bearer luxk_mock_operator", "content-type": "application/json" }, body: JSON.stringify({ name: "mutated", email: "bruno@acme.dev", role: "owner" }) });
    const u = new URL(req.url);
    expect(await peopleApi(req, u, callerOf(req, u, false), "cloudflare-access")).toBeNull();
    expect(JSON.stringify({ SAS, KEYS, MEMBERSHIPS })).toBe(before);
  }
  resetPeople();
});

test("the mock answers every people route in docs/openapi.yaml", async () => {
  const ops = peopleOperations().filter((o) => /^\/v1\/(members|service-accounts)/.test(o.path));
  // The routes this change added are among them.
  for (const want of ["DELETE /v1/service-accounts/{sa}", "POST /v1/service-accounts/{sa}/convert", "PATCH /v1/service-accounts/{sa}", "POST /v1/members"]) expect(ops.map((o) => `${o.method} ${o.path}`)).toContain(want);
  const unanswered: string[] = [];
  for (const o of ops) {
    resetPeople();
    const path = o.path.replace(/\{(\w+)\}/g, (_, k: string) => k === "key" && o.path.startsWith("/v1/members") ? "key_u_bru1" : FILL[k] ?? k);
    const req = new Request(`http://mock${path}?tenant=ten_acme`, { method: o.method, headers: { authorization: "Bearer luxk_mock_operator", "content-type": "application/json" }, ...(o.method === "GET" || o.method === "DELETE" ? {} : { body: JSON.stringify({ email: "bruno@acme.dev", role: "member", name: "x", upsert: true }) }) });
    const u = new URL(req.url);
    const r = await peopleApi(req, u, callerOf(req, u, false), "cloudflare-access");
    if (r == null) unanswered.push(`${o.method} ${o.path}`);
    expect(r, `${o.method} ${o.path}`).not.toBeNull();
    if (!r) continue;
    expect(r.status, `${o.method} ${o.path}`).toBe(o.method === "DELETE" ? 204 : o.method === "POST" && (o.path === "/v1/service-accounts" || o.path.endsWith("/keys")) ? 201 : 200);
    const b = r.body as Record<string, unknown>;
    if (o.method === "DELETE") expect(b).toBeNull();
    else if (o.path.endsWith("/convert")) {
      expect(b.user).toMatchObject({ id: "usr_bru", email: "bruno@acme.dev", role: "member" });
      expect(typeof b.keys).toBe("number");
      expect(typeof b.runs).toBe("number");
      expect(b).not.toHaveProperty("apiKey");
    } else if (o.path.endsWith("/keys") && o.method === "GET") expect(Array.isArray(b.keys)).toBe(true);
    else if (o.path.endsWith("/keys") && o.method === "POST") {
      expect(b).toMatchObject({ name: "x", readOnly: false });
      expect(typeof b.apiKey).toBe("string");
      expect(typeof b.id).toBe("string");
    } else if (o.path === "/v1/service-accounts" && o.method === "GET") expect(Array.isArray(b.serviceAccounts)).toBe(true);
    else if (o.path === "/v1/members" && o.method === "GET") expect(Array.isArray(b.members)).toBe(true);
    else if (o.path.startsWith("/v1/members")) expect(b).toMatchObject({ id: "usr_bru", email: "bruno@acme.dev", role: "member" });
    else {
      expect(typeof b.id).toBe("string");
      expect(typeof b.name).toBe("string");
      expect(typeof b.activeKeys).toBe("number");
      expect(typeof b.runs).toBe("number");
    }
  }
  resetPeople();
  expect(unanswered).toEqual([]);
});
