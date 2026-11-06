import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act, useState } from "react";
import { createRoot, type Root } from "react-dom/client";
import { Dialog } from "./Dialog.tsx";
import { isEmail, PersonPicker, type PersonOption } from "./PersonPicker.tsx";

const PEOPLE: PersonOption[] = [
  { email: "ana@acme.dev", name: "Ana Ribeiro", sub: "Admin" },
  { email: "bruno@acme.dev", name: "Bruno Costa", sub: "Member" },
  { email: "eva@acme.dev", name: "Eva Moreira", sub: "Owner", disabled: "already the owner" },
];

test("isEmail", () => {
  expect(isEmail("a@b.io")).toBe(true);
  expect(isEmail(" a@b.io ")).toBe(true);
  expect(isEmail("a@b")).toBe(false);
  expect(isEmail("Ana")).toBe(false);
});

describe("in a DOM", () => {
  const mounted: { el: HTMLElement; root: Root }[] = [];
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

  let last = "";
  function Harness({ freeText }: { freeText?: boolean }) {
    const [v, setV] = useState("");
    last = v;
    return <PersonPicker people={PEOPLE} value={v} onChange={setV} freeText={freeText} label="Owner" hint="Becomes Owner" />;
  }
  async function render(node: React.ReactNode) {
    const el = document.createElement("div");
    document.body.appendChild(el);
    const root = createRoot(el);
    mounted.push({ el, root });
    await act(async () => root.render(node));
    return el;
  }
  async function type(input: HTMLInputElement, value: string) {
    await act(async () => {
      input.focus();
      input.dispatchEvent(new Event("focusin", { bubbles: true }));
      Object.getOwnPropertyDescriptor(Object.getPrototypeOf(input), "value")!.set!.call(input, value);
      input.dispatchEvent(new Event("input", { bubbles: true }));
      input.dispatchEvent(new KeyboardEvent("keyup", { key: "x", bubbles: true }));
    });
  }
  const options = (el: HTMLElement) => [...el.querySelectorAll('[role="option"]')];

  test("suggestions filter by name or email; a pick sets the email; a disabled one says why and is not picked", async () => {
    const el = await render(<Harness />);
    const input = el.querySelector("input")!;
    expect(el.querySelector("label")!.textContent).toBe("Owner");
    await act(async () => input.focus());
    expect(options(el).length).toBe(3);
    await type(input, "bru");
    expect(options(el).map((o) => o.querySelector(".person-name")!.textContent)).toEqual(["Bruno Costa"]);
    await act(async () => options(el)[0]!.dispatchEvent(new MouseEvent("mousedown", { bubbles: true })));
    expect(last).toBe("bruno@acme.dev");
    await type(input, "eva");
    const eva = options(el)[0]!;
    expect(eva.getAttribute("aria-disabled")).toBe("true");
    expect(eva.textContent).toContain("already the owner");
    await act(async () => eva.dispatchEvent(new MouseEvent("mousedown", { bubbles: true })));
    expect(last).toBe("eva");
  });

  test("free text: any email is a value; not an email says so; arrows and Enter pick", async () => {
    const el = await render(<Harness />);
    const input = el.querySelector("input")!;
    await type(input, "new@contractor.io");
    expect(last).toBe("new@contractor.io");
    expect(input.getAttribute("aria-invalid")).toBeNull();
    expect(el.textContent).toContain("Becomes Owner");
    await type(input, "not-an-email");
    expect(input.getAttribute("aria-invalid")).toBe("true");
    expect(el.textContent).toContain("Not an email address");
    await type(input, "a");
    await act(async () => input.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true })));
    await act(async () => input.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true })));
    expect(last).toBe(PEOPLE.filter((p) => p.email.includes("a") || p.name!.toLowerCase().includes("a"))[1]!.email);
  });

  test("exact email Enter retains the target; active descendant follows arrows and clears on close", async () => {
    const el = await render(<Harness freeText={false} />);
    const input = el.querySelector("input")!;
    await type(input, "bruno@acme.dev");
    expect(options(el)).toHaveLength(1);
    expect(document.getElementById(input.getAttribute("aria-activedescendant")!)?.textContent).toContain("Bruno Costa");
    await act(async () => input.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true })));
    expect(last).toBe("bruno@acme.dev");
    expect(input.hasAttribute("aria-activedescendant")).toBe(false);
    await type(input, "");
    await act(async () => input.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true })));
    const active = document.getElementById(input.getAttribute("aria-activedescendant")!)!;
    expect(active.textContent).toContain("Bruno");
    expect(active.getAttribute("aria-selected")).toBe("true");
    expect(options(el).filter((o) => o.getAttribute("aria-selected") === "true")).toHaveLength(1);
    expect(document.getElementById(input.getAttribute("aria-describedby")!)?.textContent).toBe("Becomes Owner");
    await type(input, "no-match");
    expect(input.hasAttribute("aria-activedescendant")).toBe(false);
    expect(document.getElementById(input.getAttribute("aria-describedby")!)?.textContent).toContain("Pick one");
  });

  test("touch picks and multiple pickers have unique option IDs", async () => {
    const el = await render(<><Harness /><Harness /></>);
    const inputs = el.querySelectorAll("input");
    await type(inputs[0]!, "bru");
    const first = options(el)[0]!.id;
    await act(async () => options(el)[0]!.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true, pointerType: "touch" })));
    expect(last).toBe("bruno@acme.dev");
    await type(inputs[1]!, "bru");
    expect(options(el)[0]!.id).not.toBe(first);
  });

  test("Escape consumes only the first key inside Dialog", async () => {
    function InDialog() {
      const [open, setOpen] = useState(true);
      return <Dialog open={open} title="Pick" onConfirm={() => {}} onCancel={() => setOpen(false)}><Harness /></Dialog>;
    }
    const el = await render(<InDialog />);
    const input = el.querySelector("input")!;
    const dialog = el.querySelector("dialog")!;
    await type(input, "");
    expect(options(el).length).toBe(3);
    // happy-dom has no native Escape default: emulate cancellation only if
    // the input did not prevent the key's default (real browser checks too).
    async function escape() {
      const key = new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true });
      await act(async () => {
        input.dispatchEvent(key);
        if (!key.defaultPrevented) dialog.dispatchEvent(new Event("cancel", { cancelable: true }));
      });
    }
    await escape();
    expect(options(el)).toHaveLength(0);
    expect(dialog.open).toBe(true);
    expect(input.hasAttribute("aria-activedescendant")).toBe(false);
    await escape();
    expect(dialog.open).toBe(false);
  });

  test("freeText off: an address not among the people is invalid", async () => {
    const el = await render(<Harness freeText={false} />);
    const input = el.querySelector("input")!;
    await type(input, "new@contractor.io");
    expect(el.textContent).toContain("Pick one of the suggestions");
    await type(input, "ANA@acme.dev");
    expect(input.getAttribute("aria-invalid")).toBeNull();
  });
});


test("PersonPicker list overrides the shared popup positioning even when Select styles follow it", async () => {
  const css = await Bun.file(new URL("./components.css", import.meta.url)).text();
  // It also carries select-pop; the combined selector outranks that later rule.
  const block = /\.select-pop\.person-picker-list\s*\{([^}]+)\}/.exec(css)?.[1];
  expect(block).toBeDefined();
  expect(block).toContain("position: static");
});
