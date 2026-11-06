import { expect, test } from "bun:test";
import { withDom } from "../testDom.ts";

// Hosts, pools and tenant servers by role (decisions 2–5): viewers change
// nothing; members only their own Runs' servers; admins the tenant's
// pools, hosts and servers; operators the platform's pools too.
let HostPage: typeof import("./HostPage.tsx").HostPage;
let Pools: typeof import("./Pools.tsx").Pools;
let ServerPage: typeof import("./ServerPage.tsx").ServerPage;
let RunPage: typeof import("./RunPage.tsx").RunPage;
let t: typeof import("../peopleTesting.tsx");
withDom(async () => {
  ({ HostPage } = await import("./HostPage.tsx"));
  ({ Pools } = await import("./Pools.tsx"));
  ({ ServerPage } = await import("./ServerPage.tsx"));
  ({ RunPage } = await import("./RunPage.tsx"));
  t = await import("../peopleTesting.tsx");
});

const now = new Date().toISOString();
const times = { created: now, provisionRequested: null, provisioned: null, registered: now, firstPlacement: null, lastPlacementEnded: null, drainRequested: null, terminateRequested: null, terminated: null, lost: null };
const host = (platform: boolean) => ({ id: "h1", name: "host-1", tenant: platform ? "" : "acme", pool: "burst", state: "ready", draining: false, labels: {}, capacity: { cpus: 4, memory: 1024, disk: 1024, runs: 4 }, allocated: {}, versions: {}, platform, liveRuns: 1, times });
const hostApi = (platform: boolean) => (path: string) => {
  if (path.startsWith("/v1/hosts/h1/history")) return { from: now, to: now, resolution: 60, samples: [] };
  if (path.startsWith("/v1/hosts/h1/events")) return { events: [] };
  if (path.startsWith("/v1/hosts/h1/cost")) return { hostId: "h1", from: now, to: now, basis: "list", hours: [] };
  if (path.startsWith("/v1/hosts/h1")) return host(platform);
  if (path.startsWith("/v1/runs")) return { runs: [] };
  return undefined;
};

test("Host: Drain for admins (the tenant's hosts) and operators; never for viewers or members, nor admins on a platform host", async () => {
  for (const [role, platform, shown] of [["viewer", false, false], ["member", false, false], ["admin", false, true], ["owner", true, false], ["operator", true, true]] as const) {
    const p = await t.mountAs(role, "/hosts/h1", <HostPage id="h1" />, { extra: hostApi(platform) });
    try {
      await p.until(() => p.text().includes("host-1"), `${role}: the host`);
      expect([role, platform, p.button("Drain") != null]).toEqual([role, platform, shown]);
    } finally {
      await p.done();
    }
  }
});

const pool = (name: string, platform: boolean) => ({ id: `p_${name}`, name, tenant: platform ? undefined : "acme", provider: "static", minHosts: 0, maxHosts: 0, warmHosts: 0, shared: platform, platform, isDefault: false });
const poolsApi = (path: string) => {
  if (path.startsWith("/v1/pools/stats")) return { pools: [] };
  if (path.startsWith("/v1/pools")) return { pools: [pool("mine", false), pool("shared", true)] };
  if (path.startsWith("/v1/hosts")) return { hosts: [] };
  return undefined;
};
const rowButtons = (el: HTMLElement, name: string) => [...el.querySelectorAll("tbody tr")].find((r) => r.textContent?.includes(name))?.querySelectorAll(".row-actions button").length ?? 0;

test("Pools: Make default and Rename for admins on the tenant's pools, operators on every pool; none for viewers and members", async () => {
  for (const [role, mine, shared] of [["viewer", 0, 0], ["member", 0, 0], ["admin", 2, 0], ["operator", 2, 2]] as const) {
    const p = await t.mountAs(role, "/pools", <Pools />, { extra: poolsApi });
    try {
      await p.until(() => p.text().includes("mine") && p.text().includes("shared"), `${role}: pools`);
      expect([role, rowButtons(p.el, "mine"), rowButtons(p.el, "shared")]).toEqual([role, mine, shared]);
    } finally {
      await p.done();
    }
  }
});

const server = (runId: string | null) => ({ id: "srv_1", name: "web", port: 3000, state: "ready", process: "running", desired: "running", wake: "run", lifetime: "owner", owner: "acme", runId, createdAt: now, updatedAt: now, since: now, labels: {}, env: {}, url: null });
const runOf = (owner: { kind: "user"; id: string; name: string }) => ({ id: "run_1", tenant: "acme", state: "running", epoch: 1, labels: {}, createdAt: now, spec: { image: { ref: "img" }, resources: {}, workload: { adapter: "generic" } }, owner, servers: [] });
const serverApi = (runId: string | null, owner = { kind: "user" as const, id: "usr_car", name: "Carla Santos" }) => (path: string) => {
  if (path.startsWith("/v1/servers/srv_1/events")) return { events: [] };
  if (path.startsWith("/v1/servers/srv_1/log")) return { lines: [] };
  if (path.startsWith("/v1/servers/srv_1")) return server(runId);
  if (path.startsWith("/v1/runs/run_1")) return runOf(owner);
  return undefined;
};

test("Tenant server: a viewer changes nothing; a member their own Run's server only; an unattached one admins only", async () => {
  const cases = [
    ["viewer", "run_1", false],
    ["member", "run_1", true], // Carla's Run
    ["bruno", "run_1", false], // a member, not the owner
    ["member", null, false],
    ["admin", null, true],
  ] as const;
  for (const [role, runId, shown] of cases) {
    const p = await t.mountAs(role, "/servers/srv_1", <ServerPage id="srv_1" />, { extra: serverApi(runId) });
    try {
      await p.until(() => p.text().includes("web"), `${role}: the server`);
      if (shown) await p.until(() => p.button("Delete") != null, `${role}: Delete`);
      else await new Promise((r) => setTimeout(r, 50));
      expect([role, runId, p.button("Delete") != null, p.button("Restart") != null]).toEqual([role, runId, shown, shown && runId != null]);
    } finally {
      await p.done();
    }
  }
});

test("A Run's Servers tab: no Add server (in the header or the empty state) for a viewer or another member; the owner has it", async () => {
  const runApi = (path: string) => (path.startsWith("/v1/runs/run_1/output") ? new Response("event: end\ndata: {}\n\n", { headers: { "Content-Type": "text/event-stream" } }) : path.startsWith("/v1/runs/run_1") ? runOf({ kind: "user", id: "usr_car", name: "Carla Santos" }) : undefined);
  for (const [role, shown] of [["viewer", false], ["bruno", false], ["member", true]] as const) {
    const p = await t.mountAs(role, "/runs/run_1?tab=servers", <RunPage id="run_1" />, { extra: runApi });
    try {
      await p.until(() => p.text().includes("No servers"), `${role}: the empty Servers tab`);
      if (shown) await p.until(() => p.button("Add server") != null, `${role}: Add server`);
      const adds = [...p.el.querySelectorAll("button")].filter((b) => t.visibleText(b) === "Add server").length;
      expect([role, adds > 0]).toEqual([role, shown]);
      if (!shown) expect(p.text()).toContain("only Carla Santos and admins can act on it");
    } finally {
      await p.done();
    }
  }
});
