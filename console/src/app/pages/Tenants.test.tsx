import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Tenant } from "../../api/index.ts";

// The page's imports (the router) touch window at load: register the DOM first.
let Tenants: typeof import("./Tenants.tsx").Tenants;
let ScopeProvider: typeof import("../scope.tsx").ScopeProvider;
let fakeApi: typeof import("../testing.ts").fakeApi;
let api: typeof import("../../api/index.ts");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ Tenants } = await import("./Tenants.tsx"));
  ({ ScopeProvider } = await import("../scope.tsx"));
  ({ fakeApi } = await import("../testing.ts"));
  api = await import("../../api/index.ts");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const sleep = (ms: number) => act(() => new Promise<void>((r) => setTimeout(r, ms)));

/** Polls (10 ms steps, inside act) until ok() holds; fails after 5 s. */
async function until(ok: () => boolean, what: string) {
  for (const end = Date.now() + 5000; !ok(); ) {
    if (Date.now() > end) throw new Error(`never: ${what}`);
    await sleep(10);
  }
}

/** n's text as a reader gets it: without aria-hidden decoration (a sort arrow). */
function visibleText(n: Element): string {
  const c = n.cloneNode(true) as Element;
  for (const hidden of c.querySelectorAll('[aria-hidden="true"]')) hidden.remove();
  return c.textContent?.trim() ?? "";
}

const tenant = (id: string, expireAfterDays: number): Tenant => ({
  id,
  name: id,
  retentionDays: 30,
  expireAfterDays,
  activeRuns: 0,
  runs: 0,
  hosts: 0,
  storedBytes: 0,
  createdAt: "2026-01-01T00:00:00Z",
});

/** The Tenants page as an operator, /v1/tenants answering tenants. */
async function render(tenants: Tenant[], answer?: (path: string, method: string) => unknown) {
  api.signIn("k");
  api.setWhoAmI({ operator: true, tenant: "", tenantId: "", scopes: ["read", "run", "admin"], consoleAuth: "cloudflare-access", memberships: [], invitationsEnabled: true });
  const fake = fakeApi((path, method) => answer?.(path, method) ?? (path.startsWith("/v1/tenants") ? { tenants } : {}));
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL("http://localhost/tenants");
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <ScopeProvider>
        <Tenants />
      </ScopeProvider>,
    ),
  );
  const table = () => el.querySelector("table");
  // Column headers and body rows by the elements carrying those table roles
  // (th: columnheader, tbody tr: row, td: cell), never the design system's
  // wrapper classes.
  const headerCells = () => [...(table()?.querySelectorAll("thead th") ?? [])];
  const headers = () => headerCells().map(visibleText);
  const bodyRows = () => [...(table()?.querySelectorAll("tbody tr") ?? [])];
  const cellTexts = () => bodyRows().map((tr) => [...tr.querySelectorAll("td")].map((td) => visibleText(td)));
  const done = async () => {
    await act(async () => root.unmount());
    el.remove();
    fake.restore();
    api.signOut();
  };
  try {
    await until(() => cellTexts().some((cells) => cells.includes(tenants[0]!.name)), `a row for ${tenants[0]!.name}`);
  } catch (e) {
    await done();
    throw e;
  }
  return {
    // Each row's tenant name and Expiry cell, in the order shown.
    expiries: () => {
      const name = headers().indexOf("Tenant");
      const exp = headers().indexOf("Expiry");
      return cellTexts().map((cells) => [cells[name], cells[exp]]);
    },
    sortByExpiry: async () => {
      const th = headerCells().find((h) => visibleText(h) === "Expiry") as HTMLElement;
      await act(async () => th.click());
    },
    /** Each row as header → cell text. */
    cells: () => cellTexts().map((cells) => Object.fromEntries(headers().map((h, i) => [h, cells[i]]))),
    dialogText: () => [...el.querySelectorAll("dialog")].filter((d) => d.open).map((d) => visibleText(d)).join(" "),
    create: async (name: string, email: string) => {
      const btn = (root: ParentNode, label: string) => [...root.querySelectorAll("button")].find((b) => visibleText(b) === label) as HTMLButtonElement;
      await act(async () => btn(el, "New tenant").click());
      const d = [...el.querySelectorAll("dialog")].find((x) => x.open)!;
      const [n, e] = [...d.querySelectorAll<HTMLInputElement>("input")];
      const { fill } = await import("../peopleTesting.tsx");
      for (const [input, v] of [[n!, name], [e!, email]] as const) await fill(input, v);
      const b = btn(d, "Create and get link");
      if (!b) throw new Error(`no Create and get link in: ${[...d.querySelectorAll("button")].map((x) => visibleText(x)).join(" | ")}; inputs ${[...d.querySelectorAll("input")].map((i) => i.value).join(",")}`);
      await act(async () => b.click());
    },
    done,
  };
}

