// What the Overview's Cost panel shows: the Show filter (All / Compute /
// External), what it breaks down by, its label filters, and the shaping of
// /v1/costs summaries into the panel's figures. Amounts stay decimal
// strings; nothing is ever added or ranked across currencies. The step
// (hour or day) is the page's (every.ts).
import { compareMoney, familyDisplay, sumMoney, type TimeRange } from "@lux/design-system";
import type { CostFamily, CostKey, CostSummary, CostSummaryRow, MoneyTotal } from "../../api/index.ts";

/** Which costs the panel counts: every family, compute (host time) only, or every other family. */
export type CostShow = "all" | "compute" | "external";
export type CostInterval = "hour" | "day";

export const COMPUTE = "compute";

/** ?cost=: compute or external; anything else is All. */
export function parseShow(v: string | null): CostShow {
  return v === "compute" || v === "external" ? v : "all";
}

/** Whether a family counts under show. */
export function shows(show: CostShow, family: string): boolean {
  return show === "all" || (show === "compute") === (family === COMPUTE);
}

/** What the panel breaks cost down by. A label breakdown without a key takes the default key (defaultLabelKey). */
export type Breakdown = { kind: "family" } | { kind: "label"; key: string } | { kind: "key" } | { kind: "pool" } | { kind: "tenant" };
export type BreakdownKind = Breakdown["kind"];

/** ?by=: label:<key>, label, key, pool, or tenant (only where tenants are shown); anything else is Family. */
export function parseBreakdown(v: string | null, showTenant: boolean): Breakdown {
  if (v === "key" || v === "pool") return { kind: v };
  if (v === "tenant" && showTenant) return { kind: "tenant" };
  if (v === "label") return { kind: "label", key: "" };
  if (v?.startsWith("label:")) return { kind: "label", key: v.slice("label:".length) };
  return { kind: "family" };
}

/** The ?by= value of a breakdown: null for Family (the default). */
export function breakdownParam(b: Breakdown): string | null {
  return b.kind === "family" ? null : b.kind === "label" ? (b.key ? `label:${b.key}` : "label") : b.kind;
}

/** The summary group of a breakdown (its label key resolved). */
export function breakdownGroup(b: Breakdown): string {
  return b.kind === "label" ? `label:${b.key}` : b.kind;
}

/** The label key a label breakdown uses: its own, else app when Runs carry it, else the most common key. */
export function defaultLabelKey(asked: string, keys: string[]): string {
  return asked || (keys.includes("app") ? "app" : (keys[0] ?? "app"));
}

/** A label filter: the label is one of values (OR), or, with notSet, the Runs lack the key (values empty). Filters AND together. */
export interface LabelFilter {
  key: string;
  values: string[];
  notSet?: boolean;
}

/** ?label=key=value (repeated: one key's values OR) and ?nolabel=key, in the order first seen; a label without "=" is ignored. */
export function parseFilters(labels: string[], nolabels: string[]): LabelFilter[] {
  const out: LabelFilter[] = [];
  for (const l of labels) {
    const i = l.indexOf("=");
    if (i <= 0) continue;
    const key = l.slice(0, i);
    const value = l.slice(i + 1);
    const f = out.find((x) => x.key === key && !x.notSet);
    if (!f) out.push({ key, values: [value] });
    else if (!f.values.includes(value)) f.values.push(value);
  }
  for (const key of nolabels) if (key && !out.some((x) => x.key === key && x.notSet)) out.push({ key, values: [], notSet: true });
  return out;
}

/** The label and nolabel parameters of filters: in the page URL and in /v1/costs alike. */
export function filtersParams(fs: LabelFilter[]): { label: string[]; nolabel: string[] } {
  return {
    label: fs.flatMap((f) => (f.notSet ? [] : f.values.map((v) => `${f.key}=${v}`))),
    nolabel: fs.filter((f) => f.notSet).map((f) => f.key),
  };
}

