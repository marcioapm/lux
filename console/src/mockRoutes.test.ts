import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { callerOf, resetPeople } from "../mockPeople.ts";
import { peopleApi } from "../mockPeopleApi.ts";

// Every people operation luxd documents (docs/openapi.yaml, tag people) is
// one the dev mock answers: a new route cannot ship with a console the mock
// cannot show. Path parameters get fixture values; the answer only has to
// be the mock's own (not null, which means "not a people route").
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

test("the mock answers every people route in docs/openapi.yaml", async () => {
  const ops = peopleOperations().filter((o) => /^\/v1\/(members|service-accounts)/.test(o.path));
  // The routes this change added are among them.
  for (const want of ["DELETE /v1/service-accounts/{sa}", "POST /v1/service-accounts/{sa}/convert", "PATCH /v1/service-accounts/{sa}", "POST /v1/members"]) expect(ops.map((o) => `${o.method} ${o.path}`)).toContain(want);
  const unanswered: string[] = [];
  for (const o of ops) {
    resetPeople();
    const path = o.path.replace(/\{(\w+)\}/g, (_, k: string) => FILL[k] ?? k);
    const req = new Request(`http://mock${path}?tenant=ten_acme`, { method: o.method, headers: { authorization: "Bearer luxk_mock_operator", "content-type": "application/json" }, ...(o.method === "GET" || o.method === "DELETE" ? {} : { body: JSON.stringify({ email: "bruno@acme.dev", role: "member", name: "x" }) }) });
    const u = new URL(req.url);
    const r = await peopleApi(req, u, callerOf(req, u, false), "cloudflare-access");
    if (r == null) unanswered.push(`${o.method} ${o.path}`);
  }
  resetPeople();
  expect(unanswered).toEqual([]);
});
