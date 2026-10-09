// What the Overview's Cost panel shows and how it buckets it: the Show
// filter (All / Compute / External), the granularity (Per), and the shaping
// of /v1/costs summaries into the panel's figures. Amounts stay decimal
// strings; nothing is ever added or ranked across currencies.
import { compareMoney, familyDisplay, sumMoney, type TimeRange } from "@lux/design-system";
import type { CostFamily, CostSummary, CostSummaryRow, MoneyTotal } from "../../api/index.ts";

/** Which costs the panel counts: every family, compute (host time) only, or every other family. */
export type CostShow = "all" | "compute" | "external";
/** The granularity asked for: auto picks one from the range. */
export type CostPer = "auto" | "hour" | "day";
export type CostInterval = "hour" | "day";

export const COMPUTE = "compute";

/** ?cost=: compute or external; anything else is All. */
export function parseShow(v: string | null): CostShow {
  return v === "compute" || v === "external" ? v : "all";
}

/** ?per=: hour or day; anything else is Auto. */
export function parsePer(v: string | null): CostPer {
  return v === "hour" || v === "day" ? v : "auto";
}

/** Whether a family counts under show. */
export function shows(show: CostShow, family: string): boolean {
  return show === "all" || (show === "compute") === (family === COMPUTE);
}

export type CostSince = "6h" | "24h" | "7d" | "30d";

/** The range costs are read over: 1h reads 6h, as costs are whole-hour buckets and an hour is one bar. */
export function costSince(range: TimeRange): CostSince {
  return range === "1h" ? "6h" : range;
}

const SINCE_HOURS: Record<CostSince, number> = { "6h": 6, "24h": 24, "7d": 168, "30d": 720 };
const INTERVAL_HOURS: Record<CostInterval, number> = { hour: 1, day: 24 };
/** Fewer buckets than this says nothing over time; more cannot be told apart. */
export const MIN_BUCKETS = 3;
export const MAX_BUCKETS = 200;

export interface PerChoice {
  value: CostPer;
  interval: CostInterval;
  buckets: number;
  /** Why it cannot be picked for this range; undefined when it can. */
  disabled?: string;
}

/** Auto, Hourly and Daily for a range: each with its bucket count, and the reason a choice is off. */
export function perChoices(since: CostSince): PerChoice[] {
  const hours = SINCE_HOURS[since];
  const auto: CostInterval = hours <= 24 ? "hour" : "day";
  const choice = (value: CostPer, interval: CostInterval): PerChoice => {
    const buckets = Math.ceil(hours / INTERVAL_HOURS[interval]);
    const disabled = buckets < MIN_BUCKETS ? `${buckets} ${buckets === 1 ? "bar" : "bars"} · too coarse` : buckets > MAX_BUCKETS ? `${buckets} bars · too many` : undefined;
    return { value, interval, buckets, disabled };
  };
  return [{ ...choice("auto", auto), disabled: undefined }, choice("hour", "hour"), choice("day", "day")];
}

/** The choice in effect: the one asked for, or Auto when it is off for this range. */
export function resolvePer(since: CostSince, per: CostPer): PerChoice {
  const all = perChoices(since);
  const asked = all.find((c) => c.value === per);
  return asked && !asked.disabled ? asked : all[0]!;
}

export function intervalWord(i: CostInterval): string {
  return i === "hour" ? "hourly" : "daily";
}

/** Amounts of one currency, exact; null when there are none (no figure is not a zero). */
function sum(amounts: string[]): string | null {
  return amounts.length ? sumMoney(amounts) : null;
}

function byCurrency<T extends { currency: string }>(rows: T[]): Map<string, T[]> {
  const m = new Map<string, T[]>();
  for (const r of rows) m.set(r.currency, [...(m.get(r.currency) ?? []), r]);
  return new Map([...m].sort(([a], [b]) => a.localeCompare(b)));
}

const familyOf = (r: CostSummaryRow) => r.group?.family ?? "(none)";

/** Rows grouped by family summed exactly per currency, counting only the families show keeps. */
export function shownTotals(rows: CostSummaryRow[], show: CostShow): MoneyTotal[] {
  return [...byCurrency(rows.filter((r) => shows(show, familyOf(r))))].flatMap(([currency, rs]) => {
    const amount = sum(rs.map((r) => r.amount));
    return amount == null ? [] : [{ currency, amount }];
  });
}

