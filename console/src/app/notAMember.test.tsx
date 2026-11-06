import { expect, test } from "bun:test";
import { withDom } from "./testDom.ts";
import { act } from "react";
import { createRoot } from "react-dom/client";

// whoami answers 403 not_a_member with no identity body (luxd: a verified
// person in no tenant). The console shows "not part of any tenant", with
// the pending invitations, not the generic "Cannot reach luxd" error.
let App: typeof import("./App.tsx").App;
let api: typeof import("../api/index.ts");
let testing: typeof import("./testing.ts");
withDom(async () => {
  ({ App } = await import("./App.tsx"));
  api = await import("../api/index.ts");
  testing = await import("./testing.ts");
});

const notAMember = () => new Response(JSON.stringify({ error: { code: "not_a_member", message: "you are not a member of this tenant: ask one of its admins to invite this email" } }), { status: 403, headers: { "Content-Type": "application/json" } });
const invitations = { invitations: [{ code: "inv_1", tenant: "acme", role: "member", invitedBy: { name: "Ana Ribeiro" }, createdAt: new Date().toISOString(), expiresAt: new Date(Date.now() + 86400e3).toISOString() }] };

async function mount(fake: ReturnType<typeof testing.fakeApi>) {
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL("http://localhost/");
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () => root.render(<App />));
  return {
    el,
    done: async () => {
      await act(async () => root.unmount());
      el.remove();
      fake.restore();
      api.signOut();
    },
  };
}

test("a key whose whoami is 403 not_a_member: the no-tenant screen", async () => {
  api.signIn("k");
  const fake = testing.fakeApi((path) => (path.startsWith("/v1/whoami") ? notAMember() : path.startsWith("/v1/me/invitations") ? invitations : {}));
  const p = await mount(fake);
  try {
    await testing.until(() => p.el.textContent?.includes("You're not part of any tenant yet") === true, "the no-tenant screen", 3000);
    expect(p.el.textContent).not.toContain("Cannot reach luxd");
    expect(p.el.textContent).not.toContain("Could not check this key's access");
  } finally {
    await p.done();
  }
});

test("signed in through Access, whoami 403 not_a_member: who from /v1/me, the no-tenant screen with the pending invitation", async () => {
  api.signOut();
  const fake = testing.fakeApi((path) => {
    if (path.startsWith("/v1/whoami")) return notAMember();
    if (path.startsWith("/v1/me/invitations")) return invitations;
    if (path.startsWith("/v1/me")) return { id: "usr_sam", email: "sam@globex.io", name: "Sam Lee", picture: "", operator: false, memberships: [] };
    return {};
  });
  const p = await mount(fake);
  try {
    await testing.until(() => p.el.textContent?.includes("You're not part of any tenant yet") === true, "the no-tenant screen", 3000);
    expect(p.el.textContent).toContain("sam@globex.io");
    expect(p.el.textContent).not.toContain("Sign in");
  } finally {
    await p.done();
  }
});
