import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";

// Member cost per principal and currency: summed per currency with
// sumMoney, shown as a MoneyList, shares within a currency; never one
// currency's code on another's amounts ("EUR 152.05").
let Members: typeof import("./Members.tsx").Members;
let MemberPage: typeof import("./MemberPage.tsx").MemberPage;
let SA: typeof import("./ServiceAccounts.tsx");
let Profile: typeof import("./Profile.tsx").Profile;
let MemberCost: typeof import("./MemberCost.tsx").MemberCost;
let money: typeof import("./memberMoney.ts");
let t: typeof import("../peopleTesting.tsx");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ Members } = await import("./Members.tsx"));
  ({ MemberPage } = await import("./MemberPage.tsx"));
  SA = await import("./ServiceAccounts.tsx");
  ({ Profile } = await import("./Profile.tsx"));
  ({ MemberCost } = await import("./MemberCost.tsx"));
  money = await import("./memberMoney.ts");
  t = await import("../peopleTesting.tsx");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const row = (key: string, currency: string, amount: string, runs: number, owner?: object) => ({ key, group: { member: key }, currency, amount, runs, ...(owner ? { owner } : {}) });
// Carla (usr_car): USD and EUR of her own. Her account ci-review-bot (sa_7cr3b2x9): USD and EUR.
const TOTALS = [
  row("user:usr_car", "USD", "100.25", 3, { kind: "user", id: "usr_car", name: "Carla Santos", email: "carla@acme.dev" }),
  row("user:usr_car", "EUR", "40.10", 2, { kind: "user", id: "usr_car", name: "Carla Santos", email: "carla@acme.dev" }),
  row("sa:sa_7cr3b2x9", "USD", "120.00", 5, { kind: "serviceAccount", id: "sa_7cr3b2x9", name: "ci-review-bot" }),
  row("sa:sa_7cr3b2x9", "EUR", "32.05", 1, { kind: "serviceAccount", id: "sa_7cr3b2x9", name: "ci-review-bot" }),
  row("user:usr_bru", "USD", "300.00", 4, { kind: "user", id: "usr_bru", name: "Bruno Costa", email: "bruno@acme.dev" }),
];
const costs = (path: string) => (path.startsWith("/v1/costs") ? { from: "2026-10-06T00:00:00Z", to: "2026-11-05T00:00:00Z", basis: "list", totals: TOTALS } : undefined);
const BAD = /EUR\s?152\.05|€152\.05|\$152\.05|€220|\$220\.25|\$260\.35/;

test("Members: a person with USD and EUR shows both, never added", async () => {
  const p = await t.mountAs("admin", "/members?tenant=ten_acme", <Members />, { extra: costs });
  try {
    await p.until(() => p.text().includes("Carla Santos") && p.text().includes("€40.10"), "Carla's EUR");
    const carla = [...p.el.querySelectorAll("tbody tr")].map((r) => t.visibleText(r)).find((r) => r.startsWith("Carla Santos"))!;
    expect(carla).toContain("$100.25");
    expect(carla).toContain("€40.10");
    expect(p.text()).not.toMatch(BAD);
  } finally {
    await p.done();
  }
});

test("A member's page: own USD+EUR, and their account's USD+EUR per currency (no EUR 152.05)", async () => {
  const p = await t.mountAs("admin", "/members/usr_car?tenant=ten_acme", <MemberPage id="usr_car" />, { extra: costs });
  try {
    await p.until(() => p.text().includes("more via service accounts") && p.text().includes("€32.05"), "the via-accounts line");
    const tile = [...p.el.querySelectorAll(".stat-tile, .stat")].map((x) => t.visibleText(x)).find((x) => x.startsWith("Cost · 30d"))!;
    expect(tile).toContain("$100.25");
    expect(tile).toContain("€40.10");
    expect(tile).toContain("$120.00");
    expect(tile).toContain("€32.05");
    expect(p.text()).not.toMatch(BAD);
  } finally {
    await p.done();
  }
});

test("Service accounts list and page: both currencies; the share only within one currency", async () => {
  const l = await t.mountAs("admin", "/service-accounts?tenant=ten_acme", <SA.ServiceAccounts />, { extra: costs });
  try {
    await l.until(() => l.text().includes("€32.05"), "the account's EUR");
    const bot = [...l.el.querySelectorAll("tbody tr")].map((r) => t.visibleText(r)).find((r) => r.startsWith("ci-review-bot"))!;
    expect(bot).toContain("$120.00");
    expect(bot).toContain("€32.05");
    expect(l.text()).not.toMatch(BAD);
  } finally {
    await l.done();
  }
  const d = await t.mountAs("admin", "/service-accounts/sa_7cr3b2x9?tenant=ten_acme", <SA.ServiceAccountPage id="sa_7cr3b2x9" />, { extra: costs });
  try {
    await d.until(() => d.text().includes("€32.05"), "the account's EUR");
    expect(d.text()).toContain("$120.00");
    expect(d.text()).not.toMatch(BAD);
    // Two currencies: no single "% of" share.
    expect(d.text()).not.toMatch(/% of acme/);
  } finally {
    await d.done();
  }
});

