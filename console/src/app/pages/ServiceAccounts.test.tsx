import { expect, test } from "bun:test";
import { withDom } from "../testDom.ts";
let ServiceAccounts: typeof import("./ServiceAccounts.tsx").ServiceAccounts;
let ServiceAccountPage: typeof import("./ServiceAccounts.tsx").ServiceAccountPage;
let t: typeof import("../peopleTesting.tsx");
withDom(async () => {
  ({ ServiceAccounts, ServiceAccountPage } = await import("./ServiceAccounts.tsx"));
  t = await import("../peopleTesting.tsx");
});

test("service accounts: admins create, members read; names, roles and owners lead", async () => {
  const p = await t.mountAs("admin", "/service-accounts", <ServiceAccounts />);
  try {
    await p.until(() => p.text().includes("gha-runners"), "accounts");
    expect(p.button("New service account")).toBeDefined();
    expect(p.text()).toContain("Eva Moreira");
    await t.click(p.button("New service account"));
    const d = p.dialog()!;
    await t.fill(d.querySelector<HTMLInputElement>("input"), "release-bot");
    await t.click(t.dialogButton(d, "Create and make a key"));
    await p.until(() => p.dialog()?.textContent?.includes("Key created") === true, "first key reveal");
    expect(p.fake.posts.find((x) => x.path === "/v1/service-accounts")!.body).toMatchObject({ name: "release-bot", role: "member" });
    await t.click(t.dialogButton(p.dialog(), "Done"));
    expect(p.text()).toContain("release-bot");
  } finally { await p.done(); }
});

test("service account detail: cap 2, rotation copy; a non-admin owner rotates keys, not role", async () => {
  const p = await t.mountAs("owner", "/service-accounts/sa_4kq2v8m1", <ServiceAccountPage id="sa_4kq2v8m1" />, { extra: (path) => path.startsWith("/v1/runs") ? { runs: [] } : undefined });
  try {
    await p.until(() => p.button("New key") != null && p.text().includes("gha-2026-05"), "keys");
    expect(p.button("New key")!.disabled).toBe(true);
    expect(p.text()).toContain("create the new one, switch the client, revoke the old");
    await t.click(p.button("Revoke gha-2026-05"));
    await t.click(t.dialogButton(p.dialog(), "Revoke key"));
    await p.until(() => p.button("New key")?.disabled === false, "one slot free");
  } finally { await p.done(); }
  const m = await t.mountAs("member", "/service-accounts/sa_7cr3b2x9", <ServiceAccountPage id="sa_7cr3b2x9" />, { extra: (path) => path.startsWith("/v1/runs") ? { runs: [] } : undefined });
  try {
    await m.until(() => m.button("New key") != null, "owner's key action");
    expect(m.button("Edit")).toBeUndefined();
    expect(m.button("Disable")).toBeUndefined();
    expect(m.button("New key")!.disabled).toBe(false);
  } finally { await m.done(); }
});

// luxd answers a service account's keys to its owner and admins only (others 404): the page neither asks nor shows them.
test("service account detail as a member who does not own it: no Keys card, no keys request", async () => {
  const p = await t.mountAs("bruno", "/service-accounts/sa_7cr3b2x9", <ServiceAccountPage id="sa_7cr3b2x9" />, { extra: (path) => (path.startsWith("/v1/runs") ? { runs: [] } : undefined) });
  try {
    await p.until(() => p.text().includes("ci-review-bot"), "the account");
    expect(p.text()).toContain("Its keys are visible to its owner and to admins.");
    expect(p.button("New key")).toBeUndefined();
    expect(p.fake.calls.some((c) => c.includes("/keys"))).toBe(false);
    expect(p.text()).not.toContain("Not found");
  } finally { await p.done(); }
});

// The server takes the owner by email (ownerEmail), and "" clears it.
const pickOption = async (d: HTMLDialogElement, trigger: number, label: string) => {
  const triggers = [...d.querySelectorAll<HTMLButtonElement>(".select-trigger")];
  await t.click(triggers[trigger]);
  const opt = [...d.querySelectorAll<HTMLElement>('[role="option"]')].find((o) => t.visibleText(o).startsWith(label));
  await t.click(opt);
};

test("service account owner: created with ownerEmail, then cleared with ownerEmail \"\"", async () => {
  const p = await t.mountAs("admin", "/service-accounts", <ServiceAccounts />);
  try {
    await p.until(() => p.text().includes("gha-runners"), "accounts");
    await t.click(p.button("New service account"));
    const d = p.dialog()!;
    await t.fill(d.querySelector<HTMLInputElement>("input"), "release-bot");
    await pickOption(d, 0, "Carla Santos");
    await t.click(t.dialogButton(d, "Create and make a key"));
    await p.until(() => p.dialog()?.textContent?.includes("Key created") === true, "first key reveal");
    const create = p.fake.posts.find((x) => x.path.startsWith("/v1/service-accounts") && x.method === "POST")!.body as Record<string, unknown>;
    expect(create).toMatchObject({ name: "release-bot", ownerEmail: "carla@acme.dev" });
    expect(create).not.toHaveProperty("ownerId");
    await t.click(t.dialogButton(p.dialog(), "Done"));
    await p.until(() => p.button("Edit release-bot") != null, "row");
    await t.click(p.button("Edit release-bot"));
    const e = p.dialog()!;
    await pickOption(e, 0, "No owner");
    await t.click(t.dialogButton(e, "Save"));
    await p.until(() => p.fake.posts.some((x) => x.method === "PATCH"), "patch");
    const patch = p.fake.posts.find((x) => x.method === "PATCH")!.body as Record<string, unknown>;
    expect(patch.ownerEmail).toBe("");
    expect(patch).not.toHaveProperty("ownerId");
    await p.until(() => !p.dialog(), "saved");
    const row = [...p.el.querySelectorAll("tr")].find((r) => r.textContent?.includes("release-bot"))!;
    expect(t.visibleText(row)).toContain("no owner");
  } finally { await p.done(); }
});
