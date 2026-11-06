import { expect, mock, test } from "bun:test";
import { withDom } from "../testDom.ts";

// The share column computes each currency's total once, not per row.
// sumMoney is counted through a module
// mock: the amounts it is handed stay O(rows).
let t: typeof import("../peopleTesting.tsx");
withDom(async () => {
  t = await import("../peopleTesting.tsx");
});

test("MemberCost at 3,000 rows: sumMoney sees O(rows) amounts, not O(rows²)", async () => {
  const ds = await import("@lux/design-system");
  let calls = 0;
  let summed = 0;
  const real = ds.sumMoney;
  mock.module("@lux/design-system", () => ({ ...ds, sumMoney: (a: readonly string[]) => (calls++, (summed += a.length), real(a)) }));
  try {
    const { MemberCost } = await import(`./MemberCost.tsx?n=${Date.now()}`);
    const many = Array.from({ length: 3000 }, (_, i) => ({ key: `user:u${i}`, group: { member: `user:u${i}` }, currency: i % 2 ? "USD" : "EUR", amount: `${i}.00`, runs: 1, owner: { kind: "user", id: `u${i}`, name: `P ${i}`, email: `p${i}@x.io` } }));
    const body = { from: "2026-10-06T00:00:00Z", to: "2026-11-05T00:00:00Z", basis: "list", totals: many };
    const p = await t.mountAs("viewer", "/?tab=cost&tenant=ten_acme", <MemberCost />, { extra: (path) => (path.startsWith("/v1/costs") ? body : undefined) });
    try {
      await p.until(() => p.el.querySelectorAll("tbody tr").length > 0, "rows");
      // A few renders, each summing every currency once: a bounded multiple of the rows.
      expect(summed).toBeLessThan(3000 * 10);
      expect(calls).toBeLessThan(100);
    } finally {
      await p.done();
    }
  } finally {
    mock.module("@lux/design-system", () => ds);
  }
});