test("the Tenants page shows each tenant's expiry, never for 0", async () => {
  const p = await render([tenant("acme", 90), tenant("brisk", 7), tenant("keep", 0)]);
  try {
    expect(p.expiries()).toEqual([
      ["acme", "90d"],
      ["brisk", "7d"],
      ["keep", "never"],
    ]);
  } finally {
    await p.done();
  }
});

test("sorted by Expiry, never is the longest", async () => {
  const p = await render([tenant("keep", 0), tenant("acme", 90), tenant("brisk", 7)]);
  try {
    await p.sortByExpiry();
    expect(p.expiries().map((r) => r[0])).toEqual(["keep", "acme", "brisk"]);
    await p.sortByExpiry();
    expect(p.expiries().map((r) => r[0])).toEqual(["brisk", "acme", "keep"]);
  } finally {
    await p.done();
  }
});

// GET /v1/tenants as luxd answers it (TestTenantPeopleContract): owners are {id, name, email}, with the counts.
test("Owners and Members come from owners/ownersCount/membersCount; ownersCount 0 is No owner", async () => {
  const p = await render([
    { ...tenant("acme", 90), owners: [{ id: "usr_owner", name: "owner person", email: "olive@example.com" }, { id: "usr_noname", name: "", email: "zed@example.com" }], ownersCount: 2, membersCount: 7 },
    { ...tenant("bare", 90), owners: [], ownersCount: 0, membersCount: 0 },
    tenant("old", 90),
  ]);
  try {
    const rows = p.cells();
    expect(rows.find((r) => r.Tenant === "acme")).toMatchObject({ Owners: "owner person, zed@example.com", Members: "7" });
    expect(rows.find((r) => r.Tenant === "bare")).toMatchObject({ Owners: "No owner", Members: "0" });
    expect(rows.find((r) => r.Tenant === "old")).toMatchObject({ Owners: "–", Members: "–" });
    // Each owner links to their page in that tenant (not the selected one).
    const links = [...document.querySelectorAll<HTMLAnchorElement>("tbody a")].map((a) => [a.textContent, a.getAttribute("href")]);
    expect(links).toEqual([
      ["owner person", "/members/usr_owner?tenant=acme"],
      ["zed@example.com", "/members/usr_noname?tenant=acme"],
    ]);
  } finally {
    await p.done();
  }
});

// POST /v1/tenants answers {id, tenantId, name, invitation?} (no tenant object).
test("New tenant with a first owner shows that invitation's link, named for the new tenant", async () => {
  const created = { id: "ten_new", tenantId: "ten_new", name: "newco", invitation: { id: "inv_1", email: "cto@newco.dev", role: "owner", status: "pending", code: "c", url: "http://localhost/invite/c", createdAt: "2026-11-05T00:00:00Z", expiresAt: "2026-11-19T00:00:00Z" } };
  const p = await render([tenant("acme", 90)], (path, method) => (path.startsWith("/v1/tenants") && method === "POST" ? created : undefined));
  try {
    await p.create("newco", "cto@newco.dev");
    await until(() => p.dialogText().includes("Send newco's Owner invitation"), "the link dialog");
    expect(p.dialogText()).toContain("http://localhost/invite/c");
    expect(p.dialogText()).toContain("cto@newco.dev");
  } finally {
    await p.done();
  }
});