export interface SideTotals {
  currency: string;
  all: string | null;
  compute: string | null;
  external: string | null;
}

/** Per currency, the total and its Compute and External parts; a side with no row is null, not zero. */
export function sideTotals(rows: CostSummaryRow[]): SideTotals[] {
  return [...byCurrency(rows)].map(([currency, rs]) => ({
    currency,
    all: sum(rs.map((r) => r.amount)),
    compute: sum(rs.filter((r) => familyOf(r) === COMPUTE).map((r) => r.amount)),
    external: sum(rs.filter((r) => familyOf(r) !== COMPUTE).map((r) => r.amount)),
  }));
}

/** A ratio of two amounts of one currency, for a share or a bar's length; display only. */
export function ratio(part: string | null | undefined, whole: string | null | undefined): number | null {
  const p = Number(part);
  const w = Number(whole);
  return part == null || whole == null || !Number.isFinite(p) || !Number.isFinite(w) || w === 0 ? null : p / w;
}

export interface FamilyChart {
  currency: string;
  x: number[];
  ys: (number | null)[][];
  series: { label: string; color: string }[];
  /** Each series' total over the range, exact, aligned with series. */
  totals: string[];
}

/** Families in the order the panel shows them: compute first, then by key. */
export function familyOrder(a: string, b: string): number {
  return a === COMPUTE ? -1 : b === COMPUTE ? 1 : a.localeCompare(b);
}

/** One chart per currency: a series per family show keeps, aligned on every bucket of the range. */
export function familyCharts(d: CostSummary | undefined, interval: CostInterval, show: CostShow): FamilyChart[] {
  if (!d?.series?.length) return [];
  // Every bucket of the range: one with no row is a gap, not a zero.
  const step = INTERVAL_HOURS[interval] * 3600;
  const start = Math.floor(Date.parse(d.from) / 1000 / step) * step;
  const end = Math.floor(Date.parse(d.to) / 1000);
  const x: number[] = [];
  for (let t = start; t < end; t += step) x.push(t);
  const index = new Map(x.map((t, i) => [t, i]));
  // Label and colour as on the Run page: displayName and hint from the describe.
  const meta = new Map((d.families ?? []).map((f) => [f.family, f]));
  return [...byCurrency(d.series.filter((r) => shows(show, familyOf(r))))].map(([currency, rows]) => {
    const families = [...new Set(rows.map(familyOf))].sort(familyOrder);
    const display = familyDisplay(families.map((family) => meta.get(family) ?? { family }));
    const ys = families.map(() => x.map((): number | null => null));
    for (const r of rows) {
      const i = index.get(Math.floor(Date.parse(r.at!) / 1000));
      // Chart geometry only: the figures in text are formatted from the strings.
      if (i != null) ys[families.indexOf(familyOf(r))]![i] = Number(r.amount);
    }
    const totals = families.map((f) => sumMoney(rows.filter((r) => familyOf(r) === f).map((r) => r.amount)) ?? "0");
    return { currency, x, ys, series: families.map((f) => display.get(f)!), totals };
  });
}

export interface Part {
  family: string;
  amount: string;
}

export interface SplitRow {
  key: string;
  currency: string;
  /** What show counts: the figure the row is ranked by. */
  amount: string;
  /** Every family's part, compute first, hidden ones included (they are drawn muted). */
  parts: Part[];
  /** Every family's parts summed: the whole the split bar is drawn against. */
  all: string;
}

/** Rows' largest `limit` per currency by what show counts, largest first; never ranks across currencies. Rows grouped by `key` and family. */
export function topSplit(rows: CostSummaryRow[], key: string, show: CostShow, limit = 10): SplitRow[] {
  const out: SplitRow[] = [];
  for (const [currency, rs] of byCurrency(rows)) {
    const byKey = new Map<string, Part[]>();
    for (const r of rs) {
      const k = r.group?.[key];
      if (!k) continue;
      byKey.set(k, [...(byKey.get(k) ?? []), { family: familyOf(r), amount: r.amount }]);
    }
    const ranked: SplitRow[] = [];
    for (const [k, parts] of byKey) {
      const amount = sum(parts.filter((p) => shows(show, p.family)).map((p) => p.amount));
      if (amount == null) continue;
      ranked.push({ key: k, currency, amount, parts: parts.sort((a, b) => familyOrder(a.family, b.family)), all: sum(parts.map((p) => p.amount))! });
    }
    out.push(...ranked.sort((a, b) => compareMoney(b.amount, a.amount) || a.key.localeCompare(b.key)).slice(0, limit));
  }
  return out;
}

