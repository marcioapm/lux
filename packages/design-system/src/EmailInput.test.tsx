import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act, useState } from "react";
import { createRoot, type Root } from "react-dom/client";
import { classifyEmails, emailCounts, EmailInput, inviteButtonText, parseEmails, type EmailEntry, type EmailVerdict } from "./EmailInput.tsx";

const pick = (es: EmailEntry[]) => es.map((e) => (e.email ? (e.name ? `${e.name} <${e.email}>` : e.email) : `!${e.raw}`));

describe("parseEmails", () => {
  test("Name <email> keeps the name; addresses are lower-cased", () => {
    expect(pick(parseEmails("Jorge Faria <Jorge@ACME.dev>"))).toEqual(["Jorge Faria <jorge@acme.dev>"]);
    expect(pick(parseEmails('"Faria, Jorge" <j@acme.dev>, b@acme.dev'))).toEqual(["Faria, Jorge <j@acme.dev>", "b@acme.dev"]);
  });
  test("commas, semicolons, whitespace and lines separate", () => {
    expect(pick(parseEmails("a@x.io, b@x.io;c@x.io d@x.io\ne@x.io\r\n\nf@x.io"))).toEqual(["a@x.io", "b@x.io", "c@x.io", "d@x.io", "e@x.io", "f@x.io"]);
  });
  test("spreadsheet rows: the cell with an @ is the address, another the name", () => {
    expect(pick(parseEmails("Ana Ribeiro\tana@acme.dev\tadmin\nbruno@acme.dev\tBruno Costa"))).toEqual(["Ana Ribeiro <ana@acme.dev>", "Bruno Costa <bruno@acme.dev>"]);
  });
  test("what is not an email stays, marked", () => {
    expect(pick(parseEmails("tomás@, nope"))).toEqual(["!tomás@", "!nope"]);
  });
  test("duplicates are flagged after the first; counts", () => {
    const es = parseEmails("a@x.io, A@x.io, bad, b@x.io, c@x.io");
    const views = classifyEmails(es, (e) => (e.email === "c@x.io" ? { status: "renew" } : e.email === "b@x.io" ? { status: "skip", reason: "already in acme" } : { status: "invite" }));
    expect(views.map((v) => v.status)).toEqual(["invite", "duplicate", "invalid", "skip", "renew"]);
    expect(emailCounts(views)).toEqual({ taken: 2, renewed: 1, left: 3 });
  });
  test("button text carries the count", () => {
    expect([0, 1, 3].map(inviteButtonText)).toEqual(["Nothing to invite", "Invite 1 person", "Invite 3 people"]);
  });
});

const mounted: { el: HTMLElement; root: Root }[] = [];

function Harness({ initial = "", classify }: { initial?: string; classify?: (e: EmailEntry & { email: string }) => EmailVerdict }) {
  const [entries, setEntries] = useState(() => parseEmails(initial));
  const views = classifyEmails(entries, classify ?? (() => ({ status: "invite" })));
  return (
    <div>
      <EmailInput id="ei" entries={entries} onChange={setEntries} classify={classify ?? (() => ({ status: "invite" }))} hint="Type or paste." />
      <button type="submit">{inviteButtonText(emailCounts(views).taken)}</button>
    </div>
  );
}

