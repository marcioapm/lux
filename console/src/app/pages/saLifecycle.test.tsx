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

async function pickMember(p: Awaited<ReturnType<typeof t.mountAs>>, text: string, email: string) {
  const { act } = await import("react");
  const input = p.dialog()!.querySelector<HTMLInputElement>('input[role="combobox"]')!;
  await t.fill(input, text);
  await p.until(() => [...p.dialog()!.querySelectorAll('[role="option"]')].some((o) => o.textContent?.includes(email)), `${email} suggested`);
  const opt = [...p.dialog()!.querySelectorAll('[role="option"]')].find((o) => o.textContent?.includes(email))!;
  await act(async () => opt.dispatchEvent(new MouseEvent("mousedown", { bubbles: true })));
  return opt;
}

test("Convert, same role: the sentence, the counts, then the member's page; the keys and Runs are theirs", async () => {
  const p = await t.mountAs("admin", "/service-accounts/sa_3jo1n4s2", <ServiceAccountPage id="sa_3jo1n4s2" />, { extra: noRuns });
  try {
    await p.until(() => p.button("Convert to personal keys") != null, "the page");
    await p.until(() => p.fake.calls.some((c) => c.startsWith("/v1/members")), "members");
    await t.click(p.button("Convert to personal keys"));
    expect(p.dialog()!.textContent).toContain("Convert jonas to personal keys");
    expect(t.dialogButton(p.dialog(), "Convert")!.disabled).toBe(true);
    await pickMember(p, "bru", "bruno@acme.dev");
    const d = p.dialog()!;
    expect(d.textContent).toContain("jonas's key will act as Bruno Costa (Member) instead of the service account (Member).");
    expect(d.querySelector(".callout")).toBeNull();
    expect(d.textContent).toContain("1 key, 145 Runs move to Bruno; the service account is deleted.");
    await t.click(t.dialogButton(d, "Convert"));
    await p.until(() => p.fake.posts.some((x) => x.path.endsWith("/convert")), "the POST");
    expect(p.fake.posts.find((x) => x.path.endsWith("/convert"))).toMatchObject({ method: "POST", path: "/v1/service-accounts/sa_3jo1n4s2/convert", body: { email: "bruno@acme.dev" } });
    await p.until(() => window.location.pathname === "/members/usr_bru", "Bruno's page");
    expect(mock.KEYS.find((k) => k.id === "key_jonas")).toMatchObject({ userId: "usr_bru", saId: undefined });
    expect(mock.SAS.some((s) => s.id === "sa_3jo1n4s2")).toBe(false);
  } finally {
    await p.done();
  }
});

test("Convert, more access: a Callout says so; less access is said too; an admin cannot pick an owner", async () => {
  const p = await t.mountAs("admin", "/service-accounts/sa_3jo1n4s2", <ServiceAccountPage id="sa_3jo1n4s2" />, { extra: noRuns });
  try {
    await p.until(() => p.button("Convert to personal keys") != null, "the page");
    await p.until(() => p.fake.calls.some((c) => c.startsWith("/v1/members")), "members");
    await t.click(p.button("Convert to personal keys"));
    await pickMember(p, "ana", "ana@acme.dev");
    const d = p.dialog()!;
    expect(d.querySelector(".callout")!.textContent).toBe("jonas's key will act as Ana Ribeiro (Admin) instead of the service account (Member): more access.");
    await pickMember(p, "diogo", "diogo@acme.dev");
    expect(d.textContent).toContain("as Diogo Lopes (Viewer) instead of the service account (Member): less access.");
    expect(d.querySelector(".callout")).toBeNull();
    await t.fill(d.querySelector<HTMLInputElement>('input[role="combobox"]'), "eva");
    await p.until(() => d.querySelectorAll('[role="option"]').length > 0, "Eva suggested");
    const eva = [...d.querySelectorAll('[role="option"]')].find((o) => o.textContent?.includes("eva@acme.dev"))!;
    expect(eva.getAttribute("aria-disabled")).toBe("true");
    expect(eva.textContent).toContain("Owner: above your role");
  } finally {
    await p.done();
  }
});

test("Convert: a member at the key cap is off with the reason", async () => {
  // Ana has 3 keys; gha-runners has 2: 5 fits. Give her a 4th.
  const p = await t.mountAs("owner", "/service-accounts/sa_4kq2v8m1", <ServiceAccountPage id="sa_4kq2v8m1" />, {
    extra: (path) => {
      if (!mock.KEYS.some((k) => k.id === "key_u_ana4")) mock.KEYS.push({ id: "key_u_ana4", tenantId: "ten_acme", userId: "usr_ana", name: "four", prefix: "lux_u_four", readOnly: false, created: 0 });
      return noRuns(path);
    },
  });
  try {
    await p.until(() => p.button("Convert to personal keys") != null, "the page");
    await p.until(() => p.fake.calls.some((c) => c.startsWith("/v1/members")), "members");
    await t.click(p.button("Convert to personal keys"));
    await t.fill(p.dialog()!.querySelector<HTMLInputElement>('input[role="combobox"]'), "ana");
    await p.until(() => p.dialog()!.querySelectorAll('[role="option"]').length > 0, "Ana suggested");
    expect(p.dialog()!.querySelector('[role="option"]')!.textContent).toContain("has 4 keys: 5 at most");
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
