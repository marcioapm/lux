import { expect, test } from "bun:test";
import { withDom } from "../testDom.ts";
let MemberCost: typeof import("./MemberCost.tsx").MemberCost;
let t: typeof import("../peopleTesting.tsx");
withDom(async () => { ({ MemberCost } = await import("./MemberCost.tsx")); t = await import("../peopleTesting.tsx"); });
test("cost by member: service accounts separately, then folded into their owner; no owner stays", async () => {
  const p = await t.mountAs("viewer", "/?tab=cost", <MemberCost />);
  try {
    await p.until(() => p.text().includes("ci-review-bot"), "cost rows");
    expect(p.text()).toContain("(no owner)");
    expect(p.el.querySelectorAll(".meter").length).toBeGreaterThan(0);
    await t.click(p.el.querySelectorAll('[role="radio"]')[1]);
    await p.until(() => !p.text().includes("ci-review-bot"), "folded account");
    expect(p.text()).toContain("$601.17");
    expect(p.text()).toContain("(no owner)");
  } finally { await p.done(); }
});

// The totals TestCostByMemberContract (internal/server/contract_test.go) logs for by=member&runs=true, verbatim.
const SERVER_TOTALS = [{"amount":"100","currency":"USD","group":{"member":"(none)"},"key":"(none)","runs":1},{"amount":"10","currency":"USD","group":{"member":"sa:sa_56c49b9eef3a9763"},"key":"sa:sa_56c49b9eef3a9763","owner":{"id":"sa_56c49b9eef3a9763","kind":"serviceAccount","name":"k"},"runs":1},{"amount":"2","currency":"USD","group":{"member":"user:usr_cost"},"key":"user:usr_cost","owner":{"email":"cost@example.com","id":"usr_cost","kind":"user","name":"Cost Member"},"runs":1}];

test("cost by member reads luxd's CostSummary rows (key, owner, runs) and asks for runs=true", async () => {
  const body = { from: "2026-10-06T00:00:00Z", to: "2026-11-05T00:00:00Z", basis: "list", totals: SERVER_TOTALS };
  const p = await t.mountAs("viewer", "/?tab=cost", <MemberCost />, { extra: (path) => (path.startsWith("/v1/costs") ? body : undefined) });
  try {
    await p.until(() => p.text().includes("Cost Member"), "rows");
    const call = new URL(`http://x${p.fake.calls.find((c) => c.startsWith("/v1/costs"))!}`);
    expect(call.searchParams.get("by")).toBe("member");
    expect(call.searchParams.get("runs")).toBe("true");
    const rows = [...p.el.querySelectorAll("tbody tr")].map((r) => t.visibleText(r));
    const rowText = (name: string) => rows.find((r) => r.startsWith(name)) ?? `no row ${name} in ${rows.join(" | ")}`;
    expect(rowText("Cost Member")).toContain("cost@example.com");
    expect(rowText("Cost Member")).toMatch(/\b1\b.*\$2\.00/);
    expect(rowText("(no owner)")).toMatch(/\b1\b.*\$100\.00/);
    expect(rowText("k ")).toContain("service account");
    expect(rowText("k ")).toContain("$10.00");
  } finally { await p.done(); }
});
