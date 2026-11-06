import { expect, test } from "bun:test";
import { withDom } from "../testDom.ts";
let InviteAccept: typeof import("./InviteAccept.tsx").InviteAccept;
let NoTenant: typeof import("./InviteAccept.tsx").NoTenant;
let t: typeof import("../peopleTesting.tsx");
withDom(async () => { ({ InviteAccept, NoTenant } = await import("./InviteAccept.tsx")); t = await import("../peopleTesting.tsx"); });
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
test("a lookup that fails on the way (network, 5xx) offers a retry, not \"can't be used\"; a 410 is unusable", async () => {
  let fail = true;
  const p = await t.mountAs("invited", "/invite/inv_7Rk2Qm9xPa", <InviteAccept code="inv_7Rk2Qm9xPa" />, {
    extra: (path) => (path.startsWith("/v1/invitations/by-code") && fail ? new Response("upstream down", { status: 502 }) : undefined),
  });
  try {
    await p.until(() => p.button("Try again") != null, "the retry");
    expect(p.text()).not.toContain("can't be used");
    fail = false;
    await t.click(p.button("Try again"));
    await p.until(() => p.button("Join acme") != null, "the join offer after the retry");
  } finally { await p.done(); }
  const gone = await t.mountAs("invited", "/invite/nope", <InviteAccept code="nope" />);
  try { await gone.until(() => gone.text().includes("can't be used"), "unusable"); expect(gone.button("Try again")).toBeUndefined(); } finally { await gone.done(); }
});
