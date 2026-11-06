import { expect, test } from "bun:test";
import { withDom } from "../testDom.ts";

// Your own row on Members and your own member page (members.go checkOwnerChange):
// operators always change their role and may leave; an owner changes their
// own role while another owner exists; an admin gets nothing on themselves.
let Members: typeof import("./Members.tsx").Members;
let MemberPage: typeof import("./MemberPage.tsx").MemberPage;
let memberActions: typeof import("./Members.tsx").memberActions;
let t: typeof import("../peopleTesting.tsx");
let mock: typeof import("../../../mockPeople.ts");
withDom(async () => {
  ({ Members, memberActions } = await import("./Members.tsx"));
  ({ MemberPage } = await import("./MemberPage.tsx"));
  t = await import("../peopleTesting.tsx");
  mock = await import("../../../mockPeople.ts");
});

const rows = (el: HTMLElement) => [...el.querySelectorAll("tbody tr")].filter((tr) => !tr.querySelector(".td-empty"));
const rowOf = (el: HTMLElement, name: string) => rows(el).find((tr) => tr.querySelector(".person-name")?.textContent?.startsWith(name));
const actions = (el: HTMLElement, name: string) => [...(rowOf(el, name)?.querySelectorAll(".row-actions button") ?? [])].map((b) => ({ label: b.getAttribute("aria-label"), disabled: (b as HTMLButtonElement).disabled, title: b.getAttribute("title") }));
const noRuns = (path: string) => (path.startsWith("/v1/runs") ? { runs: [] } : undefined);
/** Eva the only owner of acme: Márcio (operator) made admin before the first answer. */
const evaAlone = (path: string) => {
  const mar = mock.MEMBERSHIPS.find((m) => m.tenantId === "ten_acme" && m.userId === "usr_mar");
  if (mar) mar.role = "admin";
  return noRuns(path);
};

test("an operator's own row: Change your role and Leave, with a confirm that says operators keep access", async () => {
  const p = await t.mountAs("operator", "/members?tenant=ten_acme", <Members />);
  try {
    await p.until(() => rows(p.el).length === 6, "6 members");
    expect(t.visibleText(rowOf(p.el, "Márcio Martins")!)).toContain("(you)");
    expect(actions(p.el, "Márcio Martins")).toEqual([
      { label: "Change your role", disabled: false, title: null },
      { label: "Leave acme", disabled: false, title: null },
    ]);
    await t.click(p.button("Leave acme"));
    expect(p.dialog()!.textContent).toContain("Leave acme?");
    expect(p.dialog()!.textContent).toContain("you keep it only because you are an operator");
    await t.click(t.dialogButton(p.dialog(), "Cancel"));
    await t.click(p.button("Change your role"));
    expect(p.dialog()!.textContent).toContain("Change your role");
    await t.click(p.dialog()!.querySelector(".select-trigger"));
    await t.click([...p.dialog()!.querySelectorAll('[role="option"]')].find((o) => o.textContent?.startsWith("Admin")));
    expect(p.dialog()!.textContent).toContain("You give up Owner");
    await t.click(t.dialogButton(p.dialog(), "Change role"));
    await p.until(() => p.fake.posts.some((x) => x.method === "PATCH"), "the PATCH");
    expect(p.fake.posts.find((x) => x.method === "PATCH")).toMatchObject({ path: "/v1/members/usr_mar?tenant=ten_acme", body: { role: "admin" } });
  } finally {
    await p.done();
  }
});

test("an owner with another owner changes their own role; never Remove on themselves", async () => {
  const p = await t.mountAs("owner", "/members", <Members />);
  try {
    await p.until(() => rows(p.el).length === 6, "6 members");
    expect(actions(p.el, "Eva Moreira")).toEqual([{ label: "Change your role", disabled: false, title: null }]);
    await t.click(p.button("Change your role"));
    await t.click(p.dialog()!.querySelector(".select-trigger"));
    await t.click([...p.dialog()!.querySelectorAll('[role="option"]')].find((o) => o.textContent?.startsWith("Admin")));
    await t.click(t.dialogButton(p.dialog(), "Change role"));
    await p.until(() => p.fake.posts.some((x) => x.method === "PATCH"), "the PATCH");
    expect(p.fake.posts.find((x) => x.method === "PATCH")).toMatchObject({ path: "/v1/members/usr_eva", body: { role: "admin" } });
    // whoami is asked again: the role in the session follows.
    await p.until(() => p.fake.calls.some((c) => c.startsWith("/v1/whoami")), "whoami again");
  } finally {
    await p.done();
  }
});

