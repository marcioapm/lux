import { expect, test } from "bun:test";
import type { CostSummary, CostSummaryRow } from "../../api/index.ts";
import { changes, costSince, familyCharts, familyRows, peakRuns, peaks, peakWindows, perChoices, previousWindow, resolvePer, shownTotals, sideTotals, topSplit } from "./costView.ts";

const fam = (family: string, currency: string, amount: string, at?: string): CostSummaryRow => ({ group: { family }, currency, amount, ...(at ? { at } : {}) });
const runFam = (run: string, family: string, currency: string, amount: string): CostSummaryRow => ({ group: { run, family }, currency, amount });

test("granularity: 1h reads 6h; Auto is hourly up to 24h and daily from 7d", () => {
  expect(costSince("1h")).toBe("6h");
  expect(costSince("7d")).toBe("7d");
  for (const [since, interval] of [["6h", "hour"], ["24h", "hour"], ["7d", "day"], ["30d", "day"]] as const) {
    expect([since, resolvePer(since, "auto").interval]).toEqual([since, interval]);
  }
});

test("granularity: a choice under 3 or over 200 buckets is off, with its reason; 168 hourly bars are allowed", () => {
  const off = (since: "6h" | "24h" | "7d" | "30d") => Object.fromEntries(perChoices(since).map((c) => [c.value, c.disabled ?? null]));
  expect(off("6h")).toEqual({ auto: null, hour: null, day: "1 bar · too coarse" });
  expect(off("24h")).toEqual({ auto: null, hour: null, day: "1 bar · too coarse" });
  expect(off("7d")).toEqual({ auto: null, hour: null, day: null });
  expect(off("30d")).toEqual({ auto: null, hour: "720 bars · too many", day: null });
  expect(perChoices("7d").find((c) => c.value === "hour")!.buckets).toBe(168);
});

test("granularity: the URL asking for a choice that is off falls back to Auto", () => {
  expect(resolvePer("24h", "day")).toMatchObject({ value: "auto", interval: "hour" });
  expect(resolvePer("30d", "hour")).toMatchObject({ value: "auto", interval: "day" });
  expect(resolvePer("7d", "hour")).toMatchObject({ value: "hour", interval: "hour" });
  expect(resolvePer("6h", "hour")).toMatchObject({ value: "hour", interval: "hour" });
});

const ROWS = [fam("compute", "USD", "3.87"), fam("ai", "USD", "55.08"), fam("video", "USD", "1.05"), fam("compute", "EUR", "2"), fam("ai", "EUR", "0.5")];

test("split: Compute is the compute family, External every other, per currency and exact", () => {
  expect(shownTotals(ROWS, "all")).toEqual([
    { currency: "EUR", amount: "2.5" },
    { currency: "USD", amount: "60" },
  ]);
  expect(shownTotals(ROWS, "compute")).toEqual([
    { currency: "EUR", amount: "2" },
    { currency: "USD", amount: "3.87" },
  ]);
  expect(shownTotals(ROWS, "external")).toEqual([
    { currency: "EUR", amount: "0.5" },
    { currency: "USD", amount: "56.13" },
  ]);
  expect(sideTotals(ROWS)).toEqual([
    { currency: "EUR", all: "2.5", compute: "2", external: "0.5" },
    { currency: "USD", all: "60", compute: "3.87", external: "56.13" },
  ]);
});

test("split: a side with no cost is no figure, not zero; a currency with no shown cost is absent", () => {
  const rows = [fam("ai", "USD", "1"), fam("compute", "EUR", "2")];
  expect(sideTotals(rows)).toEqual([
    { currency: "EUR", all: "2", compute: "2", external: null },
    { currency: "USD", all: "1", compute: null, external: "1" },
  ]);
  expect(shownTotals(rows, "compute")).toEqual([{ currency: "EUR", amount: "2" }]);
});

test("family rows: only the shown side, largest first per currency, share of the shown total", () => {
  expect(familyRows(ROWS, "external").map((r) => [r.currency, r.family, r.amount, r.share?.toFixed(3)])).toEqual([
    ["EUR", "ai", "0.5", "1.000"],
    ["USD", "ai", "55.08", "0.981"],
    ["USD", "video", "1.05", "0.019"],
  ]);
  expect(familyRows(ROWS, "all").filter((r) => r.currency === "USD").map((r) => r.family)).toEqual(["ai", "compute", "video"]);
});

const T = (h: number) => new Date(Date.UTC(2026, 9, 9, h)).toISOString();
const SUMMARY: CostSummary = {
  from: T(0),
  to: T(4),
  basis: "list",
  totals: [],
  series: [fam("compute", "USD", "1", T(0)), fam("ai", "USD", "2", T(0)), fam("compute", "USD", "1.5", T(1)), fam("ai", "USD", "40", T(3)), fam("ai", "EUR", "9", T(1)), fam("compute", "EUR", "3", T(2))],
  families: [{ family: "compute", displayName: "Compute" }, { family: "ai", displayName: "AI models", color: "violet" }],
};

