import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { Avatar, initials } from "./Avatar.tsx";
import { inviteLinksText, InviteLinkList } from "./InviteLinkList.tsx";
import { Person, RoleBadge, roleAtLeast } from "./Person.tsx";

test("initials", () => {
  expect(initials("Ada Lovelace")).toBe("AL");
  expect(initials("ada@example.com")).toBe("A");
  expect(initials("Márcio Martins")).toBe("MM");
  expect(initials("")).toBe("?");
});

test("roleAtLeast: the ladder, unknown below everything", () => {
  expect(roleAtLeast("owner", "admin")).toBe(true);
  expect(roleAtLeast("member", "admin")).toBe(false);
  expect(roleAtLeast("viewer", "viewer")).toBe(true);
  expect(roleAtLeast(undefined, "viewer")).toBe(false);
  expect(roleAtLeast("superuser", "viewer")).toBe(false);
});

test("inviteLinksText: one line per address", () => {
  expect(inviteLinksText([{ email: "a@x.io", url: "https://l/invite/1" }, { email: "b@x.io", url: "https://l/invite/2" }])).toBe("a@x.io  https://l/invite/1\nb@x.io  https://l/invite/2");
});

const mounted: { el: HTMLElement; root: Root }[] = [];
async function render(node: React.ReactNode) {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  mounted.push({ el, root });
  await act(async () => root.render(node));
  return el;
}

describe("in a DOM", () => {
  beforeAll(() => {
    GlobalRegistrator.register();
    (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
  });
  afterEach(async () => {
    for (const { el, root } of mounted.splice(0)) {
      await act(async () => root.unmount());
      el.remove();
    }
  });
  afterAll(async () => {
    await GlobalRegistrator.unregister();
  });

  test("Avatar: sizes, initials, labelled, the service-account square", async () => {
    const el = await render(
      <>
        <Avatar name="Ada Lovelace" size="sm" />
        <Avatar name="Ada Lovelace" size="lg" labelled />
        <Avatar name="gha-runners" serviceAccount />
      </>,
    );
    const [sm, lg, sa] = [...el.querySelectorAll(".avatar")];
    expect(sm!.className).toBe("avatar avatar-sm");
    expect(sm!.getAttribute("aria-hidden")).toBe("true");
    expect(sm!.textContent).toBe("AL");
    expect(lg!.className).toBe("avatar avatar-lg");
    expect(lg!.getAttribute("aria-label")).toBe("Ada Lovelace");
    expect(sa!.className).toBe("avatar avatar-sa");
    expect(sa!.querySelector("svg")).not.toBeNull();
    expect(sa!.textContent).toBe("");
  });

  test("Avatar: a picture that fails to load falls back to initials", async () => {
    const el = await render(<Avatar name="Ada Lovelace" picture="https://x.invalid/a.png" />);
    const img = el.querySelector("img")!;
    await act(async () => img.dispatchEvent(new Event("error")));
    expect(el.querySelector("img")).toBeNull();
    expect(el.textContent).toBe("AL");
  });

  test("Person: name, note and sub line; inline is one line with a small avatar", async () => {
    const el = await render(
      <>
        <Person name="Carla Santos" sub="carla@acme.dev" note="(you)" />
        <Person name="ci-review-bot" serviceAccount inline />
      </>,
    );
    expect(el.querySelector(".person .person-name")!.textContent).toBe("Carla Santos (you)");
    expect(el.querySelector(".person .person-sub")!.textContent).toBe("carla@acme.dev");
    const inline = el.querySelector(".person-inline")!;
    expect(inline.querySelector(".avatar")!.className).toBe("avatar avatar-sm avatar-sa");
    expect(inline.textContent).toBe("ci-review-bot");
  });

  test("RoleBadge: tone per role, what it may do in the title", async () => {
    const el = await render(
      <>
        <RoleBadge role="owner" />
        <RoleBadge role="admin" />
        <RoleBadge role="member" />
        <RoleBadge role="viewer" />
        <RoleBadge role="auditor" />
      </>,
    );
    const b = [...el.querySelectorAll(".badge")];
    expect(b.map((x) => x.textContent)).toEqual(["Owner", "Admin", "Member", "Viewer", "auditor"]);
    expect(b.map((x) => x.className.split(/\s+/).join(" "))).toEqual(["badge badge-accent", "badge badge-info", "badge badge-neutral", "badge badge-neutral badge-role-viewer", "badge badge-neutral"]);
    expect(b[3]!.getAttribute("title")).toContain("Cannot start or change anything");
  });

  test("InviteLinkList: each address with its link and a Copy", async () => {
    const el = await render(<InviteLinkList links={[{ email: "jorge@acme.dev", url: "https://lux/invite/a" }, { email: "filipa@acme.dev", url: "https://lux/invite/b", note: <span className="badge">already invited</span> }]} />);
    const rows = [...el.querySelectorAll("li")];
    expect(rows.map((r) => r.querySelector(".invlinks-url")!.textContent)).toEqual(["https://lux/invite/a", "https://lux/invite/b"]);
    expect(rows[1]!.textContent).toContain("already invited");
    expect(rows.map((r) => r.querySelector("button")!.getAttribute("aria-label"))).toEqual(["Copy link for jorge@acme.dev", "Copy link for filipa@acme.dev"]);
  });
});
