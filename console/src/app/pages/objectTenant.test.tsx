import { expect, test } from "bun:test";
import { withDom } from "../testDom.ts";

// Object pages decide in the object's tenant, not the one in scope: Rui
// (mock "multi") is a viewer in globex and an admin in initech.
let RunPage: typeof import("./RunPage.tsx").RunPage;
let TerminalPage: typeof import("./TerminalPage.tsx").TerminalPage;
let ServerPage: typeof import("./ServerPage.tsx").ServerPage;
let t: typeof import("../peopleTesting.tsx");
withDom(async () => {
  ({ RunPage } = await import("./RunPage.tsx"));
  ({ TerminalPage } = await import("./TerminalPage.tsx"));
  ({ ServerPage } = await import("./ServerPage.tsx"));
  t = await import("../peopleTesting.tsx");
});

const runIn = (tenant: string) => ({ id: `run_${tenant}`, tenant, name: `a-run-in-${tenant}`, state: "running", epoch: 1, labels: {}, owner: { kind: "user", id: "usr_bru", name: "Bruno Costa" }, spec: { image: { ref: "alpine" }, workload: { adapter: "generic" }, resources: {} }, createdAt: "2026-11-01T00:00:00Z" });
const serverIn = (tenantId: string) => ({ id: "srv_x", tenantId, name: "web", hostname: null, url: null, state: "stopped", process: "stopped", desired: "up", port: 3000, command: null, workdir: "", env: {}, afterSync: null, inject: "", labels: {}, wake: "never", idleAfter: "0s", wakeTimeout: "5m", lifetime: "owner", expireAfter: null, owner: "k", runId: null, fromSpec: false, since: "2026-11-01T00:00:00Z", readySince: null, stopReason: null, epoch: null, lastRequestAt: null, wakes: 0, createdAt: "2026-11-01T00:00:00Z", updatedAt: "2026-11-01T00:00:00Z" });

test("Run page: admin in the Run's tenant acts though viewer in the selected one, and the other way round", async () => {
  for (const [scope, tenant, acts] of [["ten_globex", "initech", true], ["ten_initech", "globex", false]] as const) {
    const run = runIn(tenant);
    const p = await t.mountAs("multi", `/runs/${run.id}?tenant=${scope}`, <RunPage id={run.id} />, { extra: (path) => (path.startsWith(`/v1/runs/${run.id}`) ? run : undefined) });
    try {
      await p.until(() => p.text().includes(run.name), "the run");
      await p.until(() => (p.el.querySelector(".page-head-actions") != null) === acts && (acts || p.text().includes("can act on it")), `${tenant} run in scope ${scope}: acts=${acts}`);
      if (acts) expect(p.text()).not.toContain("can act on it");
      // Owned service accounts are asked of the Run's tenant.
      expect(p.fake.calls.some((c) => c.startsWith("/v1/service-accounts") && c.includes(`tenant=${tenant}`))).toBe(true);
    } finally {
      await p.done();
    }
  }
});

test("Terminal: a shell in the Run's tenant where Rui is admin; refused where Rui is viewer, whatever the scope", async () => {
  for (const [scope, tenant, opens] of [["ten_globex", "initech", true], ["ten_initech", "globex", false]] as const) {
    // Stopped: the page opens the terminal view without dialing a shell.
    const run = { ...runIn(tenant), state: "stopped" };
    const p = await t.mountAs("multi", `/runs/${run.id}/terminal?tenant=${scope}`, <TerminalPage id={run.id} />, { extra: (path) => (path.startsWith(`/v1/runs/${run.id}`) ? run : undefined) });
    try {
      await p.until(() => p.text().includes("Only the owner and admins can open a terminal") !== opens && p.el.querySelector(".skeleton, .page-skeleton") == null, `terminal ${tenant} in ${scope}`);
      expect(p.text().includes("Only the owner and admins can open a terminal")).toBe(!opens);
    } finally {
      await p.done();
    }
  }
});