/** The Runs list for the same Runs, where it can say so: it filters by one key=value; anything more is the plain list. */
export function runsListPath(fs: LabelFilter[]): string {
  const f = fs.length === 1 ? fs[0]! : null;
  return f && !f.notSet && f.values.length === 1 ? `/runs?label=${encodeURIComponent(`${f.key}=${f.values[0]}`)}` : "/runs";
}

/** The range costs are read over: 1h reads 6h, as costs are whole-hour buckets and an hour is one bar. */
export function costSince(range: TimeRange): CostSince {
  return range === "1h" ? "6h" : range;
}

export type CostSince = "6h" | "24h" | "7d" | "30d";

const INTERVAL_HOURS: Record<CostInterval, number> = { hour: 1, day: 24 };

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

/** The group value of Runs without the label, without a submitter on record, or (pool) cost tied to no host. */
export const NONE = "(none)";
/** The band of every value past the top ones; not a group value luxd sends. */
export const OTHER = "\u0000other";
/** Values stacked apart before the rest go into Other. */
export const TOP_VALUES = 7;

export interface Band {
  /** A group value, OTHER or NONE. */
  id: string;
  /** The group values it holds. */
  values: string[];
  color: string;
}

/** A band's colour: chart slots in rank order, Other the last slot, (none) grey. */
function bandColor(id: string, rank: number): string {
  return id === NONE ? "var(--st-neutral-dot)" : id === OTHER ? "var(--chart-8)" : `var(--chart-${rank + 1})`;
}

/**
 * Per currency, the bands a breakdown stacks: the `limit` values costing
 * the most (as show counts) in their own colours, the rest as Other, and
 * the value-less Runs (NONE) last, never dropped. A value with no shown cost
 * has no band. Rows are grouped by `dim` and family.
 */
export function breakdownBands(rows: CostSummaryRow[], dim: string, show: CostShow, limit = TOP_VALUES): Map<string, Band[]> {
  const out = new Map<string, Band[]>();
  for (const [currency, rs] of byCurrency(rows.filter((r) => shows(show, familyOf(r))))) {
    const amounts = new Map<string, string[]>();
    for (const r of rs) {
      const v = r.group?.[dim] ?? NONE;
      amounts.set(v, [...(amounts.get(v) ?? []), r.amount]);
    }
    const ranked = [...amounts]
      .filter(([v]) => v !== NONE)
      .map(([v, a]) => ({ v, amount: sumMoney(a) ?? "0" }))
      .sort((a, b) => compareMoney(b.amount, a.amount) || a.v.localeCompare(b.v));
    // Other only when it would hold two values or more: one value is shown as itself.
    const top = ranked.length > limit + 1 ? ranked.slice(0, limit) : ranked;
    const rest = ranked.slice(top.length);
    const bands: Band[] = top.map(({ v }, i) => ({ id: v, values: [v], color: bandColor(v, i) }));
    if (rest.length) bands.push({ id: OTHER, values: rest.map((x) => x.v), color: bandColor(OTHER, 0) });
    if (amounts.has(NONE)) bands.push({ id: NONE, values: [NONE], color: bandColor(NONE, 0) });
    out.set(currency, bands);
  }
  return out;
}

const bandOf = (bands: Band[], value: string) => bands.find((b) => b.values.includes(value));

export interface BreakdownChart {
  currency: string;
  x: number[];
  ys: (number | null)[][];
  bands: Band[];
  /** Each band's shown total over the range, exact, aligned with bands. */
  totals: string[];
}

