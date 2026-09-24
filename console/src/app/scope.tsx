// Global scope (tenant + time range) shared by every page. Persisted in the URL
// query (router.tsx's location store) so links carry their scope.
import { createContext, useCallback, useContext, useMemo, type ReactNode } from "react";
import { ALL_TENANTS, type TimeRange } from "../ds/index.ts";
import { setSearchParams, useSearchParams } from "./router.tsx";

export interface Scope {
  /** Tenant id, or ALL_TENANTS. */
  tenant: string;
  /** The tenant to pass to list calls: undefined for all tenants. */
  apiTenant: string | undefined;
  range: TimeRange;
  setTenant: (t: string) => void;
  setRange: (r: TimeRange) => void;
}

const ScopeCtx = createContext<Scope | null>(null);

const RANGES: TimeRange[] = ["1h", "6h", "24h", "7d", "30d"];

export function ScopeProvider({ children }: { children: ReactNode }) {
  const params = useSearchParams();
  const tenant = params.get("tenant") ?? ALL_TENANTS;
  const r = params.get("range");
  const range: TimeRange = RANGES.includes(r as TimeRange) ? (r as TimeRange) : "24h";
  const setTenant = useCallback((t: string) => setSearchParams({ tenant: t === ALL_TENANTS ? null : t }), []);
  const setRange = useCallback((x: TimeRange) => setSearchParams({ range: x === "24h" ? null : x }), []);
  const value = useMemo(() => ({ tenant, apiTenant: tenant === ALL_TENANTS ? undefined : tenant, range, setTenant, setRange }), [tenant, range, setTenant, setRange]);
  return <ScopeCtx.Provider value={value}>{children}</ScopeCtx.Provider>;
}

export function useScope(): Scope {
  const s = useContext(ScopeCtx);
  if (!s) throw new Error("useScope outside ScopeProvider");
  return s;
}
