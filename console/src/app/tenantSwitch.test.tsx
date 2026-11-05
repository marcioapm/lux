import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";

// A person (not an operator) in two tenants: the tenant picker offers
// their memberships, and picking one asks whoami again for that tenant,
// so the role (and with it every control) is the one held there.
let App: typeof import("./App.tsx").App;
let api: typeof import("../api/index.ts");
let testing: typeof import("./testing.ts");
let mock: typeof import("../../mockPeople.ts");
let people: typeof import("../../mockPeopleApi.ts");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ App } = await import("./App.tsx"));
  api = await import("../api/index.ts");
  testing = await import("./testing.ts");
  mock = await import("../../mockPeople.ts");
  people = await import("../../mockPeopleApi.ts");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

test("a two-tenant member switches tenant in the picker; whoami is asked again for it and the role follows", async () => {
  mock.resetPeople();
  const key = "luxk_mock_multi";
  api.signIn(key);
  const headers = { get: (k: string) => (k.toLowerCase() === "authorization" ? `Bearer ${key}` : null) };
  const fake = testing.fakeApi(async (path, method, body) => {
    const u = new URL(`http://localhost${path}`);
    const req = { method, headers, json: async () => body ?? {} } as unknown as Request;
    const c = mock.callerOf(req, u, false);
    if (u.pathname === "/v1/whoami") return mock.whoamiFor(c!, "cloudflare-access", null);
    const r = await people.peopleApi(req, u, c, "cloudflare-access");
    if (r) return new Response(JSON.stringify(r.body), { status: r.status, headers: { "Content-Type": "application/json" } });
    if (u.pathname === "/v1/runs") return { runs: [] };
    if (u.pathname === "/v1/status") return { runs: {} };
    return {};
  });
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL("http://localhost/members");
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () => root.render(<App />));
  const whoamis = () => fake.calls.filter((c) => c.startsWith("/v1/whoami"));
  const picker = () => [...el.querySelectorAll<HTMLButtonElement>(".select-trigger")].find((b) => b.textContent?.includes("Tenant"));
  try {
    await testing.until(() => picker() != null, "the tenant picker", 3000);
    // whoami resolved Rui to his first membership (globex, viewer): no Invite people.
    await testing.until(() => el.textContent?.includes("Sam Lee") === true || el.textContent?.includes("Rui Alves") === true, "globex's members", 3000);
    expect(picker()!.textContent).toContain("globex");
    expect([...el.querySelectorAll("button")].some((b) => b.textContent?.trim() === "Invite people")).toBe(false);
    await act(async () => picker()!.click());
    const options = [...el.querySelectorAll<HTMLElement>('[role="option"]')];
    expect(options.map((o) => o.querySelector(".tenant-opt span")?.textContent)).toEqual(["globex", "initech"]);
    expect(options.some((o) => o.textContent?.includes("All tenants"))).toBe(false);
    await act(async () => options.find((o) => o.textContent?.includes("initech"))!.click());
    await testing.until(() => whoamis().some((c) => c.includes("tenant=ten_initech")), "whoami for initech", 3000);
    expect(new URL(window.location.href).searchParams.get("tenant")).toBe("ten_initech");
    // Admin in initech: Invite people is there.
    await testing.until(() => [...el.querySelectorAll("button")].some((b) => b.textContent?.trim() === "Invite people"), "admin controls in initech", 3000);
    expect(fake.calls.some((c) => c.startsWith("/v1/members") && c.includes("tenant=ten_initech"))).toBe(true);
  } finally {
    await act(async () => root.unmount());
    el.remove();
    fake.restore();
    api.signOut();
  }
});