test("charts: one per currency, a series per shown family, a bucket with no row a gap", () => {
  const all = familyCharts(SUMMARY, "hour", "all");
  expect(all.map((c) => c.currency)).toEqual(["EUR", "USD"]);
  const usd = all.find((c) => c.currency === "USD")!;
  expect(usd.x.length).toBe(4);
  expect(usd.series.map((s) => s.label)).toEqual(["Compute", "AI models"]);
  expect(usd.ys).toEqual([
    [1, 1.5, null, null],
    [2, null, null, 40],
  ]);
  expect(usd.totals).toEqual(["2.5", "42"]);
  const compute = familyCharts(SUMMARY, "hour", "compute");
  expect(compute.find((c) => c.currency === "USD")!.series.map((s) => s.label)).toEqual(["Compute"]);
  expect(compute.find((c) => c.currency === "USD")!.ys).toEqual([[1, 1.5, null, null]]);
  // External hides compute's figures: none of its values is in the chart.
  const ext = familyCharts(SUMMARY, "hour", "external").find((c) => c.currency === "EUR")!;
  expect(ext.ys).toEqual([[null, 9, null, null]]);
});

test("Top Runs: ranked by what Show counts, per currency, with every family's part kept for the split", () => {
  const rows = [runFam("r1", "compute", "USD", "10"), runFam("r1", "ai", "USD", "1"), runFam("r2", "ai", "USD", "5"), runFam("r2", "compute", "USD", "0.5"), runFam("r3", "ai", "EUR", "100")];
  expect(topSplit(rows, "run", "all").map((r) => [r.currency, r.key, r.amount, r.all])).toEqual([
    ["EUR", "r3", "100", "100"],
    ["USD", "r1", "11", "11"],
    ["USD", "r2", "5.5", "5.5"],
  ]);
  const ext = topSplit(rows, "run", "external");
  expect(ext.map((r) => [r.currency, r.key, r.amount])).toEqual([
    ["EUR", "r3", "100"],
    ["USD", "r2", "5"],
    ["USD", "r1", "1"],
  ]);
  expect(ext.find((r) => r.key === "r1")!.parts).toEqual([
    { family: "compute", amount: "10" },
    { family: "ai", amount: "1" },
  ]);
  // A Run with nothing on the shown side is not listed (not a $0 row).
  expect(topSplit(rows, "run", "compute").map((r) => r.key)).toEqual(["r1", "r2"]);
  expect(topSplit(rows, "run", "all", 1).map((r) => r.key)).toEqual(["r3", "r1"]);
});

test("previous window: the same length, just before", () => {
  expect(previousWindow({ from: T(4), to: T(8) })).toEqual({ from: T(0), to: T(4) });
});

test("change vs previous: per currency; no earlier figure, or a zero or refund one, has none", () => {
  expect(changes([{ currency: "USD", amount: "13.8" }, { currency: "EUR", amount: "1" }, { currency: "GBP", amount: "1" }], [{ currency: "USD", amount: "10" }, { currency: "GBP", amount: "0" }]).map((c) => [c.currency, c.ratio == null ? null : Number(c.ratio.toFixed(3))])).toEqual([
    ["USD", 0.38],
    ["EUR", null],
    ["GBP", null],
  ]);
  expect(changes([{ currency: "USD", amount: "5" }], [{ currency: "USD", amount: "10" }])[0]!.ratio).toBe(-0.5);
});

test("peak: per currency, the largest bucket of what Show counts; hidden families do not lift it", () => {
  const all = peaks(SUMMARY, "all");
  expect(all.map((p) => [p.currency, new Date(p.at * 1000).toISOString(), p.amount])).toEqual([
    ["EUR", T(1), "9"],
    ["USD", T(3), "40"],
  ]);
  const compute = peaks(SUMMARY, "compute");
  expect(compute.map((p) => [p.currency, new Date(p.at * 1000).toISOString(), p.amount])).toEqual([
    ["EUR", T(2), "3"],
    ["USD", T(1), "1.5"],
  ]);
  expect(peakWindows(all, "hour")).toEqual([
    { from: T(1), to: T(2) },
    { from: T(3), to: T(4) },
  ]);
  expect(peakWindows([{ currency: "USD", at: Date.parse(T(0)) / 1000, amount: "1" }, { currency: "EUR", at: Date.parse(T(0)) / 1000, amount: "1" }], "day")).toEqual([{ from: T(0), to: T(24) }]);
});

test("peak Run: the costliest Run of that bucket, as Show counts, per currency", () => {
  const at = Date.parse(T(3)) / 1000;
  const ps = [{ currency: "USD", at, amount: "40" }];
  const rows = [runFam("big-compute", "compute", "USD", "30"), runFam("big-ai", "ai", "USD", "20"), runFam("eur", "ai", "EUR", "99")];
  expect(peakRuns(ps, [{ at, rows }], "all").get("USD")).toBe("big-compute");
  expect(peakRuns(ps, [{ at, rows }], "external").get("USD")).toBe("big-ai");
  expect(peakRuns(ps, [{ at: at + 3600, rows }], "all").size).toBe(0);
});