test("Server page: an unattached server is its tenant's admins' (Delete), decided in its tenant", async () => {
  for (const [scope, tenantId, deletes] of [["ten_globex", "ten_initech", true], ["ten_initech", "ten_globex", false]] as const) {
    const sv = serverIn(tenantId);
    const p = await t.mountAs("multi", `/servers/srv_x?tenant=${scope}`, <ServerPage id="srv_x" />, { extra: (path) => (path.startsWith("/v1/servers/srv_x") && !path.includes("/events") && !path.includes("/log") ? sv : undefined) });
    try {
      await p.until(() => p.text().includes("web"), "the server");
      if (deletes) await p.until(() => p.button("Delete") != null, "Delete for the admin of its tenant");
      else await new Promise((r) => setTimeout(r, 150));
      expect(p.button("Delete") != null).toBe(deletes);
    } finally {
      await p.done();
    }
  }
});

// A Run of acme, a tenant Rui's session whoami does not know: the page asks
// whoami?tenant=acme and shows no mutating control until it answers.
function deferredWhoami() {
  let settle!: (r: unknown) => void;
  const answer = new Promise<unknown>((r) => (settle = r));
  return { answer, settle };
}
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const ACTIONS = ["Stop", "Terminate", "Cancel", "Rename", "Extend", "Push", "Sync", "Checkpoint"];

test("Run page in a tenant whoami does not know: whoami?tenant=<it>, nothing mutating while pending; admin there → actions", async () => {
  const run = runIn("acme");
  const who = deferredWhoami();
  const p = await t.mountAs("multi", `/runs/${run.id}?tenant=ten_globex`, <RunPage id={run.id} />, {
    extra: (path) => {
      if (path.startsWith(`/v1/runs/${run.id}`)) return run;
      if (path.startsWith("/v1/whoami") && new URLSearchParams(path.split("?")[1] ?? "").get("tenant") === "acme") return who.answer;
      return undefined;
    },
  });
  try {
    await p.until(() => p.text().includes(run.name), "the run");
    await p.until(() => p.fake.calls.some((c) => c.startsWith("/v1/whoami") && c.includes("tenant=acme")), "whoami asked of the Run's tenant");
    await new Promise((r) => setTimeout(r, 100));
    expect(p.el.querySelector(".page-head-actions")).toBeNull();
    for (const a of ACTIONS) expect(p.button(a)).toBeUndefined();
    expect(p.el.querySelector("textarea")).toBeNull();
    who.settle(json(200, { operator: false, tenant: "acme", tenantId: "ten_acme", scopes: ["admin"], consoleAuth: "cloudflare-access", userId: "usr_rui", email: "rui@globex.dev", role: "admin", memberships: [{ tenantId: "ten_acme", tenant: "acme", role: "admin" }], invitationsEnabled: true }));
    await p.until(() => p.el.querySelector(".page-head-actions") != null, "the admin's actions");
    expect(p.text()).not.toContain("can act on it");
  } finally {
    who.settle(json(200, {}));
    await p.done();
  }
});

test("Run page in a tenant whoami does not know: 403 not_a_member there → read-only", async () => {
  const run = runIn("acme");
  const who = deferredWhoami();
  const p = await t.mountAs("multi", `/runs/${run.id}?tenant=ten_initech`, <RunPage id={run.id} />, {
    extra: (path) => {
      if (path.startsWith(`/v1/runs/${run.id}`)) return run;
      if (path.startsWith("/v1/whoami") && new URLSearchParams(path.split("?")[1] ?? "").get("tenant") === "acme") return who.answer;
      return undefined;
    },
  });
  try {
    await p.until(() => p.fake.calls.some((c) => c.startsWith("/v1/whoami") && c.includes("tenant=acme")), "whoami asked of the Run's tenant");
    expect(p.el.querySelector(".page-head-actions")).toBeNull();
    who.settle(json(403, { status: 403, code: "not_a_member", message: "you are not a member of acme" }));
    await new Promise((r) => setTimeout(r, 150));
    // Rui is admin in initech (the scope): that must not leak onto acme's Run.
    expect(p.el.querySelector(".page-head-actions")).toBeNull();
    for (const a of ACTIONS) expect(p.button(a)).toBeUndefined();
  } finally {
    await p.done();
  }
});
