import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";

// The page's imports (the router) touch window at load: register the DOM first.
let Members: typeof import("./Members.tsx").Members;
let MemberPage: typeof import("./MemberPage.tsx").MemberPage;
let t: typeof import("../peopleTesting.tsx");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ Members } = await import("./Members.tsx"));
  ({ MemberPage } = await import("./MemberPage.tsx"));
  t = await import("../peopleTesting.tsx");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const headers = (el: HTMLElement) => [...el.querySelectorAll("thead th")].map((th) => t.visibleText(th));
const rows = (el: HTMLElement) => [...el.querySelectorAll("tbody tr")].filter((tr) => !tr.querySelector(".td-empty"));
const rowOf = (el: HTMLElement, name: string) => rows(el).find((tr) => tr.querySelector(".person-name")?.textContent?.startsWith(name));

test("Members as an admin: columns, tabs, no actions on Owners or on themselves", async () => {
  const p = await t.mountAs("admin", "/members", <Members />);
  try {
    await p.until(() => rows(p.el).length === 6, "6 members");
    expect(headers(p.el)).toEqual(["Member", "Role", "Running", "Runs · 30d", "Cost · 30d", "Keys", "Last seen", ""]);
    expect([...p.el.querySelectorAll('[role="tab"]')].map((x) => t.visibleText(x))).toEqual(["Members 6", "Invitations 2"]);
    expect(p.button("Invite people")).toBeDefined();
    const actions = (name: string) => [...rowOf(p.el, name)!.querySelectorAll(".row-actions button")].map((b) => b.getAttribute("aria-label"));
    expect(actions("Eva Moreira")).toEqual([]);
    expect(actions("Ana Ribeiro")).toEqual([]);
    expect(actions("Carla Santos")).toEqual(["Change Carla Santos's role", "Remove Carla Santos from acme"]);
    await p.until(() => t.visibleText(rowOf(p.el, "Carla Santos")!).includes("$212.40"), "Carla's cost");
  } finally {
    await p.done();
  }
});

test("Members as a member: no tabs, no Invite, others' keys hidden, the ask-an-admin note", async () => {
  const p = await t.mountAs("member", "/members", <Members />);
  try {
    await p.until(() => rows(p.el).length === 6, "6 members");
    expect(p.el.querySelector('[role="tablist"]')).toBeNull();
    expect(p.button("Invite people")).toBeUndefined();
    expect(headers(p.el)).not.toContain("");
    const keys = headers(p.el).indexOf("Keys");
    const cell = (name: string) => t.visibleText(rowOf(p.el, name)!.querySelectorAll("td")[keys]!);
    expect(cell("Carla Santos")).toBe("2");
    expect(cell("Ana Ribeiro")).toBe("·");
    expect(p.text()).toContain("Only admins and owners add or change members. Ask");
  } finally {
    await p.done();
  }
});

test("Members in key mode: no Invitations tab and no Invite people, even for admins", async () => {
  const p = await t.mountAs("admin", "/members", <Members />, { auth: "key" });
  try {
    await p.until(() => rows(p.el).length === 6, "6 members");
    expect(p.el.querySelector('[role="tablist"]')).toBeNull();
    expect(p.button("Invite people")).toBeUndefined();
    expect(p.fake.calls.some((c) => c.startsWith("/v1/invitations"))).toBe(false);
  } finally {
    await p.done();
  }
});

test("Change role: an admin cannot pick Owner, and the change is sent", async () => {
  const p = await t.mountAs("admin", "/members", <Members />);
  try {
    await p.until(() => rows(p.el).length === 6, "6 members");
    await t.click(p.button("Change Bruno Costa's role"));
    const d = p.dialog()!;
    expect(d.textContent).toContain("Change Bruno Costa's role");
    await t.click(d.querySelector(".select-trigger, .select button"));
    const owner = [...d.querySelectorAll('[role="option"]')].find((o) => o.textContent?.startsWith("Owner"))!;
    expect(owner.getAttribute("aria-disabled")).toBe("true");
    expect(owner.textContent).toContain("only owners grant Owner");
    const adminOpt = [...d.querySelectorAll('[role="option"]')].find((o) => o.textContent?.startsWith("Admin"))!;
    await t.click(adminOpt);
    await t.click(t.dialogButton(d, "Change role"));
    await p.until(() => p.fake.posts.some((x) => x.method === "PATCH"), "the PATCH");
    expect(p.fake.posts.find((x) => x.method === "PATCH")).toMatchObject({ path: "/v1/members/usr_bru", body: { role: "admin" } });
  } finally {
    await p.done();
  }
});

