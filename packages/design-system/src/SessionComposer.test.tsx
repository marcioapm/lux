import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { renderToStaticMarkup } from "react-dom/server";
import { SessionComposer } from "./SessionComposer.tsx";
import { SessionTranscript } from "./SessionTranscript.tsx";

const mounted: { el: HTMLElement; root: Root }[] = [];
async function render(node: React.ReactNode) {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  mounted.push({ el, root });
  await act(async () => root.render(node));
  return el;
}

// A typed change as a browser delivers it: React tracks the value, so set it through the native setter.
async function type(area: HTMLTextAreaElement, value: string) {
  await act(async () => {
    area.focus();
    area.dispatchEvent(new Event("focusin", { bubbles: true }));
    Object.getOwnPropertyDescriptor(Object.getPrototypeOf(area), "value")!.set!.call(area, value);
    area.dispatchEvent(new Event("input", { bubbles: true }));
    area.dispatchEvent(new KeyboardEvent("keyup", { key: "x", bubbles: true }));
  });
}

async function key(area: HTMLTextAreaElement, init: KeyboardEventInit) {
  let ev!: KeyboardEvent;
  await act(async () => {
    ev = new KeyboardEvent("keydown", { bubbles: true, cancelable: true, ...init });
    area.dispatchEvent(ev);
  });
  return ev;
}

const settle = () => act(async () => Bun.sleep(5));

