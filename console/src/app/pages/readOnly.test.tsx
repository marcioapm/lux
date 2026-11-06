import { expect, test } from "bun:test";
import { withDom } from "../testDom.ts";

// whoami readOnly: a read-only key (an admin's, an operator's) reads as its
// role and gets no mutating control anywhere, its own keys and an account's
// keys included (luxd answers every write 403).
let Profile: typeof import("./Profile.tsx").Profile;
let Members: typeof import("./Members.tsx").Members;
let MemberPage: typeof import("./MemberPage.tsx").MemberPage;
let ServiceAccounts: typeof import("./ServiceAccounts.tsx");
let RunPage: typeof import("./RunPage.tsx").RunPage;
let t: typeof import("../peopleTesting.tsx");
withDom(async () => {
  ({ Profile } = await import("./Profile.tsx"));
  ({ Members } = await import("./Members.tsx"));
  ({ MemberPage } = await import("./MemberPage.tsx"));
  ServiceAccounts = await import("./ServiceAccounts.tsx");
  ({ RunPage } = await import("./RunPage.tsx"));
  t = await import("../peopleTesting.tsx");
});

const ACTION = /^(New key|New service account|Invite people|Change role|Remove|Edit|Disable|Stop|Terminate|Resume|Cancel|Extend|Rename)$/;
const actionButtons = (el: HTMLElement) => [...el.querySelectorAll("button")].filter((b) => !b.closest("dialog") && (ACTION.test(b.textContent?.trim() ?? "") || /^(Revoke|Rename|Change|Remove|Edit|Disable) /.test(b.getAttribute("aria-label") ?? ""))).map((b) => b.textContent?.trim() || b.getAttribute("aria-label"));

for (const as of ["readonly", "opreadonly"]) {
  test(`${as}: Profile shows the keys, with no New key, Rename or Revoke`, async () => {
    const p = await t.mountAs(as, "/me?tenant=ten_acme", <Profile />);
    try {
      await p.until(() => p.text().includes("API keys") && p.el.querySelectorAll("tbody tr td:first-child").length > 0, "the keys");
      expect(actionButtons(p.el)).toEqual([]);
      expect(p.text()).toContain("Signed in with a read-only key");
    } finally {
      await p.done();
    }
  });

  test(`${as}: Members, a member's page and service accounts have no mutating control`, async () => {
    const pages: [string, React.ReactNode, string][] = [
      ["/members?tenant=ten_acme", <Members />, "Carla Santos"],
      ["/members/usr_car?tenant=ten_acme", <MemberPage id="usr_car" />, "carla@acme.dev"],
      ["/service-accounts?tenant=ten_acme", <ServiceAccounts.ServiceAccounts />, "ci-review-bot"],
      ["/service-accounts/sa_7cr3b2x9?tenant=ten_acme", <ServiceAccounts.ServiceAccountPage id="sa_7cr3b2x9" />, "ci-review-bot"],
    ];
    for (const [url, node, shown] of pages) {
      const p = await t.mountAs(as, url, node);
      try {
        await p.until(() => p.text().includes(shown), `${url} loaded`);
        await new Promise((r) => setTimeout(r, 30));
        expect({ url, buttons: actionButtons(p.el) }).toEqual({ url, buttons: [] });
      } finally {
        await p.done();
      }
    }
  });

  test(`${as}: a Run page shows no actions and says who can act`, async () => {
    const run = { id: "run_ro", tenant: "acme", name: "ro-run", state: "running", epoch: 1, labels: {}, owner: { kind: "user", id: "usr_bru", name: "Bruno Costa" }, spec: { image: { ref: "alpine" }, workload: { adapter: "generic" }, resources: {} }, createdAt: "2026-11-01T00:00:00Z" };
    const p = await t.mountAs(as, "/runs/run_ro?tenant=ten_acme", <RunPage id="run_ro" />, { extra: (path) => (path.startsWith("/v1/runs/run_ro") ? run : undefined) });
    try {
      await p.until(() => p.text().includes("ro-run"), "run");
      await new Promise((r) => setTimeout(r, 30));
      expect(p.el.querySelector(".page-head-actions")).toBeNull();
      expect(p.text()).toContain("can act on it");
    } finally {
      await p.done();
    }
  });
}
