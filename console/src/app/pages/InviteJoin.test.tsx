import { afterAll, beforeAll, expect, test } from "bun:test";
import { act, useState } from "react";
import { GlobalRegistrator } from "@happy-dom/global-registrator";

// Accepting lands in the joined tenant: whoami?tenant=<accepted tenantId>,
// then /members?tenant=<it>, never the tenant the person was in before.
let InviteAccept: typeof import("./InviteAccept.tsx").InviteAccept;
let t: typeof import("../peopleTesting.tsx");
let api: typeof import("../../api/index.ts");
let mock: typeof import("../../../mockPeople.ts");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ InviteAccept } = await import("./InviteAccept.tsx"));
  t = await import("../peopleTesting.tsx");
  api = await import("../../api/index.ts");
  mock = await import("../../../mockPeople.ts");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

test("Join: whoami for the accepted tenant, then its Members page (Rui, in globex, joins acme)", async () => {
  // Rui (mock "multi") is in globex and initech; an invitation to acme for him.
  const p = await t.mountAs("multi", "/invite/inv_rui_acme?tenant=ten_globex", <InviteAccept code="inv_rui_acme" />, {
    extra: (path) => {
      if (path.startsWith("/v1/invitations/by-code/inv_rui_acme/accept")) {
        mock.MEMBERSHIPS.push({ tenantId: "ten_acme", userId: "usr_rui", role: "member", joined: 0 });
        return { tenantId: "ten_acme", tenant: "acme", role: "member" };
      }
      if (path.startsWith("/v1/invitations/by-code/inv_rui_acme")) return { tenant: "acme", role: "member", invitedBy: { name: "Ana Ribeiro" }, expiresAt: "2026-11-20T00:00:00Z", status: "pending", emailMatches: true };
      if (path.startsWith("/v1/whoami")) {
        // As mock.ts answers it: for the tenant asked (?tenant=), else the first membership.
        const req = { headers: { get: (k: string) => (k.toLowerCase() === "authorization" ? "Bearer luxk_mock_multi" : null) } } as unknown as Request;
        return mock.whoamiFor(mock.callerOf(req, new URL(`http://localhost${path}`), false)!, "cloudflare-access", null);
      }
      return undefined;
    },
  });
  try {
    await p.until(() => p.button("Join acme") != null, "the join offer");
    await t.click(p.button("Join acme"));
    await p.until(() => window.location.pathname === "/members", "navigated to Members");
    const q = new URLSearchParams(window.location.search);
    expect(q.get("tenant")).toBe("ten_acme");
    const who = p.fake.calls.filter((c) => c.startsWith("/v1/whoami"));
    expect(who.at(-1)).toContain("tenant=ten_acme");
    expect(api.getSession().whoami?.tenantId).toBe("ten_acme");
    expect(api.getSession().whoami?.role).toBe("member");
  } finally {
    await p.done();
  }
});

test("Join, then whoami fails: the membership is kept; Retry reloads whoami only, never accept again, and lands", async () => {
  let whoamiDown = false;
  const p = await t.mountAs("multi", "/invite/inv_rui_acme?tenant=ten_globex", <InviteAccept code="inv_rui_acme" />, {
    extra: (path, method) => {
      if (path.startsWith("/v1/invitations/by-code/inv_rui_acme/accept")) {
        if (mock.MEMBERSHIPS.some((m) => m.tenantId === "ten_acme" && m.userId === "usr_rui")) {
          return new Response(JSON.stringify({ code: "invitation_unusable", message: "this invitation can't be used", status: 410 }), { status: 410, headers: { "Content-Type": "application/json" } });
        }
        mock.MEMBERSHIPS.push({ tenantId: "ten_acme", userId: "usr_rui", role: "member", joined: 0 });
        return { tenantId: "ten_acme", tenant: "acme", role: "member" };
      }
      if (path.startsWith("/v1/invitations/by-code/inv_rui_acme")) return { tenant: "acme", role: "member", invitedBy: { name: "Ana Ribeiro" }, expiresAt: "2026-11-20T00:00:00Z", status: "pending", emailMatches: true };
      if (path.startsWith("/v1/whoami")) {
        if (whoamiDown) return new Response(JSON.stringify({ error: { code: "unavailable", message: "luxd is restarting" } }), { status: 503, headers: { "Content-Type": "application/json" } });
        const req = { headers: { get: (k: string) => (k.toLowerCase() === "authorization" ? "Bearer luxk_mock_multi" : null) } } as unknown as Request;
        return mock.whoamiFor(mock.callerOf(req, new URL(`http://localhost${path}`), false)!, "cloudflare-access", null);
      }
      void method;
      return undefined;
    },
  });
  try {
    await p.until(() => p.button("Join acme") != null, "the join offer");
    whoamiDown = true;
    await t.click(p.button("Join acme"));
    await p.until(() => p.text().includes("Couldn't open acme yet."), "the joined-but-not-loaded message");
    expect(p.text()).toContain("You joined acme");
    expect(p.el.querySelector(".error-strip[role=alert]")?.textContent).toBe("luxd is restarting");
    expect(p.button("Join acme")).toBeUndefined();
    expect(window.location.pathname).toBe("/invite/inv_rui_acme");
    whoamiDown = false;
    await t.click(p.button("Retry"));
    await p.until(() => window.location.pathname === "/members", "navigated to Members");
    expect(new URLSearchParams(window.location.search).get("tenant")).toBe("ten_acme");
    expect(p.fake.posts.filter((c) => c.path.includes("/accept")).length).toBe(1);
    expect(p.fake.calls.filter((c) => c.startsWith("/v1/whoami")).at(-1)).toContain("tenant=ten_acme");
    expect(api.getSession().whoami?.tenantId).toBe("ten_acme");
  } finally {
    await p.done();
  }
});

