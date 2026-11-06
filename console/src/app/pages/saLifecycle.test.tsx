import { expect, test } from "bun:test";
import { withDom } from "../testDom.ts";

// Service account lifecycle in the console: rename (Edit › Name), delete, convert.
let ServiceAccounts: typeof import("./ServiceAccounts.tsx").ServiceAccounts;
let ServiceAccountPage: typeof import("./ServiceAccounts.tsx").ServiceAccountPage;
let t: typeof import("../peopleTesting.tsx");
let mock: typeof import("../../../mockPeople.ts");
withDom(async () => {
  ({ ServiceAccounts, ServiceAccountPage } = await import("./ServiceAccounts.tsx"));
  t = await import("../peopleTesting.tsx");
  mock = await import("../../../mockPeople.ts");
});

export const noRuns = (path: string) => (path.startsWith("/v1/runs") ? { runs: [] } : undefined);
const nameInput = (d: HTMLDialogElement) => [...d.querySelectorAll<HTMLInputElement>("input")].find((i) => i.closest(".field")?.querySelector(".field-label")?.textContent === "Name")!;

test("Edit has a Name: a rename says its key of that name follows, and is sent", async () => {
  const p = await t.mountAs("admin", "/service-accounts/sa_5dd0p2x8", <ServiceAccountPage id="sa_5dd0p2x8" />, { extra: noRuns });
  try {
    await p.until(() => p.button("Edit") != null, "the page");
    await t.click(p.button("Edit"));
    const d = p.dialog()!;
    expect(nameInput(d).value).toBe("dude-prod");
    await t.fill(nameInput(d), "jervasion");
    await p.until(() => d.textContent!.includes("Its key named dude-prod is renamed too."), "the key note");
    await t.click(t.dialogButton(d, "Save"));
    await p.until(() => p.fake.posts.some((x) => x.method === "PATCH"), "the PATCH");
    expect(p.fake.posts.find((x) => x.method === "PATCH")).toMatchObject({ path: "/v1/service-accounts/sa_5dd0p2x8", body: { name: "jervasion" } });
    expect(mock.KEYS.find((k) => k.id === "key_dude")!.name).toBe("jervasion");
    await p.until(() => p.text().includes("jervasion"), "the new name");
  } finally {
    await p.done();
  }
});

test("Edit: an unchanged name is not sent; a bad one disables Save; no key note without a key of that name", async () => {
  const p = await t.mountAs("admin", "/service-accounts", <ServiceAccounts />);
  try {
    await p.until(() => p.button("Edit gha-runners") != null, "accounts");
    await t.click(p.button("Edit gha-runners"));
    const d = p.dialog()!;
    await t.fill(nameInput(d), "GHA");
    expect(t.dialogButton(d, "Save")!.disabled).toBe(true);
    await t.fill(nameInput(d), "gha");
    await p.until(() => p.fake.calls.some((c) => c.startsWith("/v1/service-accounts/sa_4kq2v8m1/keys")), "its keys");
    expect(d.textContent).not.toContain("is renamed too");
    await t.fill(nameInput(d), "gha-runners");
    await t.click(t.dialogButton(d, "Save"));
    await p.until(() => p.fake.posts.some((x) => x.method === "PATCH"), "the PATCH");
    expect(p.fake.posts.find((x) => x.method === "PATCH")!.body).not.toHaveProperty("name");
  } finally {
    await p.done();
  }
});