export interface FamilyRow {
  family: string;
  currency: string;
  amount: string;
  /** Of what show counts in this currency. */
  share: number | null;
}

/** The families show keeps, per currency, largest first, each with its share of the shown total. */
export function familyRows(rows: CostSummaryRow[], show: CostShow): FamilyRow[] {
  const totals = new Map(shownTotals(rows, show).map((t) => [t.currency, t.amount]));
  return [...byCurrency(rows.filter((r) => shows(show, familyOf(r))))].flatMap(([currency, rs]) =>
    rs
      .map((r) => ({ family: familyOf(r), currency, amount: r.amount, share: ratio(r.amount, totals.get(currency)) }))
      .sort((a, b) => compareMoney(b.amount, a.amount) || familyOrder(a.family, b.family)),
  );
}

/** The window of equal length just before a summary's own [from, to); null when its bounds do not parse. */
export function previousWindow(d: { from?: string; to?: string }): { from: string; to: string } | null {
  const from = Date.parse(d.from ?? "");
  const to = Date.parse(d.to ?? "");
  if (!Number.isFinite(from) || !Number.isFinite(to) || to <= from) return null;
  return { from: new Date(from - (to - from)).toISOString(), to: new Date(from).toISOString() };
}

export interface Change {
  currency: string;
  /** (now − before) / before; null when there is no earlier figure, or it was not above zero (no percentage means anything then). */
  ratio: number | null;
}

const negate = (a: string) => (a.startsWith("-") ? a.slice(1) : `-${a}`);

/** The change per currency from the previous window; a currency missing there has no change. */
export function changes(now: MoneyTotal[], before: MoneyTotal[]): Change[] {
  const prev = new Map(before.map((t) => [t.currency, t.amount]));
  return now.map((t) => {
    const p = prev.get(t.currency);
    return { currency: t.currency, ratio: p == null || compareMoney(p, "0") <= 0 ? null : ratio(sumMoney([t.amount, negate(p)]), p) };
  });
}

export interface Peak {
  currency: string;
  /** The bucket's start, epoch seconds. */
  at: number;
  amount: string;
}

/** Per currency, the bucket where what show counts was largest (the earliest on a tie). */
export function peaks(d: CostSummary | undefined, show: CostShow): Peak[] {
  const out: Peak[] = [];
  for (const [currency, rs] of byCurrency((d?.series ?? []).filter((r) => r.at && shows(show, familyOf(r))))) {
    const buckets = new Map<number, string[]>();
    for (const r of rs) {
      const at = Math.floor(Date.parse(r.at!) / 1000);
      buckets.set(at, [...(buckets.get(at) ?? []), r.amount]);
    }
    let best: Peak | null = null;
    for (const [at, amounts] of [...buckets].sort(([a], [b]) => a - b)) {
      const amount = sumMoney(amounts);
      if (amount != null && (best == null || compareMoney(amount, best.amount) > 0)) best = { currency, at, amount };
    }
    if (best) out.push(best);
  }
  return out;
}

/** The distinct peak buckets as [from, to) to ask the summary about, in time order. */
export function peakWindows(ps: Peak[], interval: CostInterval): { from: string; to: string }[] {
  const step = INTERVAL_HOURS[interval] * 3600;
  return [...new Set(ps.map((p) => p.at))].sort((a, b) => a - b).map((at) => ({ from: new Date(at * 1000).toISOString(), to: new Date((at + step) * 1000).toISOString() }));
}

/** The Run that cost the most (as show counts) in the peak bucket of each currency; rows grouped by run and family, each tagged with its window's start. */
export function peakRuns(ps: Peak[], windows: { at: number; rows: CostSummaryRow[] }[], show: CostShow): Map<string, string> {
  const out = new Map<string, string>();
  for (const p of ps) {
    const rows = windows.find((w) => w.at === p.at)?.rows.filter((r) => r.currency === p.currency) ?? [];
    const top = topSplit(rows, "run", show, 1)[0];
    if (top) out.set(p.currency, top.key);
  }
  return out;
}

/** Families by key, for describe metadata. */
export function familyMeta(families: CostFamily[] | undefined): Map<string, CostFamily> {
  return new Map((families ?? []).map((f) => [f.family, f]));
}
