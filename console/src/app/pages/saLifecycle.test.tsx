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

test("Delete: enabled with no Runs (confirm names it, then back to the list); disabled with the reason when it has Runs", async () => {
  const p = await t.mountAs("admin", "/service-accounts/sa_9gr1d7k3", <ServiceAccountPage id="sa_9gr1d7k3" />, { extra: noRuns });
  try {
    await p.until(() => p.button("Delete") != null, "the page");
    expect(p.button("Delete")!.disabled).toBe(false);
    await t.click(p.button("Delete"));
    const d = p.dialog()!;
    expect(d.textContent).toContain("Delete grafana-reader?");
    expect(d.textContent).toContain("Its key stops working at once");
    expect(d.querySelector(".btn-danger")?.textContent).toContain("Delete service account");
    await t.click(t.dialogButton(d, "Delete service account"));
    await p.until(() => p.fake.posts.some((x) => x.method === "DELETE"), "the DELETE");
    expect(p.fake.posts.find((x) => x.method === "DELETE")!.path).toBe("/v1/service-accounts/sa_9gr1d7k3");
    await p.until(() => window.location.pathname === "/service-accounts", "back to the list");
    expect(mock.SAS.some((s) => s.id === "sa_9gr1d7k3")).toBe(false);
    expect(mock.KEYS.find((k) => k.id === "key_s_gr1")!.revoked).toBe(true);
  } finally {
    await p.done();
  }
  const r = await t.mountAs("admin", "/service-accounts/sa_3jo1n4s2", <ServiceAccountPage id="sa_3jo1n4s2" />, { extra: noRuns });
  try {
    await r.until(() => r.button("Delete") != null, "the page");
    expect(r.button("Delete")!.disabled).toBe(true);
    expect(r.button("Delete")!.title).toContain("It owns 145 Runs: disable it instead");
    expect(r.text()).toContain("Delete is off: It owns 145 Runs");
  } finally {
    await r.done();
  }
});

test("Delete on the list: per row, disabled for accounts with Runs; a 409 has_runs from luxd is shown", async () => {
  const p = await t.mountAs("owner", "/service-accounts", <ServiceAccounts />);
  try {
    await p.until(() => p.button("Delete grafana-reader") != null, "accounts");
    expect(p.button("Delete grafana-reader")!.disabled).toBe(false);
    expect(p.button("Delete gha-runners")!.disabled).toBe(true);
    expect(p.button("Delete gha-runners")!.title).toContain("4,210 Runs");
    // A Run started since the list loaded: luxd refuses, the toast says why.
    mock.SAS.find((s) => s.id === "sa_9gr1d7k3")!.runs = 1;
    await t.click(p.button("Delete grafana-reader"));
    await t.click(t.dialogButton(p.dialog(), "Delete service account"));
    await p.until(() => document.body.textContent!.includes("owns 1 Runs"), "the refusal");
  } finally {
    await p.done();
  }
});

test("no lifecycle controls for members, viewers and read-only keys", async () => {
  for (const role of ["member", "viewer", "readonly"]) {
    const p = await t.mountAs(role, "/service-accounts/sa_3jo1n4s2", <ServiceAccountPage id="sa_3jo1n4s2" />, { extra: noRuns });
    try {
      await p.until(() => p.text().includes("jonas"), `the page (${role})`);
      for (const b of ["Edit", "Delete", "Disable", "Convert to personal keys"]) expect(p.button(b)).toBeUndefined();
    } finally {
      await p.done();
    }
    const l = await t.mountAs(role, "/service-accounts", <ServiceAccounts />);
    try {
      await l.until(() => l.text().includes("gha-runners"), `the list (${role})`);
      expect(l.el.querySelectorAll(".row-actions button").length).toBe(0);
    } finally {
      await l.done();
    }
  }
});

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
