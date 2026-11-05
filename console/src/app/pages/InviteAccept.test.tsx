import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
let InviteAccept: typeof import("./InviteAccept.tsx").InviteAccept;
let NoTenant: typeof import("./InviteAccept.tsx").NoTenant;
let t: typeof import("../peopleTesting.tsx");
beforeAll(async () => { GlobalRegistrator.register(); ({ InviteAccept, NoTenant } = await import("./InviteAccept.tsx")); t = await import("../peopleTesting.tsx"); (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(async () => { await GlobalRegistrator.unregister(); });
test("invitation: matching email can join, another email sees nothing about tenant", async () => {
  const p = await t.mountAs("invited", "/invite/inv_7Rk2Qm9xPa", <InviteAccept code="inv_7Rk2Qm9xPa" />);
  try { await p.until(() => p.text().includes("Join acme"), "join offer"); expect(p.button("Join acme")).toBeDefined(); } finally { await p.done(); }
  const w = await t.mountAs("notenant", "/invite/inv_7Rk2Qm9xPa", <InviteAccept code="inv_7Rk2Qm9xPa" />);
  try { await w.until(() => w.text().includes("someone else"), "wrong email"); expect(w.text()).not.toContain("acme"); } finally { await w.done(); }
});
test("no tenant shows the contact-admin sentence and the pending invitation for this email", async () => {
  const p = await t.mountAs("invited", "/", <NoTenant />);
  try { await p.until(() => p.button("Join") != null, "pending invitation"); expect(p.text()).toContain("Contact your admin to invite this email"); expect(p.text()).toContain("acme · Member"); } finally { await p.done(); }
});