test("Profile: my USD and EUR", async () => {
  const p = await t.mountAs("member", "/me?tenant=ten_acme", <Profile />, { extra: costs });
  try {
    await p.until(() => p.text().includes("€40.10"), "my EUR");
    expect(p.text()).toContain("$100.25");
    expect(p.text()).not.toMatch(BAD);
  } finally {
    await p.done();
  }
});

test("Overview cost by member: a share is within its currency (Carla's EUR is 40.10 of 72.15)", async () => {
  const p = await t.mountAs("viewer", "/?tab=cost&tenant=ten_acme", <MemberCost />, { extra: costs });
  try {
    await p.until(() => p.text().includes("€40.10"), "rows");
    const share = (amount: string) => [...p.el.querySelectorAll("tbody tr")].find((r) => t.visibleText(r).includes(amount))!.querySelector(".meter-text")!.textContent;
    expect(p.el.querySelectorAll("tbody tr").length).toBe(5);
    expect(share("€40.10")).toBe(`${Math.round((40.1 / 72.15) * 100)}%`);
    expect(share("€32.05")).toBe(`${Math.round((32.05 / 72.15) * 100)}%`);
    expect(share("$300.00")).toBe(`${Math.round((300 / 520.25) * 100)}%`);
  } finally {
    await p.done();
  }
});

test("principalCost and totals: exact per-currency sums", () => {
  const c = money.principalCost([row("k", "USD", "0.10", 1), row("k", "USD", "0.20", 1), row("k", "EUR", "1.005", 2)] as never);
  expect(c.amounts).toEqual([{ currency: "EUR", amount: "1.005" }, { currency: "USD", amount: "0.3" }]);
  expect(money.totalsByCurrency(TOTALS as never)).toEqual(new Map([["USD", 520.25], ["EUR", 72.15]]));
});

test("Runs: within one principal the largest currency's count, across principals added", () => {
  // Two accounts, different currencies: distinct Runs, 3 + 5.
  const two = new Map([
    ["sa:a", [row("sa:a", "USD", "1.00", 3)]],
    ["sa:b", [row("sa:b", "EUR", "2.00", 5)]],
  ]) as never;
  expect(money.keysCost(two, ["sa:a", "sa:b"]).runs).toBe(8);
  // One account whose 4 Runs are billed in USD and EUR: 4, not 8.
  const one = new Map([["sa:a", [row("sa:a", "USD", "1.00", 4), row("sa:a", "EUR", "2.00", 4)]]]) as never;
  expect(money.keysCost(one, ["sa:a"]).runs).toBe(4);
  expect(money.keysCost(one, ["sa:a"]).amounts).toEqual([{ currency: "EUR", amount: "2" }, { currency: "USD", amount: "1" }]);
});

test("A member's page: Runs via two accounts in different currencies are added (+ 8, not + 5)", async () => {
  // Ana owns nightly-evals (3 USD Runs) and grafana-reader (5 EUR Runs).
  const rows = [
    row("user:usr_ana", "USD", "10.00", 2, { kind: "user", id: "usr_ana", name: "Ana Ribeiro", email: "ana@acme.dev" }),
    row("sa:sa_2ne9v1q5", "USD", "30.00", 3, { kind: "serviceAccount", id: "sa_2ne9v1q5", name: "nightly-evals" }),
    row("sa:sa_9gr1d7k3", "EUR", "50.00", 5, { kind: "serviceAccount", id: "sa_9gr1d7k3", name: "grafana-reader" }),
  ];
  const extra = (path: string) => (path.startsWith("/v1/costs") ? { from: "2026-10-06T00:00:00Z", to: "2026-11-05T00:00:00Z", basis: "list", totals: rows } : undefined);
  const p = await t.mountAs("admin", "/members/usr_ana?tenant=ten_acme", <MemberPage id="usr_ana" />, { extra });
  try {
    await p.until(() => p.text().includes("€50.00"), "the accounts' cost");
    const tile = [...p.el.querySelectorAll(".stat-tile, .stat")].map((x) => t.visibleText(x)).find((x) => x.startsWith("Runs · 30d"))!;
    expect(tile).toContain("2");
    expect(tile).toMatch(/\+ 8 by /);
  } finally {
    await p.done();
  }
});
