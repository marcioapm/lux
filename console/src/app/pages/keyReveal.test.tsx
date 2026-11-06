import { expect, test } from "bun:test";
import { withDom } from "../testDom.ts";

// The shown-once secret: after Done, and after Escape, nothing in the
// document (an input's value, a snippet, any text or attribute) still holds
// it. One reveal dialog serves personal and service-account keys: both.
let Profile: typeof import("./Profile.tsx").Profile;
let SA: typeof import("./ServiceAccounts.tsx");
let t: typeof import("../peopleTesting.tsx");
withDom(async () => {
  ({ Profile } = await import("./Profile.tsx"));
  SA = await import("./ServiceAccounts.tsx");
  t = await import("../peopleTesting.tsx");
});

/** Whether secret is anywhere in the document: text, attributes, or an input's live value. */
function holds(secret: string): string[] {
  const where: string[] = [];
  if (document.documentElement.outerHTML.includes(secret)) where.push("markup");
  for (const i of document.querySelectorAll<HTMLInputElement | HTMLTextAreaElement>("input, textarea")) if (i.value.includes(secret)) where.push(`value of ${i.getAttribute("aria-label") ?? i.tagName}`);
  if (document.body.textContent?.includes(secret)) where.push("text");
  return where;
}

/** Closes the open dialog as Escape does in a browser (its cancel event). */
async function escape(d: HTMLDialogElement) {
  const { act } = await import("react");
  await act(async () => void d.dispatchEvent(new Event("cancel", { cancelable: true })));
}

async function reveal(p: Awaited<ReturnType<typeof t.mountAs>>, open: () => Promise<void>) {
  await open();
  const d = p.dialog()!;
  await t.fill(d.querySelector<HTMLInputElement>("input.input"), "laptop-2");
  await t.click(t.dialogButton(d, "Create key"));
  await p.until(() => p.dialog()?.textContent?.includes("Key created") === true, "the reveal");
  const secret = p.dialog()!.querySelector<HTMLInputElement>('input[aria-label="The new API key"]')!.value;
  expect(secret).toMatch(/^lux_[us]_/);
  expect(holds(secret).length).toBeGreaterThan(0);
  return secret;
}

for (const close of ["Done", "Escape"] as const) {
  test(`personal key: after ${close}, no input or snippet holds the secret`, async () => {
    const p = await t.mountAs("member", "/me?tenant=ten_acme", <Profile />);
    try {
      await p.until(() => p.button("New key") != null && !p.button("New key")!.disabled, "New key");
      const secret = await reveal(p, () => t.click(p.button("New key")));
      if (close === "Done") await t.click(t.dialogButton(p.dialog(), "Done"));
      else await escape(p.dialog()!);
      await p.until(() => p.dialog() == null, "the reveal closed");
      expect(holds(secret)).toEqual([]);
      // And a second key does not show the first one's secret.
      const second = await reveal(p, () => t.click(p.button("New key")));
      expect(second).not.toBe(secret);
      expect(holds(secret)).toEqual([]);
    } finally {
      await p.done();
    }
  });

  test(`service account key: after ${close}, no input or snippet holds the secret`, async () => {
    const p = await t.mountAs("admin", "/service-accounts/sa_2ne9v1q5?tenant=ten_acme", <SA.ServiceAccountPage id="sa_2ne9v1q5" />);
    try {
      await p.until(() => p.button("New key") != null && !p.button("New key")!.disabled, "New key");
      const secret = await reveal(p, () => t.click(p.button("New key")));
      if (close === "Done") await t.click(t.dialogButton(p.dialog(), "Done"));
      else await escape(p.dialog()!);
      await p.until(() => p.dialog() == null, "the reveal closed");
      expect(holds(secret)).toEqual([]);
    } finally {
      await p.done();
    }
  });
}