describe("EmailInput in a DOM", () => {
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

  async function render(props: Parameters<typeof Harness>[0] = {}) {
    const el = document.createElement("div");
    document.body.appendChild(el);
    const root = createRoot(el);
    mounted.push({ el, root });
    await act(async () => root.render(<Harness {...props} />));
    const input = el.querySelector<HTMLInputElement>("#ei")!;
    return {
      el,
      input,
      chips: () => [...el.querySelectorAll(".emailchip")].map((c) => c.querySelector(".emailchip-text")!.textContent),
      typeKey: async (text: string, key: string) => {
        await act(async () => {
          input.focus();
          input.dispatchEvent(new Event("focusin", { bubbles: true }));
          Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!.call(input, text);
          input.dispatchEvent(new Event("input", { bubbles: true }));
          input.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true }));
        });
      },
    };
  }

  test("hint only while empty; summary once there are chips", async () => {
    const r = await render();
    expect(r.el.textContent).toContain("Type or paste.");
    expect(r.el.querySelector(".emailinput-out")).toBeNull();
    await r.typeKey("a@x.io", "Enter");
    expect(r.chips()).toEqual(["a@x.io"]);
    expect(r.el.textContent).not.toContain("Type or paste.");
    expect(r.el.querySelector(".emailinput-sum")!.textContent).toBe("1 to invite");
    expect(r.el.querySelector("button[type=submit]")!.textContent).toBe("Invite 1 person");
  });

  test("Enter, comma, semicolon, space, Tab and blur commit", async () => {
    const r = await render();
    for (const [i, key] of ["Enter", ",", ";", " ", "Tab"].entries()) await r.typeKey(`u${i}@x.io`, key);
    await act(async () => {
      r.input.value = "last@x.io";
      r.input.dispatchEvent(new FocusEvent("focusout", { bubbles: true }));
      r.input.dispatchEvent(new FocusEvent("blur"));
    });
    expect(r.chips()).toEqual(["u0@x.io", "u1@x.io", "u2@x.io", "u3@x.io", "u4@x.io", "last@x.io"]);
    expect(r.input.value).toBe("");
  });

  test("space inside a name or before the @ does not commit", async () => {
    const r = await render();
    await r.typeKey("Tom", " ");
    expect(r.chips()).toEqual([]);
    await r.typeKey("Tom Okafor <tom", " ");
    expect(r.chips()).toEqual([]);
    await r.typeKey("Tom Okafor <tom@acme.dev>", "Enter");
    expect(r.chips()).toEqual(["Tom Okafor tom@acme.dev"]);
  });

  test("Backspace in the empty input removes the last chip; ✕ removes one", async () => {
    const r = await render({ initial: "a@x.io, b@x.io, c@x.io" });
    await r.typeKey("", "Backspace");
    expect(r.chips()).toEqual(["a@x.io", "b@x.io"]);
    await r.typeKey("zz", "Backspace");
    expect(r.chips()).toEqual(["a@x.io", "b@x.io"]);
    await act(async () => r.el.querySelector<HTMLButtonElement>('[aria-label="Remove a@x.io"]')!.click());
    expect(r.chips()).toEqual(["b@x.io"]);
  });

  test("paste splits lines and keeps names", async () => {
    const r = await render();
    await act(async () => {
      const ev = new Event("paste", { bubbles: true, cancelable: true }) as Event & { clipboardData: { getData: () => string } };
      ev.clipboardData = { getData: () => "Ana Ribeiro\tana@acme.dev\nb@acme.dev; c@acme.dev" };
      r.input.dispatchEvent(ev);
    });
    expect(r.chips()).toEqual(["Ana Ribeiro ana@acme.dev", "b@acme.dev", "c@acme.dev"]);
  });

  test("left out: tone only in the chip, reason once under it, Remove left out, one action", async () => {
    let copied = "";
    const r = await render({
      initial: "new@acme.dev, ana@acme.dev, filipa@acme.dev, rita@acme.dev, bad@, new@acme.dev",
      classify: (e) =>
        e.email === "ana@acme.dev"
          ? { status: "skip", reason: "already in acme" }
          : e.email === "filipa@acme.dev"
            ? { status: "skip", reason: "already invited", action: { label: "Copy their link", onClick: () => (copied = e.email) } }
            : e.email === "rita@acme.dev"
              ? { status: "renew" }
              : { status: "invite" },
    });
    const chips = [...r.el.querySelectorAll(".emailchip")];
    expect(chips.map((c) => c.getAttribute("data-tone"))).toEqual(["ok", "skip", "skip", "info", "bad", "skip"]);
    // No reason words inside the visible chip text; screen-reader text says the state.
    expect(chips[1]!.querySelector(".emailchip-text")!.textContent).toBe("ana@acme.dev");
    expect(chips.map((c) => c.querySelector(".sr-only")!.textContent)).toEqual([", will be invited", ", left out: already in acme", ", left out: already invited", ", will be renewed", ", left out: not an email", ", left out: twice in the list"]);
    expect(r.el.querySelector(".emailinput-count")!.textContent).toBe("2 to invite (1 renewed) · 4 left out");
    const left = [...r.el.querySelectorAll(".emailinput-left li")];
    expect(left.map((li) => [li.querySelector(".emailinput-left-addr")!.textContent, li.querySelector(".emailinput-left-why")!.textContent])).toEqual([
      ["ana@acme.dev", "already in acme"],
      ["filipa@acme.dev", "already invited"],
      ["bad@", "not an email"],
      ["new@acme.dev", "twice in the list"],
    ]);
    expect(left.map((li) => li.querySelectorAll("button").length)).toEqual([0, 1, 0, 0]);
    await act(async () => left[1]!.querySelector("button")!.click());
    expect(copied).toBe("filipa@acme.dev");
    const removeLeft = [...r.el.querySelectorAll("button")].find((b) => b.textContent === "Remove left out")!;
    await act(async () => removeLeft.click());
    expect(r.chips()).toEqual(["new@acme.dev", "rita@acme.dev"]);
    expect(r.el.querySelector(".emailinput-count")!.textContent).toBe("2 to invite (1 renewed)");
    expect(r.el.querySelector("button[type=submit]")!.textContent).toBe("Invite 2 people");
  });

  test("nothing valid: the button says Nothing to invite", async () => {
    const r = await render({ initial: "nope" });
    expect(r.el.querySelector("button[type=submit]")!.textContent).toBe("Nothing to invite");
  });

  test("unsafe input renders as text, never as an element", async () => {
    const r = await render({ initial: '<img src=x onerror="alert(1)">, javascript:alert(1), "<script>x</script>" <a@x.io>' });
    expect(r.el.querySelector("img")).toBeNull();
    expect(r.el.querySelector("script")).toBeNull();
    expect(r.el.querySelector("a")).toBeNull();
    expect(r.el.querySelectorAll(".emailchip").length).toBeGreaterThan(1);
    expect(r.el.textContent).toContain("javascript:alert(1)");
  });
});
