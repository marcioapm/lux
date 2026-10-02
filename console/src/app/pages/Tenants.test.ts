import { expect, test } from "bun:test";
import { expiry } from "./Tenants.tsx";

test("a tenant's expiry reads in days, never for 0", () => {
  expect([90, 7, 0].map(expiry)).toEqual(["90d", "7d", "never"]);
});
