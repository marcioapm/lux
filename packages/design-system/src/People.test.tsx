import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { Avatar, initials } from "./Avatar.tsx";
import { breakableEmail, CopyAllLinks, inviteLinksText, InviteLinkList } from "./InviteLinkList.tsx";
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

  // A5: two addresses with the same prefix must read apart on a phone. The
  // address is whole in the DOM (wrapping at @ and .), the badges are out of
  // its line, and the stylesheet never ellipsizes it; narrow, the badges
  // take their own row under it.
  test("InviteLinkList: the address is whole, wraps instead of truncating; narrow, badges go under it", async () => {
    const emails = ["engineering.alice@acme.dev", "engineering.bob@acme.dev"];
    const el = await render(<InviteLinkList links={emails.map((email, i) => ({ email, url: `https://lux.example.com/invite/${i}`, note: <><span className="badge">Admin</span><span className="badge">renewed</span></> }))} />);
    const rows = [...el.querySelectorAll("li")];
    for (const [i, r] of rows.entries()) {
      const e = r.querySelector(".invlinks-email")!;
      expect(e.textContent).toBe(emails[i]!);
      expect(e.getAttribute("title")).toBe(emails[i]!);
      // Break opportunities: after "@", before each ".".
      expect(e.querySelectorAll("wbr").length).toBe(3);
      expect(e.querySelector(".badge")).toBeNull();
      expect(r.querySelector(".invlinks-note")!.textContent).toBe("Adminrenewed");
      expect(r.querySelector(".invlinks-copy button")).not.toBeNull();
    }
    const css = await Bun.file(new URL("./components.css", import.meta.url)).text();
    const block = (sel: string, from = css) => from.slice(from.indexOf(`${sel} {`), from.indexOf("}", from.indexOf(`${sel} {`)));
    const email = block(".invlinks-email");
    expect(email).toContain("overflow-wrap: anywhere");
    expect(email).not.toMatch(/ellipsis|nowrap|overflow: hidden/);
    expect(block(".invlinks-url")).toContain("text-overflow: ellipsis");
    const narrow = css.slice(css.indexOf("@container invlinks (max-width: 479px)"));
    expect(narrow.length).toBeLessThan(css.length);
    expect(block(".invlinks li", narrow).replace(/\s+/g, " ")).toContain(`"email copy" "note copy" "url copy"`);
  });

  test("breakableEmail: the same text, breaks after @ and before .", async () => {
    const el = await render(<span>{breakableEmail("a.b@c.dev")}</span>);
    expect(el.textContent).toBe("a.b@c.dev");
    expect(el.innerHTML).toBe("<span>a<wbr>.b@<wbr>c<wbr>.dev</span>");
  });

  // The rendered control, not just inviteLinksText: a CopyAllLinks that put
  // the URLs alone (or anything else) on the clipboard fails here.
  test("CopyAllLinks: Copy all writes every link as `email  url` lines, exactly", async () => {
    const written: string[] = [];
    const before = Object.getOwnPropertyDescriptor(navigator, "clipboard");
    Object.defineProperty(navigator, "clipboard", { value: { writeText: async (s: string) => void written.push(s) }, configurable: true });
    try {
      const el = await render(<CopyAllLinks links={[{ email: "a@acme.dev", url: "https://lux.example.com/invite/aaa" }, { email: "b@acme.dev", name: "Bea", url: "https://lux.example.com/invite/bbb" }]} />);
      const b = [...el.querySelectorAll("button")].find((x) => x.textContent?.includes("Copy all"))!;
      await act(async () => b.click());
      await act(async () => new Promise((r) => setTimeout(r, 10)));
      expect(written).toEqual(["a@acme.dev  https://lux.example.com/invite/aaa\nb@acme.dev  https://lux.example.com/invite/bbb"]);
      expect(b.textContent).toContain("Copied");
    } finally {
      if (before) Object.defineProperty(navigator, "clipboard", before);
    }
  });
});