/** One chart per currency: a stacked series per band over every bucket of the range (a bucket with no row is a gap), counting what show keeps. Series rows grouped by `dim` and family. */
export function breakdownCharts(d: CostSummary | undefined, dim: string, interval: CostInterval, show: CostShow, bands: Map<string, Band[]>): BreakdownChart[] {
  if (!d?.series?.length) return [];
  const step = INTERVAL_HOURS[interval] * 3600;
  const start = Math.floor(Date.parse(d.from) / 1000 / step) * step;
  const end = Math.floor(Date.parse(d.to) / 1000);
  const x: number[] = [];
  for (let t = start; t < end; t += step) x.push(t);
  const index = new Map(x.map((t, i) => [t, i]));
  const out: BreakdownChart[] = [];
  for (const [currency, rows] of byCurrency(d.series.filter((r) => shows(show, familyOf(r))))) {
    const bs = bands.get(currency) ?? [];
    const cells = bs.map(() => x.map((): string[] => []));
    const totals = bs.map((): string[] => []);
    for (const r of rows) {
      const b = bandOf(bs, r.group?.[dim] ?? NONE);
      const i = index.get(Math.floor(Date.parse(r.at!) / 1000));
      if (!b || i == null) continue;
      const k = bs.indexOf(b);
      cells[k]![i]!.push(r.amount);
      totals[k]!.push(r.amount);
    }
    // Chart geometry only: the figures in text are formatted from the strings.
    const ys = cells.map((c) => c.map((amounts) => (amounts.length ? Number(sumMoney(amounts)) : null)));
    out.push({ currency, x, ys, bands: bs, totals: totals.map((t) => sumMoney(t) ?? "0") });
  }
  return out;
}

export interface BreakdownRow {
  band: Band;
  currency: string;
  /** The band's compute and external parts; null when it has none (not zero). */
  compute: string | null;
  external: string | null;
  /** What show counts. */
  amount: string;
  /** Of what show counts in this currency. */
  share: number | null;
  /** Runs with a shown cost in the band; null while unknown. */
  runs: number | null;
}

/** The table under a breakdown: each band per currency, in band order, with its Compute and External parts, its share and its Runs. */
export function breakdownRows(rows: CostSummaryRow[], dim: string, show: CostShow, bands: Map<string, Band[]>, runs?: Map<string, Set<string>>): BreakdownRow[] {
  const totals = new Map(shownTotals(rows, show).map((t) => [t.currency, t.amount]));
  const out: BreakdownRow[] = [];
  for (const [currency, rs] of byCurrency(rows)) {
    for (const band of bands.get(currency) ?? []) {
      const mine = rs.filter((r) => band.values.includes(r.group?.[dim] ?? NONE));
      const part = (compute: boolean) => sum(mine.filter((r) => (familyOf(r) === COMPUTE) === compute).map((r) => r.amount));
      const amount = sum(mine.filter((r) => shows(show, familyOf(r))).map((r) => r.amount));
      if (amount == null) continue;
      const ids = runs ? new Set(band.values.flatMap((v) => [...(runs.get(v) ?? [])])) : null;
      out.push({ band, currency, compute: part(true), external: part(false), amount, share: ratio(amount, totals.get(currency)), runs: ids ? ids.size : null });
    }
  }
  return out;
}

/**
 * The Runs under each value: rows grouped by `dim` and run, kept to the
 * Runs with a shown cost (`shownRuns`, from rows grouped by run and family).
 * Exact where a Run has one value (a label, its submitter, its tenant); a
 * Run whose cost spans pools counts in each.
 */
export function runsByValue(dimRuns: CostSummaryRow[], dim: string, shownRuns: Set<string>): Map<string, Set<string>> {
  const out = new Map<string, Set<string>>();
  for (const r of dimRuns) {
    const run = r.group?.run;
    if (!run || !shownRuns.has(run)) continue;
    const v = r.group?.[dim] ?? NONE;
    out.set(v, (out.get(v) ?? new Set()).add(run));
  }
  return out;
}

/** The Runs with a cost show counts; rows grouped by run and family. */
export function shownRunIds(rows: CostSummaryRow[], show: CostShow): Set<string> {
  return new Set(rows.filter((r) => r.group?.run && shows(show, familyOf(r))).map((r) => r.group!.run!));
}

