// Mounting a people page in a test as one of the mock's roles: the fake
// fetch answers from the dev mock's people routes (mockPeople.ts), so the
// tests and `bun run mock` show the same data.
import { act, type ReactNode } from "react";
import { createRoot } from "react-dom/client";
import { ToastProvider } from "@lux/design-system";
import { callerOf, resetPeople, whoamiFor } from "../../mockPeople.ts";
import { peopleApi } from "../../mockPeopleApi.ts";
import * as api from "../api/index.ts";
import { ScopeProvider } from "./scope.tsx";
import { fakeApi, until, type FakeApi } from "./testing.ts";

export interface Mounted {
  el: HTMLElement;
  fake: FakeApi;
  /** Buttons outside dialogs, by their text or accessible name. */
  button: (label: string) => HTMLButtonElement | undefined;
  /** The open dialog's element. */
  dialog: () => HTMLDialogElement | undefined;
  text: () => string;
  until: (ok: () => boolean, what: string) => Promise<void>;
  done: () => Promise<void>;
}

/** Visible text: without aria-hidden decoration and sr-only text, element boundaries as spaces. */
export function visibleText(n: Element): string {
  const c = n.cloneNode(true) as Element;
  for (const h of c.querySelectorAll('[aria-hidden="true"], .sr-only, dialog:not([open])')) h.remove();
  const parts: string[] = [];
  const walk = (x: Node) => {
    if (x.nodeType === 3) parts.push(x.textContent ?? "");
    // <wbr>: a break opportunity, no text and no boundary.
    else if (x.nodeName === "WBR") return;
    else {
      parts.push(" ");
      x.childNodes.forEach(walk);
      parts.push(" ");
    }
  };
  walk(c);
  return parts.join("").replace(/\s+/g, " ").trim();
}

/**
 * Render node at url as role (luxk_mock_<role>), in Access or key mode.
 * extra answers paths the people routes do not (a Run, runs); a path it
 * returns undefined for answers {}.
 */
export async function mountAs(role: string, url: string, node: ReactNode, opts: { auth?: "cloudflare-access" | "key"; extra?: (path: string, method: string, body: unknown) => unknown } = {}): Promise<Mounted> {
  const auth = opts.auth ?? "cloudflare-access";
  resetPeople();
  const key = `luxk_mock_${role}`;
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL(`http://localhost${url}`);
  api.signIn(key);
  const who = (path: string) => {
    const req = { headers: { get: (k: string) => (k.toLowerCase() === "authorization" ? `Bearer ${key}` : null) } } as unknown as Request;
    return callerOf(req, new URL(`http://localhost${path}`), false);
  };
  const caller = who(url);
  if (caller) api.setWhoAmI(whoamiFor(caller, auth, null) as unknown as api.WhoAmI);
  const fake = fakeApi(async (path, method, body) => {
    const x = opts.extra?.(path, method, body);
    if (x !== undefined) return x;
    // A plain request-like object: happy-dom's Request (a global while registered) may never settle a body read.
    const headers = new Map([["authorization", `Bearer ${key}`]]);
    const req = { method, headers: { get: (k: string) => headers.get(k.toLowerCase()) ?? null }, json: async () => body ?? {} } as unknown as Request;
    const u = new URL(`http://localhost${path}`);
    const r = await peopleApi(req, u, callerOf(req, u, false), auth);
    if (!r) return {};
    if (r.status === 204) return new Response(null, { status: 204 });
    return new Response(JSON.stringify(r.body), { status: r.status, headers: { "Content-Type": "application/json" } });
  });
  // fakeApi moves the page to the origin: back to the page under test.
  (window as unknown as { happyDOM: { setURL: (u: string) => void } }).happyDOM.setURL(`http://localhost${url}`);
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <ScopeProvider>
        <ToastProvider>{node}</ToastProvider>
      </ScopeProvider>,
    ),
  );
  return {
    el,
    fake,
    button: (label) => [...el.querySelectorAll("button")].find((b) => !b.closest("dialog") && (visibleText(b) === label || b.getAttribute("aria-label") === label)) as HTMLButtonElement | undefined,
    dialog: () => [...el.querySelectorAll("dialog")].find((d) => d.open),
    text: () => visibleText(el),
    until: (ok, what) => until(ok, what, 3000),
    done: async () => {
      await act(async () => root.unmount());
      el.remove();
      fake.restore();
      api.signOut();
    },
  };
}

/** Click, inside act. */
export async function click(b: Element | null | undefined) {
  if (!b) throw new Error("nothing to click");
  await act(async () => (b as HTMLElement).click());
}

/** Set an input's value as a browser would (React tracks it). */
export async function fill(input: HTMLInputElement | null | undefined, value: string) {
  if (!input) throw new Error("no input");
  await act(async () => {
    input.focus();
    input.dispatchEvent(new Event("focusin", { bubbles: true }));
    Object.getOwnPropertyDescriptor(Object.getPrototypeOf(input), "value")!.set!.call(input, value);
    input.dispatchEvent(new Event("input", { bubbles: true }));
    input.dispatchEvent(new KeyboardEvent("keyup", { key: "x", bubbles: true }));
  });
}

/** A dialog's button by its text. */
export function dialogButton(d: HTMLDialogElement | undefined, label: string): HTMLButtonElement | undefined {
  return [...(d?.querySelectorAll("button") ?? [])].find((b) => visibleText(b) === label) as HTMLButtonElement | undefined;
}
