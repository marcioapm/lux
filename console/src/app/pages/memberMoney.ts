// Member and service-account cost: one row per principal and currency (as
// GET /v1/costs?by=member|owner sends them), never added across currencies.
import { sumMoney, type MoneyAmount } from "@lux/design-system";
import type { CostByOwnerRow } from "../../api/index.ts";

/** A principal's (or several's) cost: one amount per currency, and its Runs. */
export interface PrincipalCost {
  amounts: MoneyAmount[];
  runs: number | null;
}

/** Each key's rows (its currencies), in one pass over rows. */
export function costsByKey(rows: readonly CostByOwnerRow[]): Map<string, CostByOwnerRow[]> {
  const m = new Map<string, CostByOwnerRow[]>();
  for (const r of rows) {
    const list = m.get(r.key);
    if (list) list.push(r);
    else m.set(r.key, [r]);
  }
  return m;
}

/**
 * The cost of these rows, summed per currency with sumMoney (exact
 * decimals), currencies in name order. runs: per principal (row key), a
 * Run is counted in each of its currencies' rows, so that principal's
 * largest per-currency count; principals' Runs are distinct, so added.
 */
export function principalCost(rows: readonly CostByOwnerRow[] | undefined): PrincipalCost {
  if (!rows || rows.length === 0) return { amounts: [], runs: null };
  const byCurrency = new Map<string, string[]>();
  // key -> currency -> Runs
  const runs = new Map<string, Map<string, number>>();
  for (const r of rows) {
    const a = byCurrency.get(r.currency);
    if (a) a.push(r.amount);
    else byCurrency.set(r.currency, [r.amount]);
    if (r.runs != null) {
      let m = runs.get(r.key);
      if (!m) runs.set(r.key, (m = new Map()));
      m.set(r.currency, (m.get(r.currency) ?? 0) + r.runs);
    }
  }
  const amounts = [...byCurrency.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([currency, list]) => ({ currency, amount: sumMoney(list) ?? "0" }));
  let total = 0;
  for (const m of runs.values()) total += Math.max(...m.values());
  return { amounts, runs: runs.size ? total : null };
}

/** The cost of several keys together (a person and the accounts they own), per currency. */
export function keysCost(byKey: ReadonlyMap<string, readonly CostByOwnerRow[]>, keys: readonly string[]): PrincipalCost {
  return principalCost(keys.flatMap((k) => byKey.get(k) ?? []));
}

/** Each currency's total over every row, computed once (a share is a row's amount over its currency's total). */
export function totalsByCurrency(rows: readonly CostByOwnerRow[]): Map<string, number> {
  const byCurrency = new Map<string, string[]>();
  for (const r of rows) {
    const a = byCurrency.get(r.currency);
    if (a) a.push(r.amount);
    else byCurrency.set(r.currency, [r.amount]);
  }
  const out = new Map<string, number>();
  for (const [c, list] of byCurrency) out.set(c, Number(sumMoney(list) ?? "0"));
  return out;
}

/** The first currency's amount, for sorting a column of per-currency lists. */
export function sortAmount(c: PrincipalCost): number | null {
  return c.amounts.length ? Number(c.amounts[0]!.amount) : null;
}
