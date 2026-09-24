// API key session. Kept in sessionStorage so a reload keeps you signed in but
// closing the tab does not. Whether the key is an operator's is learned from
// the first GET /v1/tenants (403 for tenant keys).
import { useSyncExternalStore } from "react";

const KEY = "lux.key";
const listeners = new Set<() => void>();

export type Role = "unknown" | "operator" | "tenant";

interface Session {
  key: string | null;
  role: Role;
}

let session: Session = { key: readKey(), role: "unknown" };

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

export function getKey(): string | null {
  return session.key;
}

export function signIn(key: string) {
  try {
    sessionStorage.setItem(KEY, key);
  } catch {}
  session = { key, role: "unknown" };
  emit();
}

export function signOut() {
  try {
    sessionStorage.removeItem(KEY);
  } catch {}
  session = { key: null, role: "unknown" };
  emit();
}

export function setRole(role: Role) {
  if (session.role === role) return;
  session = { ...session, role };
  emit();
}

function subscribe(cb: () => void) {
  listeners.add(cb);
  return () => listeners.delete(cb);
}

export function useSession(): Session {
  return useSyncExternalStore(subscribe, () => session, () => session);
}