describe("SessionComposer", () => {
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

  const parts = (el: HTMLElement) => {
    const form = el.querySelector<HTMLFormElement>('form[aria-label="Message the agent"]')!;
    const buttons = [...form.querySelectorAll<HTMLButtonElement>("button")];
    return {
      form,
      area: form.querySelector<HTMLTextAreaElement>("textarea")!,
      send: buttons.find((b) => b.textContent === "Send")!,
      interrupt: buttons.find((b) => b.textContent === "Interrupt"),
      alert: () => form.querySelector('[role="alert"]'),
    };
  };
  const described = (n: Element) => (n.getAttribute("aria-describedby") ?? "").split(" ").filter(Boolean).map((id) => document.getElementById(id)?.textContent);

  test("the textarea is labelled; Send is off until the trimmed draft has text", async () => {
    const el = await render(<SessionComposer label="Message the agent" onSend={async () => {}} />);
    const p = parts(el);
    expect(el.querySelector(`label[for="${p.area.id}"]`)?.textContent).toBe("Message the agent");
    expect(p.send.disabled).toBe(true);
    await type(p.area, "   \n ");
    expect(p.send.disabled).toBe(true);
    await type(p.area, " go ");
    expect(p.send.disabled).toBe(false);
  });

  test("a send that succeeds posts the trimmed text and clears the draft", async () => {
    const sent: string[] = [];
    const el = await render(<SessionComposer label="Message the agent" onSend={async (t) => void sent.push(t)} />);
    const p = parts(el);
    await type(p.area, "  add jitter \n");
    await act(async () => p.send.click());
    await settle();
    expect(sent).toEqual(["add jitter"]);
    expect(p.area.value).toBe("");
    expect(p.alert()).toBeNull();
  });

  test("while sending the draft is read-only and Send is loading; Ctrl+Enter then does not send again; a failure keeps the draft and says why in an alert", async () => {
    let reject!: (e: Error) => void;
    let calls = 0;
    const el = await render(
      <SessionComposer
        label="Message the agent"
        hint="Root session"
        onSend={() => {
          calls++;
          return new Promise((_, r) => (reject = r));
        }}
      />,
    );
    const p = parts(el);
    await type(p.area, "keep me");
    await act(async () => p.send.click());
    expect(p.area.readOnly).toBe(true);
    expect(p.send.getAttribute("aria-busy")).toBe("true");
    await key(p.area, { key: "Enter", ctrlKey: true });
    expect(calls).toBe(1);
    expect(p.area.hasAttribute("aria-invalid")).toBe(false);
    await act(async () => reject(new Error("run is stopped")));
    await settle();
    expect(p.area.value).toBe("keep me");
    expect(p.area.readOnly).toBe(false);
    expect(p.alert()?.textContent).toBe("run is stopped");
    // The field is described by its hint and the error, and marked invalid.
    expect(described(p.area)).toEqual(["Root session", "run is stopped"]);
    expect(p.area.getAttribute("aria-invalid")).toBe("true");
    // The next edit clears the error.
    await type(p.area, "keep me!");
    expect(p.alert()).toBeNull();
    expect(described(p.area)).toEqual(["Root session"]);
    expect(p.area.hasAttribute("aria-invalid")).toBe(false);
  });

  test("a successful retry after a failure clears the alert and the draft", async () => {
    let fail = true;
    const el = await render(
      <SessionComposer
        label="Message the agent"
        onSend={async () => {
          if (fail) throw new Error("run is stopped");
        }}
      />,
    );
    const p = parts(el);
    await type(p.area, "again");
    await act(async () => p.send.click());
    await settle();
    expect(p.alert()?.textContent).toBe("run is stopped");
    fail = false;
    await act(async () => p.send.click());
    await settle();
    expect(p.alert()).toBeNull();
    expect(p.area.value).toBe("");
  });

  test("disabled: textarea, Send and Interrupt are off, the placeholder says why, and Ctrl+Enter sends nothing even with a draft", async () => {
    let calls = 0;
    const el = await render(<SessionComposer label="Message the agent" disabled defaultValue="restored draft" placeholder="Resume this Run to send a message" hint="Run stopped" onSend={async () => void calls++} onInterrupt={() => void calls++} />);
    const p = parts(el);
    expect(p.area.disabled).toBe(true);
    expect(p.area.value).toBe("restored draft");
    expect(p.area.placeholder).toBe("Resume this Run to send a message");
    expect(p.send.disabled).toBe(true);
    expect(p.interrupt?.disabled).toBe(true);
    await key(p.area, { key: "Enter", ctrlKey: true });
    await settle();
    expect(calls).toBe(0);
  });

  test("Ctrl+Enter and ⌘+Enter send from the textarea; plain Enter is left to insert a newline", async () => {
    const sent: string[] = [];
    const el = await render(<SessionComposer label="Message the agent" onSend={async (t) => void sent.push(t)} />);
    const p = parts(el);
    expect(p.area.getAttribute("aria-keyshortcuts")).toBe("Control+Enter Meta+Enter");
    await type(p.area, "line one");
    const plain = await key(p.area, { key: "Enter" });
    expect(plain.defaultPrevented).toBe(false);
    expect(sent).toEqual([]);
    await key(p.area, { key: "Enter", ctrlKey: true });
    await settle();
    expect(sent).toEqual(["line one"]);
    await type(p.area, "two");
    await key(p.area, { key: "Enter", metaKey: true });
    await settle();
    expect(sent).toEqual(["line one", "two"]);
  });

  test("Ctrl/⌘+Enter that confirms an IME candidate (isComposing, or keyCode 229) is left to the IME and sends nothing", async () => {
    const sent: string[] = [];
    const el = await render(<SessionComposer label="Message the agent" onSend={async (t) => void sent.push(t)} />);
    const p = parts(el);
    await type(p.area, "にほん");
    const composing = await key(p.area, { key: "Enter", ctrlKey: true, isComposing: true });
    const legacy = await key(p.area, { key: "Enter", metaKey: true, keyCode: 229 });
    await settle();
    expect(sent).toEqual([]);
    expect(composing.defaultPrevented).toBe(false);
    expect(legacy.defaultPrevented).toBe(false);
    expect(p.area.value).toBe("にほん");
  });

  test("Interrupt calls onInterrupt and is loading while in flight; without one there is no Interrupt button", async () => {
    let n = 0;
    let done!: () => void;
    const el = await render(<SessionComposer label="Message the agent" onSend={async () => {}} onInterrupt={() => (n++, new Promise<void>((r) => (done = r)))} />);
    const interrupt = parts(el).interrupt!;
    await act(async () => interrupt.click());
    expect(n).toBe(1);
    expect(interrupt.getAttribute("aria-busy")).toBe("true");
    await act(async () => done());
    await settle();
    expect(interrupt.hasAttribute("aria-busy")).toBe(false);
    const bare = await render(<SessionComposer label="Send to stdin" onSend={async () => {}} />);
    expect([...bare.querySelectorAll("button")].map((b) => b.textContent)).toEqual(["Send"]);
  });

  test("a rejecting onInterrupt is caught: no unhandled rejection, and Interrupt is usable again", async () => {
    const unhandled: unknown[] = [];
    const onUnhandled = (reason: unknown) => void unhandled.push(reason);
    process.on("unhandledRejection", onUnhandled);
    try {
      const el = await render(<SessionComposer label="Message the agent" onSend={async () => {}} onInterrupt={() => Promise.reject(new Error("nope"))} />);
      const interrupt = parts(el).interrupt!;
      await act(async () => interrupt.click());
      await settle();
      await act(async () => Bun.sleep(20));
      expect(unhandled).toEqual([]);
      expect(interrupt.disabled).toBe(false);
      expect(parts(el).alert()).toBeNull();
    } finally {
      process.off("unhandledRejection", onUnhandled);
    }
  });

  test("SessionTranscript: while following, a scroller resized by a growing composer stays pinned to the bottom; off, it is left; unmount disconnects", async () => {
    // happy-dom's ResizeObserver never fires: stand one in that the test triggers.
    const observers: { cb: () => void; targets: Element[]; live: boolean }[] = [];
    const Real = globalThis.ResizeObserver;
    globalThis.ResizeObserver = class {
      o: { cb: () => void; targets: Element[]; live: boolean };
      constructor(cb: () => void) {
        this.o = { cb, targets: [], live: true };
        observers.push(this.o);
      }
      observe(t: Element) {
        this.o.targets.push(t);
      }
      unobserve() {}
      disconnect() {
        this.o.live = false;
      }
    } as unknown as typeof ResizeObserver;
    try {
      const el = await render(<SessionTranscript itemCount={3} composer={<SessionComposer label="Message the agent" onSend={async () => {}} />} />);
      const scroller = el.querySelector<HTMLElement>(".transcript-scroll")!;
      Object.defineProperty(scroller, "scrollHeight", { configurable: true, value: 900 });
      const resize = () => act(async () => observers.filter((o) => o.live && o.targets.includes(scroller)).forEach((o) => o.cb()));
      scroller.scrollTop = 500;
      await resize();
      expect(scroller.scrollTop).toBe(900);

      const follow = el.querySelector<HTMLButtonElement>("button[aria-pressed]")!;
      await act(async () => follow.click());
      expect(follow.getAttribute("aria-pressed")).toBe("false");
      scroller.scrollTop = 400;
      await resize();
      expect(scroller.scrollTop).toBe(400);

      await act(async () => follow.click());
      expect(observers.filter((o) => o.live && o.targets.includes(scroller))).toHaveLength(1);
      const { root } = mounted.pop()!;
      await act(async () => root.unmount());
      expect(observers.filter((o) => o.live)).toEqual([]);
    } finally {
      globalThis.ResizeObserver = Real;
    }
  });
});

test("SessionTranscript: the read-only note is the default, and a composer replaces it", () => {
  const plain = renderToStaticMarkup(<SessionTranscript itemCount={0} />);
  expect(plain).toContain("Read-only");
  const withComposer = renderToStaticMarkup(<SessionTranscript itemCount={0} composer={<SessionComposer label="Message the agent" onSend={async () => {}} />} />);
  expect(withComposer).not.toContain("Read-only");
  expect(withComposer).toContain('aria-label="Message the agent"');
});