/** Two invitations on one mounted route, as history navigation between /invite/A and /invite/B does. */
function TwoInvitations({ first, second }: { first: string; second: string }) {
  const [code, setCode] = useState(first);
  return <><button type="button" onClick={() => setCode(second)}>Open the other invitation</button><InviteAccept code={code} /></>;
}

const lookup = (path: string) =>
  path.startsWith("/v1/invitations/by-code/inv_rui_acme") && !path.includes("/accept") ? { tenant: "acme", role: "member", invitedBy: { name: "Ana Ribeiro" }, expiresAt: "2026-11-20T00:00:00Z", status: "pending", emailMatches: true }
  : path.startsWith("/v1/invitations/by-code/inv_rui_initech2") && !path.includes("/accept") ? { tenant: "initech2", role: "admin", invitedBy: { name: "Eva Moreira" }, expiresAt: "2026-11-20T00:00:00Z", status: "pending", emailMatches: true }
  : undefined;

test("Another invitation on the same route starts over: joined-but-not-loaded for acme does not carry over to the next one", async () => {
  const p = await t.mountAs("multi", "/invite/inv_rui_acme?tenant=ten_globex", <TwoInvitations first="inv_rui_acme" second="inv_rui_initech2" />, {
    extra: (path) => {
      if (path.startsWith("/v1/invitations/by-code/inv_rui_acme/accept")) return { tenantId: "ten_acme", tenant: "acme", role: "member" };
      if (path.startsWith("/v1/whoami")) return new Response(JSON.stringify({ error: { code: "unavailable", message: "luxd is restarting" } }), { status: 503, headers: { "Content-Type": "application/json" } });
      return lookup(path);
    },
  });
  try {
    await p.until(() => p.button("Join acme") != null, "the acme offer");
    await t.click(p.button("Join acme"));
    await p.until(() => p.text().includes("Couldn't open acme yet."), "acme joined, not loaded");
    await t.click(p.button("Open the other invitation"));
    await p.until(() => p.button("Join initech2") != null, "the initech2 offer");
    expect(p.text()).not.toContain("You joined acme");
    expect(p.button("Retry")).toBeUndefined();
    expect(p.fake.posts.filter((c) => c.path.includes("/accept")).length).toBe(1);
  } finally {
    await p.done();
  }
});

test("An accept that answers after its invitation was left lands nowhere", async () => {
  let answer: (v: unknown) => void = () => {};
  const p = await t.mountAs("multi", "/invite/inv_rui_acme?tenant=ten_globex", <TwoInvitations first="inv_rui_acme" second="inv_rui_initech2" />, {
    extra: (path) => {
      if (path.startsWith("/v1/invitations/by-code/inv_rui_acme/accept")) return new Promise((res) => { answer = res; });
      return lookup(path);
    },
  });
  try {
    await p.until(() => p.button("Join acme") != null, "the acme offer");
    await t.click(p.button("Join acme"));
    await t.click(p.button("Open the other invitation"));
    await p.until(() => p.button("Join initech2") != null, "the initech2 offer");
    const whoBefore = p.fake.calls.filter((c) => c.startsWith("/v1/whoami")).length;
    await act(async () => { answer({ tenantId: "ten_acme", tenant: "acme", role: "member" }); await new Promise((r) => setTimeout(r, 50)); });
    expect(window.location.pathname).toBe("/invite/inv_rui_acme");
    expect(p.fake.calls.filter((c) => c.startsWith("/v1/whoami")).length).toBe(whoBefore);
    expect(p.button("Join initech2")).toBeDefined();
  } finally {
    await p.done();
  }
});
