import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";

// /tenants and /people are operators': while whoami has not answered the
// role is unknown, so neither renders (no enabled New tenant); a resolved
// tenant session gets Not found; New tenant needs an operator who may change
// things (not a read-only key).
let App: typeof import("./App.tsx").App;
let api: typeof import("../api/index.ts");
let testing: typeof import("./testing.ts");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ App } = await import("./App.tsx"));
  api = await import("../api/index.ts");
  testing = await import("./testing.ts");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const tenants = { tenants: [{ id: "ten_acme", name: "acme", retentionDays: 30, expireAfterDays: 0, activeRuns: 0, runs: 0, hosts: 0, storedBytes: 0, createdAt: "2026-01-01T00:00:00Z", ownersCount: 0, owners: [], membersCount: 0 }] };
const operator = { operator: true, tenant: "", tenantId: "", scopes: ["read", "run", "admin"], consoleAuth: "key", memberships: [] };

async function mount(path: string, whoami: () => unknown | Promise<unknown>) {
  api.signIn("k");
  const fake = testing.fakeApi(async (p) => (p.startsWith("/v1/whoami") ? await whoami() : p.startsWith("/v1/tenants") ? tenants : {}));
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL(`http://localhost${path}`);
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () => root.render(<App />));
  const newTenant = () => [...el.querySelectorAll("button")].find((b) => b.textContent?.trim() === "New tenant");
  return {
    el,
    newTenant,
    done: async () => {
      await act(async () => root.unmount());
      el.remove();
      fake.restore();
      api.signOut();
    },
  };
}

test("/tenants with whoami pending: no Tenants page, no New tenant; then an operator gets both", async () => {
  let answer!: (v: unknown) => void;
  const pending = new Promise((r) => (answer = r));
  const p = await mount("/tenants", () => pending);
  try {
    await act(async () => new Promise((r) => setTimeout(r, 100)));
    expect(p.newTenant()).toBeUndefined();
    expect(p.el.querySelector("table")).toBeNull();
    await act(async () => answer(operator));
    await testing.until(() => p.newTenant() != null, "New tenant for the operator", 3000);
    expect(p.newTenant()!.disabled).toBe(false);
  } finally {
    await p.done();
  }
});

test("/tenants for a read-only operator key: the list, no New tenant", async () => {
  const p = await mount("/tenants", () => ({ ...operator, readOnly: true }));
  try {
    await testing.until(() => p.el.textContent?.includes("acme") === true, "the tenants list", 3000);
    expect(p.newTenant()).toBeUndefined();
  } finally {
    await p.done();
  }
});

test("/people with whoami pending renders no People page", async () => {
  const p = await mount("/people", () => new Promise(() => {}));
  try {
    await act(async () => new Promise((r) => setTimeout(r, 100)));
    expect(p.el.textContent).not.toContain("Everyone with a lux account");
  } finally {
    await p.done();
  }
});
