import { afterAll, beforeAll, expect, test } from "bun:test";
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
