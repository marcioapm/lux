import { expect, test } from "bun:test";
import { withDom } from "../testDom.ts";
let People: typeof import("./People.tsx").People;
let t: typeof import("../peopleTesting.tsx");
withDom(async () => { ({ People } = await import("./People.tsx")); t = await import("../peopleTesting.tsx"); });
test("People lists platform accounts with memberships and operator labels", async () => {
  const p = await t.mountAs("operator", "/people", <People />);
  try { await p.until(() => p.text().includes("Márcio Martins"), "people"); expect(p.text()).toContain("globex · Admin"); expect(p.text()).toContain("Operator"); expect(p.el.querySelectorAll("tbody tr").length).toBe(7); } finally { await p.done(); }
});
