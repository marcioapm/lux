import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
let MemberCost: typeof import("./MemberCost.tsx").MemberCost;
let t: typeof import("../peopleTesting.tsx");
beforeAll(async () => { GlobalRegistrator.register(); ({ MemberCost } = await import("./MemberCost.tsx")); t = await import("../peopleTesting.tsx"); (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(async () => { await GlobalRegistrator.unregister(); });
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