test("the last owner: Change your role is disabled with the reason, on the table and the member page", async () => {
  const p = await t.mountAs("owner", "/members", <Members />, { extra: evaAlone });
  try {
    await p.until(() => rows(p.el).length === 6 && t.visibleText(rowOf(p.el, "Márcio Martins")!).includes("Admin"), "Márcio admin");
    expect(actions(p.el, "Eva Moreira")).toEqual([{ label: "Change your role", disabled: true, title: "You are the last owner: make someone else an owner first" }]);
  } finally {
    await p.done();
  }
  const page = await t.mountAs("owner", "/members/usr_eva", <MemberPage id="usr_eva" />, { extra: evaAlone });
  try {
    await page.until(() => page.button("Change role") != null && page.fake.calls.some((c) => c === "/v1/members"), "the page");
    await page.until(() => page.button("Change role")!.title !== "", "the reason");
    expect(page.button("Change role")!.disabled).toBe(true);
    expect(page.button("Change role")!.title).toContain("last owner");
    expect(page.button("Leave tenant")).toBeUndefined();
    expect(page.button("Remove")).toBeUndefined();
  } finally {
    await page.done();
  }
});

test("an admin has no actions on their own row or page; viewers and read-only keys none anywhere", async () => {
  const p = await t.mountAs("admin", "/members", <Members />);
  try {
    await p.until(() => rows(p.el).length === 6, "6 members");
    expect(t.visibleText(rowOf(p.el, "Ana Ribeiro")!)).toContain("(you)");
    expect(actions(p.el, "Ana Ribeiro")).toEqual([]);
  } finally {
    await p.done();
  }
  const page = await t.mountAs("admin", "/members/usr_ana", <MemberPage id="usr_ana" />, { extra: noRuns });
  try {
    await page.until(() => page.text().includes("API keys"), "the page");
    expect(page.button("Change role")).toBeUndefined();
    expect(page.button("Leave tenant")).toBeUndefined();
  } finally {
    await page.done();
  }
  for (const role of ["viewer", "readonly", "opreadonly"]) {
    const q = await t.mountAs(role, "/members?tenant=ten_acme", <Members />);
    try {
      await q.until(() => rows(q.el).length === 6, `6 members (${role})`);
      expect(q.el.querySelectorAll(".row-actions button").length).toBe(0);
    } finally {
      await q.done();
    }
  }
});

test("memberActions: the rule per caller", async () => {
  const { viewerOf } = await import("../identity.ts");
  const base = { tenant: "acme", tenantId: "ten_acme", scopes: ["read", "run", "admin"], consoleAuth: "cloudflare-access", memberships: [] };
  const owner = viewerOf({ ...base, operator: false, userId: "usr_eva", role: "owner" });
  const admin = viewerOf({ ...base, operator: false, userId: "usr_ana", role: "admin" });
  const op = viewerOf({ ...base, operator: true, userId: "usr_mar" });
  const me = (id: string, role: "owner" | "admin") => ({ id, role });
  expect(memberActions(owner, me("usr_eva", "owner"), 2)).toMatchObject({ change: true, changeReason: null, remove: false, self: true });
  expect(memberActions(owner, me("usr_eva", "owner"), 1).changeReason).toContain("last owner");
  expect(memberActions(admin, me("usr_ana", "admin"), 1)).toMatchObject({ change: false, remove: false });
  expect(memberActions(op, me("usr_mar", "owner"), 1)).toMatchObject({ change: true, changeReason: null, remove: true, removeReason: null });
  expect(memberActions(admin, me("usr_eva", "owner"), 2)).toMatchObject({ change: false, remove: false });
  expect(memberActions(owner, me("usr_ana", "admin"), 1)).toMatchObject({ change: true, remove: true, removeReason: null });
});
