// The session: an API key, kept in sessionStorage so a reload keeps you
// signed in but closing the tab does not; or, when luxd is behind Cloudflare
// Access, the person Access signed in (no key: the browser's Access cookie
// authenticates every call). Whether it is an operator's, and who, is
// learned from GET /v1/whoami.
import { useSyncExternalStore } from "react";
import type { WhoAmI } from "./types.ts";

const KEY = "lux.key";
const listeners = new Set<() => void>();

export type Role = "unknown" | "operator" | "tenant";

/** A person signed in by Cloudflare Access; picture: their photo's URL, if the identity provider has one. */
export interface User {
  email: string;
  name: string;
  picture?: string;
}

interface Session {
  key: string | null;
  role: Role;
  /** Signed in by Cloudflare Access, as this person (no key). */
  user: User | null;
  /** How luxd's console signs people in, once whoami has said (null until then). */
  consoleAuth: "key" | "cloudflare-access" | null;
  /** The last whoami answer for this session (null until it has answered). */
  whoami: WhoAmI | null;
  /** whoami answered 403 not_a_member: signed in, in no tenant (or not in the one asked for), not an operator. */
  notAMember: boolean;
}

let session: Session = { key: readKey(), role: "unknown", user: null, consoleAuth: null, whoami: null, notAMember: false };

function readKey(): string | null {
  try {
    return sessionStorage.getItem(KEY);
  } catch {
    return null;
  }
}

function emit() {
  for (const l of listeners) l();
}

export function getSession(): Session {
  return session;
}

export function getKey(): string | null {
  return session.key;
}

export function signIn(key: string) {
  storeKey(key);
  session = { ...session, key, role: "unknown", user: null, whoami: null, notAMember: false };
  emit();
}

/** Keep the key for this tab's next document; false when storage refuses it. */
export function storeKey(key: string): boolean {
  try {
    sessionStorage.setItem(KEY, key);
    return true;
  } catch {
    return false;
  }
}

/** Signed in by the console auth luxd sits behind (Cloudflare Access). */
export function signInAs(user: User, role: Role) {
  session = { ...session, key: null, role, user, consoleAuth: "cloudflare-access" };
  emit();
}

export function signOut() {
  try {
    sessionStorage.removeItem(KEY);
  } catch {}
  session = { ...session, key: null, role: "unknown", user: null, whoami: null, notAMember: false };
  emit();
}

export function setRole(role: Role, consoleAuth?: Session["consoleAuth"]) {
  if (session.role === role && (consoleAuth == null || session.consoleAuth === consoleAuth)) return;
  session = { ...session, role, consoleAuth: consoleAuth ?? session.consoleAuth };
  emit();
}

/** What whoami said: the role, and who (a person, a service account) with their memberships. */
export function setWhoAmI(me: WhoAmI) {
  session = { ...session, role: me.operator ? "operator" : "tenant", consoleAuth: me.consoleAuth ?? session.consoleAuth, whoami: me, notAMember: false };
  emit();
}

/** whoami said 403 not_a_member (no identity body): the "not part of any tenant" screen, never a generic error. */
export function setNotAMember() {
  if (session.notAMember) return;
  session = { ...session, role: "tenant", whoami: null, notAMember: true };
  emit();
}

function subscribe(cb: () => void) {
  listeners.add(cb);
  return () => listeners.delete(cb);
}

export function useSession(): Session {
  return useSyncExternalStore(subscribe, () => session, () => session);
}
