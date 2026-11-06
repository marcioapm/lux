import { expect, test } from "bun:test";
import { withDom } from "../testDom.ts";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Invitation, InviteResult } from "../../api/index.ts";

let InviteLinksDialog: typeof import("./InviteDialog.tsx").InviteLinksDialog;
let ds: typeof import("@lux/design-system");
let visibleText: typeof import("../peopleTesting.tsx").visibleText;
withDom(async () => {
  ({ InviteLinksDialog } = await import("./InviteDialog.tsx"));
  ds = await import("@lux/design-system");
  ({ visibleText } = await import("../peopleTesting.tsx"));
});

const inv = (email: string, role: Invitation["role"], code: string): Invitation => ({ id: `inv_${code}`, email, role, status: "pending", code, url: `https://lux.example.com/invite/${code}`, createdAt: "2026-11-01T00:00:00Z", expiresAt: "2026-11-15T00:00:00Z" });

async function render(node: React.ReactNode) {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () => root.render(<ds.ToastProvider>{node}</ds.ToastProvider>));
  return {
    el,
    done: async () => {
      await act(async () => root.unmount());
      el.remove();
    },
  };
}

// A renewed invitation keeps its own role: requested Member, renewed Admin.
test("the links dialog shows each link's own role, not the role just picked", async () => {
  const results: InviteResult[] = [
    { email: "new@acme.dev", result: "created", invitation: inv("new@acme.dev", "member", "c1") },
    { email: "boss@acme.dev", result: "renewed", invitation: inv("boss@acme.dev", "admin", "c2") },
    { email: "mia@acme.dev", result: "member", message: "already a member of this tenant" },
  ];
  const p = await render(<InviteLinksDialog results={results} role="member" tenantName="acme" onDone={() => {}} />);
  try {
    const rows = [...p.el.querySelectorAll('ul[aria-label="Invitation links"] li')].map((li) => visibleText(li));
    expect(rows).toHaveLength(2);
    expect(rows[0]).toContain("new@acme.dev");
    expect(rows[0]).toContain("Member");
    expect(rows[1]).toContain("boss@acme.dev");
    expect(rows[1]).toContain("Admin");
    expect(rows[1]).not.toContain("Member");
    expect(rows[1]).toContain("renewed");
    const text = visibleText(p.el);
    expect(text).not.toContain("as Member");
  } finally {
    await p.done();
  }
});
