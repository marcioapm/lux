import { expect, test } from "bun:test";
import { withDom } from "../testDom.ts";

// A key list that failed to load is an error with Retry, never "no keys"
// (nor 0 active keys); New key waits for the list. A failed refresh keeps
// the list under an error strip (as Profile does).
let SA: typeof import("./ServiceAccounts.tsx");
let MemberPage: typeof import("./MemberPage.tsx").MemberPage;
let t: typeof import("../peopleTesting.tsx");
withDom(async () => {
  SA = await import("./ServiceAccounts.tsx");
  ({ MemberPage } = await import("./MemberPage.tsx"));
  t = await import("../peopleTesting.tsx");
});

const down = () => new Response(JSON.stringify({ error: { code: "internal", message: "database unavailable" } }), { status: 500, headers: { "Content-Type": "application/json" } });
const keysCard = (el: HTMLElement, title: string) => [...el.querySelectorAll("section.card")].find((c) => c.querySelector(".card-title")?.textContent === title) as HTMLElement | undefined;

test("service account keys: a failed load is an error with Retry, no 'No keys', no 0 / 2, New key off; Retry loads them", async () => {
  let fail = true;
  const p = await t.mountAs("admin", "/service-accounts/sa_7cr3b2x9?tenant=ten_acme", <SA.ServiceAccountPage id="sa_7cr3b2x9" />, {
    extra: (path) => (fail && /^\/v1\/service-accounts\/sa_7cr3b2x9\/keys/.test(path) ? down() : undefined),
  });
  try {
    await p.until(() => keysCard(p.el, "Keys")?.textContent?.includes("database unavailable") === true, "the error in the Keys card");
    const card = keysCard(p.el, "Keys")!;
    expect(card.textContent).not.toContain("No keys");
    expect(p.text()).not.toContain("0 / 2");
    const newKey = [...card.querySelectorAll("button")].find((b) => b.textContent?.trim() === "New key")!;
    expect(newKey.disabled).toBe(true);
    fail = false;
    await t.click([...card.querySelectorAll("button")].find((b) => /Retry|Try again/.test(b.textContent ?? "")));
    await p.until(() => keysCard(p.el, "Keys")?.textContent?.includes("ci-2026-07") === true, "the keys after Retry");
    expect([...keysCard(p.el, "Keys")!.querySelectorAll("button")].find((b) => b.textContent?.trim() === "New key")!.disabled).toBe(false);
  } finally {
    await p.done();
  }
});

test("service account keys: a failed refresh keeps the list with an error strip", async () => {
  let fail = false;
  const p = await t.mountAs("admin", "/service-accounts/sa_7cr3b2x9?tenant=ten_acme", <SA.ServiceAccountPage id="sa_7cr3b2x9" />, {
    extra: (path, method) => (fail && method === "GET" && /^\/v1\/service-accounts\/sa_7cr3b2x9\/keys/.test(path) ? down() : undefined),
  });
  try {
    await p.until(() => keysCard(p.el, "Keys")?.textContent?.includes("ci-2026-07") === true, "the keys");
    fail = true;
    // Revoke refetches the list: that refresh fails.
    await t.click(p.button("Revoke ci-2026-07"));
    const { dialogButton } = t;
    await t.click(dialogButton(p.dialog(), "Revoke key"));
    await p.until(() => keysCard(p.el, "Keys")?.querySelector(".error-strip") != null, "the strip");
    expect(keysCard(p.el, "Keys")!.textContent).toContain("ci-2026-07");
  } finally {
    await p.done();
  }
});

test("a member's keys: a failed load is an error with Retry, never 'has no API keys'", async () => {
  const p = await t.mountAs("admin", "/members/usr_car?tenant=ten_acme", <MemberPage id="usr_car" />, {
    extra: (path) => (/^\/v1\/members\/usr_car\/keys/.test(path) ? down() : undefined),
  });
  try {
    await p.until(() => keysCard(p.el, "API keys")?.textContent?.includes("database unavailable") === true, "the error in API keys");
    const card = keysCard(p.el, "API keys")!;
    expect(card.textContent).not.toContain("has no API keys");
    expect([...card.querySelectorAll("button")].some((b) => /Retry|Try again/.test(b.textContent ?? ""))).toBe(true);
  } finally {
    await p.done();
  }
});