/** Per currency, the band that cost the most in the peak bucket, and its share of that bucket. Series rows grouped by `dim` and family. */
export function peakBands(ps: Peak[], d: CostSummary | undefined, dim: string, show: CostShow, bands: Map<string, Band[]>): Map<string, { band: Band; share: number | null }> {
  const out = new Map<string, { band: Band; share: number | null }>();
  for (const p of ps) {
    const bs = bands.get(p.currency) ?? [];
    const rows = (d?.series ?? []).filter((r) => r.currency === p.currency && r.at && Math.floor(Date.parse(r.at) / 1000) === p.at && shows(show, familyOf(r)));
    const per = bs.map((b) => sum(rows.filter((r) => b.values.includes(r.group?.[dim] ?? NONE)).map((r) => r.amount)));
    let best = -1;
    per.forEach((a, i) => {
      if (a != null && (best < 0 || compareMoney(a, per[best]!) > 0)) best = i;
    });
    if (best >= 0) out.set(p.currency, { band: bs[best]!, share: ratio(per[best], p.amount) });
  }
  return out;
}

/** How a submitter (a group=key value) reads: the key's name, a person's email, "Operator key" for an operator's key a tenant may not name, "Before key tracking" for NONE. */
export function keyLabel(value: string, keys: Map<string, CostKey>): string {
  if (value === NONE) return "Before key tracking";
  const k = keys.get(value);
  if (k?.email) return k.email;
  if (k?.name) return k.name;
  if (k?.operator) return "Operator key";
  return value.startsWith("email:") ? value.slice("email:".length) : value;
}

/** How a band reads in a legend, a KPI or a table row. */
export function bandLabel(band: Band, b: Breakdown, names: { keys?: Map<string, CostKey>; tenants?: Map<string, string> } = {}): string {
  if (band.id === OTHER) return `Other (${band.values.length})`;
  switch (b.kind) {
    case "label":
      return band.id === NONE ? `(no ${b.key} label)` : band.id;
    case "key":
      return keyLabel(band.id, names.keys ?? new Map());
    case "pool":
      return band.id === NONE ? "(no pool)" : band.id;
    case "tenant":
      return names.tenants?.get(band.id) ?? band.id;
    default:
      return band.id;
  }
}

/** A filter for a band of a label breakdown: its value, or the label not set; null where filtering by it is not a label filter. */
export function bandFilter(band: Band, b: Breakdown): LabelFilter | null {
  if (b.kind !== "label" || band.id === OTHER) return null;
  return band.id === NONE ? { key: b.key, values: [], notSet: true } : { key: b.key, values: [band.id] };
}

/** Filters with one more: a value joins its key's filter (OR); "not set" replaces the key's values, and a value replaces "not set". */
export function addFilter(fs: LabelFilter[], f: LabelFilter): LabelFilter[] {
  const others = fs.filter((x) => x.key !== f.key);
  const same = fs.find((x) => x.key === f.key && !x.notSet);
  if (f.notSet || !same) return [...others, f];
  return [...others, { key: f.key, values: [...new Set([...same.values, ...f.values])] }];
}

/** The label filters as luxd's /v1/costs parameters. */
export function filterQuery(fs: LabelFilter[]): { label?: string[]; nolabel?: string[] } {
  const p = filtersParams(fs);
  return { ...(p.label.length ? { label: p.label } : {}), ...(p.nolabel.length ? { nolabel: p.nolabel } : {}) };
}

/** A filter in words, for its chip: "app = jervasion", "repo ∈ a, b", "phase is not set". */
export function filterText(f: LabelFilter): { key: string; op: string; values: string } {
  if (f.notSet) return { key: f.key, op: "is", values: "not set" };
  return { key: f.key, op: f.values.length > 1 ? "∈" : "=", values: f.values.join(", ") };
}
