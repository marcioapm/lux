import { expect, test } from "bun:test";
import type { WhoAmI } from "../api/types.ts";
import { atLeast, canActOnRun, viewerOf, whoCanAct } from "./identity.ts";

const base: WhoAmI = { operator: false, tenant: "acme", tenantId: "ten_acme", scopes: ["read", "run"], consoleAuth: "cloudflare-access" };
const sas = [{ id: "sa_ci", name: "ci-review-bot", owner: { id: "usr_car", email: "carla@acme.dev", name: "Carla Santos", operator: false } }];

test("roles: members act on their own and their service accounts' Runs, admins on all, viewers on none", () => {
  const carla = viewerOf({ ...base, userId: "usr_car", role: "member", memberships: [{ tenantId: "ten_acme", tenant: "acme", role: "member" }] });
  expect(canActOnRun(carla, { owner: { kind: "user", id: "usr_car", name: "Carla" } })).toBe(true);
  expect(canActOnRun(carla, { owner: { kind: "serviceAccount", id: "sa_ci", name: "ci-review-bot" } }, sas)).toBe(true);
  expect(canActOnRun(carla, { owner: { kind: "user", id: "usr_bru", name: "Bruno" } })).toBe(false);
  expect(canActOnRun(carla, {})).toBe(false);
  const viewer = viewerOf({ ...base, userId: "usr_dio", role: "viewer", memberships: [] });
  expect(canActOnRun(viewer, { owner: { kind: "user", id: "usr_dio", name: "Diogo" } })).toBe(false);
  const admin = viewerOf({ ...base, userId: "usr_ana", role: "admin", memberships: [] });
  expect(canActOnRun(admin, {})).toBe(true);
  expect(atLeast(admin, "owner")).toBe(false);
});

test("the role follows the tenant in scope", () => {
  const marcio = viewerOf({ ...base, operator: false, userId: "usr_mar", role: "owner", memberships: [{ tenantId: "ten_acme", tenant: "acme", role: "owner" }, { tenantId: "ten_globex", tenant: "globex", role: "admin" }] }, "ten_globex");
  expect(marcio.role).toBe("admin");
});

test("an older luxd (no role, no memberships) falls back to scopes", () => {
  const legacy = viewerOf({ ...base, scopes: ["read"] });
  expect(legacy.legacy).toBe(true);
  expect(atLeast(legacy, "member")).toBe(false);
  expect(canActOnRun(viewerOf({ ...base }), {})).toBe(true);
  expect(legacy.noTenant).toBe(false);
});

test("no tenant: signed in with no membership, not an operator; invitations only with Access", () => {
  expect(viewerOf({ ...base, memberships: [] }).noTenant).toBe(true);
  expect(viewerOf({ ...base, operator: true, memberships: [] }).noTenant).toBe(false);
  expect(viewerOf({ ...base, invitationsEnabled: true }).invitationsEnabled).toBe(true);
  expect(viewerOf({ ...base, consoleAuth: "key", invitationsEnabled: true }).invitationsEnabled).toBe(false);
});

test("whoCanAct names the owner, or a service account's owner", () => {
  expect(whoCanAct({ owner: { kind: "user", id: "u", name: "Bruno Costa" } })).toBe("Bruno Costa");
  expect(whoCanAct({ owner: { kind: "serviceAccount", id: "sa_ci", name: "ci-review-bot" } }, sas)).toBe("Carla Santos (owner of ci-review-bot)");
  expect(whoCanAct({})).toBeNull();
});
