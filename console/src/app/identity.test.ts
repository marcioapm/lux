import { expect, test } from "bun:test";
import type { WhoAmI } from "../api/types.ts";
import { atLeast, canActOnRun, canActOnServer, canManageAccountKeys, canManageInfra, canManageOwnKeys, knowsTenant, mayChange, operatorActs, viewerOf, whoCanAct } from "./identity.ts";

const base: WhoAmI = { operator: false, tenant: "acme", tenantId: "ten_acme", scopes: ["read", "run"], consoleAuth: "cloudflare-access" };
const sas = [{ id: "sa_ci", name: "ci-review-bot", owner: { id: "usr_car", email: "carla@acme.dev", name: "Carla Santos", picture: "", operator: false } }];

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
  expect(atLeast(viewerOf({ ...base, scopes: ["read", "run", "admin"] }), "admin")).toBe(true);
  expect(canManageInfra(viewerOf({ ...base, scopes: ["read", "run", "admin"] }))).toBe(true);
  expect(canManageInfra(viewerOf({ ...base }))).toBe(false);
  expect(legacy.noTenant).toBe(false);
});

test("before whoami answers, nothing may change: no Run actions, no infra, reading only", () => {
  const unknown = viewerOf(null);
  expect(unknown.resolved).toBe(false);
  expect(unknown.legacy).toBe(false);
  expect(atLeast(unknown, "viewer")).toBe(true);
  expect(atLeast(unknown, "member")).toBe(false);
  expect(canActOnRun(unknown, {})).toBe(false);
  expect(canActOnRun(unknown, { owner: { kind: "user", id: "usr_car", name: "Carla" } })).toBe(false);
  expect(canManageInfra(unknown)).toBe(false);
  expect(canActOnServer(unknown, null)).toBe(false);
});

test("infra by role: viewers and members none, admins the tenant's, operators the platform's too", () => {
  const as = (role: "viewer" | "member" | "admin" | "owner") => viewerOf({ ...base, userId: "usr_x", role, memberships: [{ tenantId: "ten_acme", tenant: "acme", role }] });
  expect(canManageInfra(as("viewer"))).toBe(false);
  expect(canManageInfra(as("member"))).toBe(false);
  expect(canManageInfra(as("admin"))).toBe(true);
  expect(canManageInfra(as("owner"), true)).toBe(false);
  expect(canManageInfra(viewerOf({ ...base, operator: true, memberships: [] }), true)).toBe(true);
  const carla = viewerOf({ ...base, userId: "usr_car", role: "member", memberships: [] });
  expect(canActOnServer(carla, { owner: { kind: "user", id: "usr_car", name: "Carla" } })).toBe(true);
  expect(canActOnServer(carla, { owner: { kind: "user", id: "usr_bru", name: "Bruno" } })).toBe(false);
  expect(canActOnServer(carla, null)).toBe(false);
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

// whoami readOnly (http.go Principal.atLeast): a read-only key reads as its
// role and changes nothing, an operator's included.
test("a read-only key: every mutation capability is denied, keys included, operators too", () => {
  const ro = (w: Partial<WhoAmI>) => viewerOf({ ...base, readOnly: true, ...w });
  const admin = ro({ userId: "usr_ana", role: "admin", memberships: [{ tenantId: "ten_acme", tenant: "acme", role: "admin" }] });
  const op = ro({ operator: true, userId: "usr_mar", memberships: [] });
  const owner = ro({ userId: "usr_car", role: "member", memberships: [{ tenantId: "ten_acme", tenant: "acme", role: "member" }] });
  for (const v of [admin, op, owner]) {
    expect(v.readOnly).toBe(true);
    expect(mayChange(v)).toBe(false);
    expect(atLeast(v, "viewer")).toBe(true);
    expect(atLeast(v, "member")).toBe(false);
    expect(atLeast(v, "admin")).toBe(false);
    expect(canActOnRun(v, {})).toBe(false);
    expect(canActOnRun(v, { owner: { kind: "user", id: "usr_car", name: "Carla" } })).toBe(false);
    expect(canManageInfra(v)).toBe(false);
    expect(canManageInfra(v, true)).toBe(false);
    expect(canActOnServer(v, null)).toBe(false);
    expect(canManageOwnKeys(v)).toBe(false);
    expect(canManageAccountKeys(v, sas[0]!)).toBe(false);
    expect(operatorActs(v)).toBe(false);
  }
  // Not read-only: the same people may.
  const carla = viewerOf({ ...base, userId: "usr_car", role: "member", memberships: [] });
  expect(canManageOwnKeys(carla)).toBe(true);
  expect(canManageAccountKeys(carla, sas[0]!)).toBe(true);
  expect(operatorActs(viewerOf({ ...base, operator: true, memberships: [] }))).toBe(true);
});

// A Run or server of another tenant than the one in scope: the role there.
test("knowsTenant: whoami's own tenant or a membership answers; another tenant needs whoami?tenant=", () => {
  const rui: WhoAmI = { ...base, tenant: "globex", tenantId: "ten_globex", userId: "usr_rui", role: "viewer", memberships: [{ tenantId: "ten_globex", tenant: "globex", role: "viewer" }, { tenantId: "ten_initech", tenant: "initech", role: "admin" }] };
  expect(knowsTenant(rui, "globex")).toBe(true);
  expect(knowsTenant(rui, "initech")).toBe(true);
  expect(viewerOf(rui, "initech").role).toBe("admin");
  expect(viewerOf(rui, "globex").role).toBe("viewer");
  expect(knowsTenant(rui, "acme")).toBe(false);
  expect(knowsTenant({ ...base, operator: true, memberships: [] }, "acme")).toBe(true);
  expect(knowsTenant(null, "acme")).toBe(false);
});