test("Invite people: counts as you type, links after, Done lands on Invitations", async () => {
  const p = await t.mountAs("admin", "/members", <Members />);
  try {
    await p.until(() => rows(p.el).length === 6 && p.el.querySelector('[role="tab"]')?.textContent === "Members6", "members and tabs");
    await t.click(p.button("Invite people"));
    const d = p.dialog()!;
    const input = d.querySelector<HTMLInputElement>("#invite-emails")!;
    await act(async () => {
      const ev = new Event("paste", { bubbles: true, cancelable: true }) as Event & { clipboardData: { getData: () => string } };
      ev.clipboardData = { getData: () => "Jorge Faria <jorge@acme.dev>, marta@acme.dev, ana@acme.dev, filipa@acme.dev, rita@acme.dev, tomás@" };
      input.dispatchEvent(ev);
    });
    expect(d.querySelector(".emailinput-count")!.textContent).toBe("3 to invite (1 renewed) · 3 left out");
    expect([...d.querySelectorAll(".emailinput-left li")].map((li) => t.visibleText(li))).toEqual(["ana@acme.dev already in acme", "filipa@acme.dev already invited Copy their link", "tomás@ not an email"]);
    // Owner is off for an admin.
    expect(d.textContent).toContain("Only owners invite Owners.");
    await t.click(t.dialogButton(d, "Invite 3 people"));
    await p.until(() => p.dialog()?.textContent?.includes("Send these links") === true, "the links dialog");
    const links = p.dialog()!;
    expect([...links.querySelectorAll(".invlinks-email")].map((x) => x.textContent)).toEqual(["jorge@acme.dev", "marta@acme.dev", "rita@acme.dev"]);
    expect(links.textContent).toContain("renewed");
    expect(p.fake.posts.find((x) => x.path.startsWith("/v1/invitations"))!.body).toEqual({ role: "member", invitations: [{ email: "jorge@acme.dev", name: "Jorge Faria" }, { email: "marta@acme.dev" }, { email: "rita@acme.dev" }] });
    await t.click(t.dialogButton(links, "Done"));
    expect(new URL(window.location.href).searchParams.get("tab")).toBe("invitations");
  } finally {
    await p.done();
  }
});

test("Invitations tab: status counts, search, renew and revoke", async () => {
  const p = await t.mountAs("admin", "/members?tab=invitations", <Members />);
  try {
    await p.until(() => rows(p.el).length === 2, `2 pending (${p.fake.calls.join(" ")} ${p.text().slice(0, 300)})`);
    expect([...p.el.querySelectorAll('[role="radio"]')].map((x) => x.textContent)).toEqual(["Pending 2", "Expired 1", "Accepted 1", "Revoked 1", "All"]);
    expect(headers(p.el)).toEqual(["Email", "Role", "Invited by", "Sent", "Expires", "Status", ""]);
    await t.fill(p.el.querySelector<HTMLInputElement>('input[aria-label="Search email"]'), "joao");
    await p.until(() => rows(p.el).length === 1, "the search");
    await t.click(p.button("Revoke joao.p@acme.dev"));
    await t.click(t.dialogButton(p.dialog(), "Revoke"));
    await p.until(() => p.fake.posts.some((x) => x.path.endsWith("/revoke")), "the revoke");
    await p.until(() => rows(p.el).length === 0, "no pending joao");
    await p.until(() => [...p.el.querySelectorAll('[role="radio"]')].map((x) => x.textContent)[3] === "Revoked 2", "counts updated");
  } finally {
    await p.done();
  }
});

test("A member's page: stats, owned service accounts, keys only for admins and themselves", async () => {
  const asAdmin = await t.mountAs("admin", "/members/usr_car", <MemberPage id="usr_car" />, { extra: (path) => (path.startsWith("/v1/runs") ? { runs: [] } : undefined) });
  try {
    await asAdmin.until(() => asAdmin.text().includes("Service accounts owned") && asAdmin.text().includes("API keys"), "the page");
    expect(asAdmin.text()).toContain("ci-review-bot");
    expect(asAdmin.button("Change role")).toBeDefined();
  } finally {
    await asAdmin.done();
  }
  const asOther = await t.mountAs("bruno", "/members/usr_car", <MemberPage id="usr_car" />, { extra: (path) => (path.startsWith("/v1/runs") ? { runs: [] } : undefined) });
  try {
    await asOther.until(() => asOther.text().includes("Service accounts owned"), "the page");
    expect([...asOther.el.querySelectorAll(".card-title")].map((x) => x.textContent)).not.toContain("API keys");
    expect(asOther.button("Change role")).toBeUndefined();
    expect(asOther.fake.calls.some((c) => c.includes("/keys"))).toBe(false);
  } finally {
    await asOther.done();
  }
});

// DELETE /v1/members/{user}/keys/{key} (members.go revokeMemberKey): admins revoke a member's key; an owner's only owners.
test("An admin revokes a member's key on their page; an owner's keys have no Revoke for an admin", async () => {
  const runs = { extra: (path: string) => (path.startsWith("/v1/runs") ? { runs: [] } : undefined) };
  const p = await t.mountAs("admin", "/members/usr_car", <MemberPage id="usr_car" />, runs);
  try {
    await p.until(() => p.button("Revoke laptop") != null, "Carla's keys");
    await t.click(p.button("Revoke laptop"));
    await t.click(t.dialogButton(p.dialog(), "Revoke key"));
    await p.until(() => p.fake.posts.some((x) => x.method === "DELETE"), "the DELETE");
    expect(p.fake.posts.find((x) => x.method === "DELETE")!.path).toBe("/v1/members/usr_car/keys/key_u_car1");
    await p.until(() => p.button("Revoke laptop") == null && p.button("Revoke dude-agent") != null, "the key gone, the other kept");
  } finally {
    await p.done();
  }
  const o = await t.mountAs("admin", "/members/usr_eva", <MemberPage id="usr_eva" />, runs);
  try {
    await o.until(() => o.text().includes("API keys") && o.text().includes("laptop"), "Eva's keys");
    expect(o.button("Revoke laptop")).toBeUndefined();
  } finally {
    await o.done();
  }
});
