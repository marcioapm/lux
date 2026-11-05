import { afterAll, beforeAll, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";

// The page's imports (the router) touch window at load: register the DOM first.
let Profile: typeof import("./Profile.tsx").Profile;
let t: typeof import("../peopleTesting.tsx");
beforeAll(async () => {
  GlobalRegistrator.register();
  ({ Profile } = await import("./Profile.tsx"));
  t = await import("../peopleTesting.tsx");
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(async () => {
  await GlobalRegistrator.unregister();
});

const keyNames = (el: HTMLElement) => [...el.querySelectorAll("section.card")].find((c) => c.querySelector(".card-title")?.textContent === "API keys")!.querySelectorAll("tbody tr td:first-child");

test("Profile: name, role, my keys; New key reveals the secret once with the export snippet", async () => {
  const p = await t.mountAs("member", "/me", <Profile />);
  try {
    await p.until(() => p.text().includes("Carla Santos") && keyNames(p.el).length === 2, "Carla's profile with 2 keys");
    expect(p.text()).toContain("carla@acme.dev");
    expect(p.el.querySelector(".page-head .badge")!.textContent).toBe("Member");
    expect(p.text()).toContain("Keys act as you. 2 of 5 active.");
    await t.click(p.button("New key"));
    const d = p.dialog()!;
    expect(d.textContent).toContain("The key acts as you (Carla Santos, Member in acme)");
    await t.fill(d.querySelector<HTMLInputElement>("input.input"), "ci-laptop");
    await t.click(t.dialogButton(d, "Create key"));
    await p.until(() => p.dialog()?.textContent?.includes("Key created") === true, `the reveal (posts: ${JSON.stringify(p.fake.posts)})`);
    const secret = p.dialog()!.querySelector<HTMLInputElement>('input[aria-label="The new API key"]')!.value;
    expect(secret).toMatch(/^lux_u_/);
    expect(p.dialog()!.querySelector("pre")!.textContent).toBe(`export LUX_API_KEY=${secret}\nlux whoami`);
    await t.click(t.dialogButton(p.dialog(), "Done"));
    await p.until(() => keyNames(p.el).length === 3, "the new key listed");
    expect(p.dialog()).toBeUndefined();
    expect(p.fake.posts.find((x) => x.path.startsWith("/v1/me/keys"))!.body).toMatchObject({ name: "ci-laptop" });
  } finally {
    await p.done();
  }
});

test("Profile: at 5 keys New key is off and says why; revoke asks first", async () => {
  const p = await t.mountAs("admin", "/me", <Profile />);
  try {
    await p.until(() => keyNames(p.el).length === 3, "Ana's 3 keys");
    // Two more reach the cap.
    for (const name of ["a", "b"]) {
      await t.click(p.button("New key"));
      await t.fill(p.dialog()!.querySelector<HTMLInputElement>("input.input"), name);
      await t.click(t.dialogButton(p.dialog(), "Create key"));
      await p.until(() => p.dialog()?.textContent?.includes("Key created") === true, "the reveal");
      await t.click(t.dialogButton(p.dialog(), "Done"));
    }
    await p.until(() => keyNames(p.el).length === 5, "5 keys");
    expect(p.button("New key")!.disabled).toBe(true);
    expect(p.button("New key")!.title).toBe("5 active keys is the limit: revoke one first");
    expect(p.text()).toContain("5 active keys is the limit: revoke one first.");
    await t.click(p.button("Revoke laptop"));
    expect(p.dialog()!.textContent).toContain("Revoke laptop?");
    await t.click(t.dialogButton(p.dialog(), "Revoke key"));
    await p.until(() => keyNames(p.el).length === 4, "one revoked");
    expect(p.button("New key")!.disabled).toBe(false);
  } finally {
    await p.done();
  }
});

test("Profile: an operator sees the Operator badge, both tenants and the operator note", async () => {
  const p = await t.mountAs("operator", "/me", <Profile />);
  try {
    await p.until(() => p.text().includes("Márcio Martins"), "the profile");
    expect([...p.el.querySelectorAll(".page-head .badge")].map((b) => b.textContent)).toContain("Operator");
    await p.until(() => p.text().includes("globex"), "the tenants card");
    expect(p.text()).toContain("You are an operator of this lux installation");
  } finally {
    await p.done();
  }
});
