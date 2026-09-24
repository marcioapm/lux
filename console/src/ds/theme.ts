import { useCallback, useEffect, useSyncExternalStore } from "react";

export type Theme = "light" | "dark" | "system";

const KEY = "lux.theme";
const listeners = new Set<() => void>();

function read(): Theme {
  try {
    const v = localStorage.getItem(KEY);
    if (v === "light" || v === "dark") return v;
  } catch {}
  return "system";
}

function apply(t: Theme) {
  const el = document.documentElement;
  if (t === "system") delete el.dataset.theme;
  else el.dataset.theme = t;
}

export function setTheme(t: Theme) {
  try {
    if (t === "system") localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, t);
  } catch {}
  apply(t);
  for (const l of listeners) l();
}

function subscribe(cb: () => void) {
  listeners.add(cb);
  const mq = window.matchMedia("(prefers-color-scheme: dark)");
  mq.addEventListener("change", cb);
  return () => {
    listeners.delete(cb);
    mq.removeEventListener("change", cb);
  };
}

/** The effective color scheme currently rendered, "light" or "dark". */
export function resolvedTheme(): "light" | "dark" {
  const t = read();
  if (t !== "system") return t;
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

export function useTheme(): { theme: Theme; resolved: "light" | "dark"; toggle: () => void; set: (t: Theme) => void } {
  const theme = useSyncExternalStore(subscribe, read, () => "system" as Theme);
  const resolved = useSyncExternalStore(subscribe, resolvedTheme, () => "light" as const);
  useEffect(() => apply(theme), [theme]);
  const toggle = useCallback(() => setTheme(resolvedTheme() === "dark" ? "light" : "dark"), []);
  return { theme, resolved, toggle, set: setTheme };
}

/** Read a CSS custom property off :root (used by canvas charts). */
export function cssVar(name: string, el: Element = document.documentElement): string {
  return getComputedStyle(el).getPropertyValue(name).trim();
}
