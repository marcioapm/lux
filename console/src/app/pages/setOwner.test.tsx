import { expect, test } from "bun:test";
import { withDom } from "../testDom.ts";

// Tenants › Set owner (operators): POST /v1/members {email, role: owner, upsert: true}
// in that tenant: a member is promoted, anyone else joins as Owner.
let Tenants: typeof import("./Tenants.tsx").Tenants;
let t: typeof import("../peopleTesting.tsx");
let mock: typeof import("../../../mockPeople.ts");
let tenantPeople: typeof import("../../../mockPeopleApi.ts").tenantPeople;
withDom(async () => {
  ({ Tenants } = await import("./Tenants.tsx"));
  t = await import("../peopleTesting.tsx");
  mock = await import("../../../mockPeople.ts");
  ({ tenantPeople } = await import("../../../mockPeopleApi.ts"));
});

const tenants = (path: string, method: string) =>
  path.startsWith("/v1/tenants") && method === "GET"
    ? { tenants: mock.TENANTS.map((x) => ({ id: x.id, name: x.name, retentionDays: 30, expireAfterDays: 0, activeRuns: 0, runs: 0, hosts: 0, storedBytes: 0, createdAt: "2026-01-01T00:00:00Z", ...tenantPeople(x.id) })) }
    : undefined;
const rowOf = (el: HTMLElement, name: string) => [...el.querySelectorAll("tbody tr")].find((tr) => tr.querySelector("td")?.textContent === name)!;

async function setOwner(p: Awaited<ReturnType<typeof t.mountAs>>, email: string, pick?: string) {
  const input = p.dialog()!.querySelector<HTMLInputElement>('input[role="combobox"]')!;
  if (pick) {
    await t.fill(input, pick);
    await p.until(() => p.dialog()!.querySelectorAll('[role="option"]').length > 0, "suggestions");
    const opt = [...p.dialog()!.querySelectorAll('[role="option"]')].find((o) => o.textContent?.includes(email))!;
    const { act } = await import("react");
    await act(async () => opt.dispatchEvent(new MouseEvent("mousedown", { bubbles: true })));
  } else await t.fill(input, email);
}

test("Set owner, a member: suggested from the tenant's members, promoted with one upsert", async () => {
  const p = await t.mountAs("operator", "/tenants", <Tenants />, { extra: tenants });
  try {
    await p.until(() => rowOf(p.el, "initech") != null, "initech");
    expect(t.visibleText(rowOf(p.el, "initech"))).toContain("No owner");
    // The No owner badge opens the same dialog.
    await t.click(p.button("No owner: set initech's owner"));
    await p.until(() => p.dialog()?.textContent?.includes("Set initech's owner") === true, "the dialog");
    await p.until(() => p.fake.calls.some((c) => c === "/v1/members?tenant=ten_initech"), "its members");
    await setOwner(p, "rui@acme.dev", "rui");
    expect(p.dialog()!.textContent).toContain("Rui Alves is Admin here: becomes Owner");
    await t.click(t.dialogButton(p.dialog(), "Make owner"));
    await p.until(() => p.fake.posts.some((x) => x.method === "POST"), "the POST");
    expect(p.fake.posts.find((x) => x.method === "POST")).toMatchObject({ path: "/v1/members?tenant=ten_initech", body: { email: "rui@acme.dev", role: "owner", upsert: true } });
    await p.until(() => t.visibleText(rowOf(p.el, "initech")).includes("Rui Alves"), "Rui the owner");
  } finally {
    await p.done();
  }
});

test("Set owner, not a member: joins as Owner; an existing owner is not offered", async () => {
  const p = await t.mountAs("operator", "/tenants", <Tenants />, { extra: tenants });
  try {
    await p.until(() => rowOf(p.el, "acme") != null, "acme");
    await t.click(p.button("Set acme's owner"));
    await p.until(() => p.fake.calls.some((c) => c === "/v1/members?tenant=ten_acme"), "its members");
    await t.fill(p.dialog()!.querySelector<HTMLInputElement>('input[role="combobox"]'), "eva");
    await p.until(() => p.dialog()!.querySelectorAll('[role="option"]').length > 0, "suggestions");
    expect(p.dialog()!.querySelector('[role="option"]')!.getAttribute("aria-disabled")).toBe("true");
    await setOwner(p, "jonas@acme.dev");
    expect(p.dialog()!.textContent).toContain("Not a member of acme yet: joins and becomes Owner");
    await t.click(t.dialogButton(p.dialog(), "Make owner"));
    await p.until(() => p.fake.posts.some((x) => x.method === "POST"), "the POST");
    expect(p.fake.posts.find((x) => x.method === "POST")!.body).toEqual({ email: "jonas@acme.dev", role: "owner", upsert: true });
    await p.until(() => mock.MEMBERSHIPS.some((m) => m.tenantId === "ten_acme" && m.role === "owner" && mock.USERS.find((u) => u.id === m.userId)?.email === "jonas@acme.dev"), "jonas an owner");
  } finally {
    await p.done();
  }
});

test("Set owner works in key console mode; it is not there for a read-only operator key", async () => {
  const p = await t.mountAs("operator", "/tenants", <Tenants />, { extra: tenants, auth: "key" });
  try {
    await p.until(() => rowOf(p.el, "acme") != null, "acme");
    expect(p.button("Set acme's owner")).toBeDefined();
  } finally {
    await p.done();
  }
  const ro = await t.mountAs("opreadonly", "/tenants", <Tenants />, { extra: tenants });
  try {
    await ro.until(() => rowOf(ro.el, "initech") != null, "initech");
    expect(ro.button("Set acme's owner")).toBeUndefined();
    expect(ro.button("No owner: set initech's owner")).toBeUndefined();
    expect(t.visibleText(rowOf(ro.el, "initech"))).toContain("No owner");
  } finally {
    await ro.done();
  }
});
