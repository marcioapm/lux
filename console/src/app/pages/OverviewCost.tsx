import { useMemo, type ReactNode } from "react";
import { Badge, Card, CENTS, ColorKey, EmptyState, familyColor, FamilyKey, formatMoney, ListPriceNote, Money, MoneyList, rangeText, SectionHeader, SegmentedControl, Select, Skeleton, sumMoney, Table, TimeSeriesChart, type Column, type SelectOption } from "@lux/design-system";
import { api, type CostSummaryRow, type MoneyTotal } from "../../api/index.ts";
import { go, Link } from "../router.tsx";
import { useScope, useScopedQuery } from "../scope.tsx";
import { ErrorStrip, RunLink, RunNameLink, runPath } from "./common.tsx";
import {
  changes,
  COMPUTE,
  costSince,
  familyCharts,
  familyMeta,
  familyRows,
  intervalWord,
  peakRuns,
  peaks,
  peakWindows,
  perChoices,
  previousWindow,
  ratio,
  resolvePer,
  shownTotals,
  sideTotals,
  topSplit,
  type CostInterval,
  type CostPer,
  type CostShow,
  type FamilyRow,
  type Peak,
  type SideTotals,
  type SplitRow,
} from "./costView.ts";

const POLL = 60_000;
/** External is every family but compute: one colour for the group, apart from compute's. */
const EXTERNAL_COLOR = "var(--chart-7)";
const COMPUTE_COLOR = familyColor(COMPUTE);
const SHOW_WORD: Record<CostShow, string> = { all: "total", compute: "compute", external: "external" };

/**
 * Cost over the page's range, in one panel: KPIs, cost per bucket as
 * stacked bars by family (one chart per currency), the top Runs with their
 * Compute/External split, cost by family and, for an operator viewing all
 * tenants, the unallocated host time and the top tenants. Show (?cost=)
 * counts all, compute or external costs; Per (?per=) picks the bucket.
 * Costs are hourly: the 1h range reads 6h.
 */
export function OverviewCost() {
  const scope = useScope();
  const show = scope.costShow;
  const since = costSince(scope.range);
  const per = resolvePer(since, scope.costPer);
  const interval = per.interval;

  const byFamily = useScopedQuery(`costs:family:${since}:${interval}`, (t, s) => api.costs(t, { group: ["family"], interval, since }, s), { interval: POLL });
  const byRun = useScopedQuery(`costs:run-family:${since}`, (t, s) => api.costs(t, { group: ["run", "family"], since }, s), { interval: POLL });
  const byTenant = useScopedQuery(`costs:tenant-family:${since}`, (t, s) => api.costs(t, { group: ["tenant", "family"], since }, s), { interval: POLL, enabled: scope.showTenant });
  const tenantNames = useScopedQuery("tenants-names", (_t, s) => api.tenants(s), { enabled: scope.showTenant });
  // The window of equal length before this one, from the summary's own effective bounds.
  const prev = byFamily.data ? previousWindow(byFamily.data) : null;
  const before = useScopedQuery(`costs:family:prev:${prev?.from}:${prev?.to}`, (t, s) => api.costs(t, { group: ["family"], from: prev!.from, to: prev!.to }, s), { enabled: prev != null });
  // The peak bucket's Runs: one small summary per distinct peak bucket (one per currency at most), once the peak is known.
  const peakList = useMemo(() => peaks(byFamily.data, show), [byFamily.data, show]);
  const windows = useMemo(() => peakWindows(peakList, interval), [peakList, interval]);
  const peakKey = windows.map((w) => w.from).join(",");
  const peakRows = useScopedQuery(
    `costs:peak-runs:${interval}:${peakKey}`,
    (t, s) => Promise.all(windows.map((w) => api.costs(t, { group: ["run", "family"], from: w.from, to: w.to }, s).then((d) => ({ at: Date.parse(w.from) / 1000, d })))),
    { interval: POLL, enabled: windows.length > 0 },
  );

  const all = byFamily.data?.totals ?? [];
  const shown = useMemo(() => shownTotals(all, show), [all, show]);
  const sides = useMemo(() => sideTotals(all), [all]);
  const change = useMemo(() => changes(shown, before.data ? shownTotals(before.data.totals, show) : []), [shown, before.data, show]);
  const charts = useMemo(() => familyCharts(byFamily.data, interval, show), [byFamily.data, interval, show]);
  const runs = useMemo(() => topSplit(byRun.data?.totals ?? [], "run", show), [byRun.data, show]);
  const tenants = useMemo(() => topSplit(byTenant.data?.totals ?? [], "tenant", show), [byTenant.data, show]);
  const families = useMemo(() => familyRows(all, show), [all, show]);
  const meta = useMemo(() => familyMeta(byFamily.data?.families), [byFamily.data]);
  const tenantName = useMemo(() => new Map((tenantNames.data ?? []).map((t) => [t.id, t.name])), [tenantNames.data]);
  // Names come with the summary grouped by run: one call, not one per row.
  const runNames = useMemo(() => {
    const m = new Map<string, string>();
    for (const d of [byRun.data, ...(peakRows.data ?? []).map((w) => w.d)]) for (const r of d?.runs ?? []) m.set(r.id, r.name ?? "");
    return m;
  }, [byRun.data, peakRows.data]);
  const peakRun = useMemo(() => peakRuns(peakList, (peakRows.data ?? []).map((w) => ({ at: w.at, rows: w.d.totals })), show), [peakList, peakRows.data, show]);

  const error = byFamily.error ?? byRun.error ?? byTenant.error ?? before.error ?? peakRows.error;
  const words = rangeText(since);
  const loading = byFamily.loading;
  const empty = !loading && all.length === 0;

  const showOptions = [
    { value: "all" as const, label: "All" },
    { value: "compute" as const, label: <ColorKey color={COMPUTE_COLOR}>Compute</ColorKey>, title: "Host time Runs reserved (the compute family)" },
    { value: "external" as const, label: <ColorKey color={EXTERNAL_COLOR}>External</ColorKey>, title: "Every other family: what cost plugins report (AI models and others)" },
  ];
  const perOptions: SelectOption<CostPer>[] = perChoices(since).map((c) => ({
    value: c.value,
    text: c.value === "auto" ? `Auto · ${intervalWord(c.interval)}` : c.value === "hour" ? "Hourly" : "Daily",
    label: (
      <span className="cost-per-opt">
        <span>{c.value === "auto" ? "Auto" : c.value === "hour" ? "Hourly" : "Daily"}</span>
        {!c.disabled && <span className="muted">{c.value === "auto" ? `${intervalWord(c.interval)} for ${since}` : `${c.buckets} bars`}</span>}
      </span>
    ),
    disabled: c.disabled,
  }));

  return (
    <div className="stack cost-panel">
      <SectionHeader
        title="Cost"
        note={
          <span className="row">
            <ListPriceNote />
            {scope.range === "1h" && <span>the last hour reads 6h: costs are hourly buckets</span>}
          </span>
        }
        actions={
          <div className="cost-controls">
            <span className="cost-control">
              <span className="cost-control-label" aria-hidden="true">
                Show
              </span>
              <SegmentedControl label="Show" options={showOptions} value={show} onChange={scope.setCostShow} />
            </span>
            <Select options={perOptions} value={per.value} onChange={scope.setCostPer} prefix="Per" size="sm" />
          </div>
        }
      />
      <ErrorStrip error={error} />
      <Card className="cost-card" flush>
        <div className="cost-kpis">
          <TotalKpi show={show} words={words} since={since} loading={loading} shown={shown} sides={sides} change={change} />
          <SideKpi side="compute" show={show} loading={loading} sides={sides} unit="host time Runs reserved" />
          <SideKpi side="external" show={show} loading={loading} sides={sides} unit="reported by cost plugins" />
          <PeakKpi interval={interval} show={show} loading={loading} peaks={peakList} runs={peakRun} names={runNames} />
        </div>
        <div className="cost-charts">
          {charts.length === 0 ? (
            <EmptyState compact title={loading ? "Loading…" : empty ? "No cost recorded in this range" : `No ${SHOW_WORD[show]} cost in this range`} description={loading ? undefined : "Nothing has been costed yet in this range; no figure is not a zero."} />
          ) : (
            charts.map((c) => (
              <div className="cost-chart" key={c.currency}>
                {charts.length > 1 && <div className="cost-chart-currency">{c.currency}</div>}
                <TimeSeriesChart
                  x={c.x}
                  ys={c.ys}
                  series={c.series}
                  unit="money"
                  currency={c.currency}
                  stacked
                  bars
                  legend
                  legendValues={c.totals.map((t) => formatMoney(t, c.currency, { decimals: CENTS }))}
                  legendNote={`each bar is one ${interval === "hour" ? "hour" : "UTC day"} · no bar: nothing recorded, not $0`}
                />
              </div>
            ))
          )}
        </div>
      </Card>
      <div className="cost-lower">
        <Card title="Top Runs" subtitle={`by ${SHOW_WORD[show]} cost · ${words}`} actions={<Link to="/runs">All Runs →</Link>} flush>
          <SplitTable rows={runs} loading={byRun.loading} show={show} lead="Run" name={(r) => <RunNameLink id={r.key} name={runNames.get(r.key)} />} sub={(r) => (runNames.get(r.key) ? <RunLink id={r.key} /> : null)} onClick={(r) => go(runPath(r.key))} empty={`No Run has a ${SHOW_WORD[show] === "total" ? "" : SHOW_WORD[show] + " "}cost in this range.`} />
        </Card>
        <div className="cost-side">
          <Card title="By family" subtitle={words} flush footer={scope.showTenant && show !== "external" ? <Unallocated amounts={byFamily.data?.unallocated} loading={loading} /> : undefined}>
            <FamilyTable rows={families} meta={meta} loading={loading} />
          </Card>
          {scope.showTenant && (
            <Card title="Top tenants" subtitle={`by ${SHOW_WORD[show]} cost · ${words}`} flush>
              <SplitTable rows={tenants} loading={byTenant.loading} show={show} lead="Tenant" name={(r) => tenantName.get(r.key) ?? r.key} empty="No tenant has a cost in this range." compact />
            </Card>
          )}
        </div>
      </div>
    </div>
  );
}

function Kpi({ label, value, sub, loading, muted, children }: { label: ReactNode; value: ReactNode; sub?: ReactNode; loading: boolean; muted?: boolean; children?: ReactNode }) {
  return (
    <div className={muted ? "cost-kpi is-muted" : "cost-kpi"}>
      <div className="cost-kpi-label">{label}</div>
      <div className="cost-kpi-value">{loading ? <Skeleton width={96} height={24} /> : value}</div>
      {!loading && sub && <div className="cost-kpi-sub">{sub}</div>}
      {!loading && children}
    </div>
  );
}

const pct = (r: number | null) => (r == null ? null : `${Math.round(r * 100)}%`);

function TotalKpi({ show, words, since, loading, shown, sides, change }: { show: CostShow; words: string; since: string; loading: boolean; shown: MoneyTotal[]; sides: SideTotals[]; change: ReturnType<typeof changes> }) {
  const label = show === "all" ? "Total" : show === "compute" ? "Compute total" : "External total";
  const one = shown.length === 1;
  const lines = change.filter((c) => c.ratio != null);
  return (
    <Kpi label={`${label} · ${words}`} loading={loading} value={<MoneyList amounts={shown} large decimals={CENTS} />}>
      {lines.length > 0 && (
        <div className="cost-kpi-sub" data-cost-change>
          {lines.map((c) => (
            <span key={c.currency} className="cost-change">
              {!one && `${c.currency} `}
              {c.ratio! > 0 ? "▲" : c.ratio! < 0 ? "▼" : "="} {Math.abs(Math.round(c.ratio! * 100))}%
            </span>
          ))}{" "}
          vs previous {since}
        </div>
      )}
      {show !== "all" && (
        <div className="cost-kpi-sub">
          of <MoneyList amounts={sides.flatMap((s) => (s.all == null ? [] : [{ currency: s.currency, amount: s.all }]))} decimals={CENTS} /> all costs
        </div>
      )}
      {sides.map((s) => (
        <SplitBar key={s.currency} compute={s.compute} external={s.external} whole={s.all} currency={s.currency} show={show} label={sides.length > 1 ? s.currency : undefined} />
      ))}
    </Kpi>
  );
}

function SideKpi({ side, show, loading, sides, unit }: { side: "compute" | "external"; show: CostShow; loading: boolean; sides: SideTotals[]; unit: string }) {
  const amounts = sides.flatMap((s) => (s[side] == null ? [] : [{ currency: s.currency, amount: s[side]! }]));
  const hidden = show !== "all" && show !== side;
  const share = sides.length === 1 ? pct(ratio(sides[0]![side], sides[0]!.all)) : null;
  return (
    <Kpi
      label={<ColorKey color={side === "compute" ? COMPUTE_COLOR : EXTERNAL_COLOR}>{side === "compute" ? "Compute" : "External"}</ColorKey>}
      loading={loading}
      muted={hidden}
      value={<MoneyList amounts={amounts} large decimals={CENTS} />}
      sub={[hidden ? "hidden" : null, share, hidden ? null : unit].filter(Boolean).join(" · ")}
    />
  );
}

function peakWhen(at: number, interval: CostInterval): string {
  const d = new Date(at * 1000);
  if (interval === "day") return d.toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric", timeZone: "UTC" });
  const p = (x: number) => String(x).padStart(2, "0");
  const today = new Date();
  const yesterday = new Date(today.getFullYear(), today.getMonth(), today.getDate() - 1);
  const same = (a: Date, b: Date) => a.toDateString() === b.toDateString();
  const day = same(d, today) ? "today" : same(d, yesterday) ? "yesterday" : d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
  return `${p(d.getHours())}:${p(d.getMinutes())} ${day}`;
}

function PeakKpi({ interval, show, loading, peaks: ps, runs, names }: { interval: CostInterval; show: CostShow; loading: boolean; peaks: Peak[]; runs: Map<string, string>; names: Map<string, string> }) {
  return (
    <Kpi label={interval === "hour" ? "Peak hour" : "Peak day"} loading={loading} value={<MoneyList amounts={ps.map((p) => ({ currency: p.currency, amount: p.amount }))} large decimals={CENTS} />}>
      {ps.map((p) => {
        const run = runs.get(p.currency);
        return (
          <div className="cost-kpi-sub cost-peak" key={p.currency} data-cost-peak={p.currency}>
            {ps.length > 1 && `${p.currency} · `}
            {peakWhen(p.at, interval)}
            {run && (
              <>
                {" · "}
                <RunNameLink id={run} name={names.get(run)} />
              </>
            )}
          </div>
        );
      })}
      {ps.length === 0 && show !== "all" && <div className="cost-kpi-sub">none in this range</div>}
    </Kpi>
  );
}

/** Compute and External as one bar against `whole`; the side Show hides is drawn faint, never dropped. */
function SplitBar({ compute, external, whole, currency, show, label, scale = 1 }: { compute: string | null; external: string | null; whole: string | null; currency: string; show: CostShow; label?: string; scale?: number }) {
  const c = Math.max(0, ratio(compute, whole) ?? 0);
  const e = Math.max(0, ratio(external, whole) ?? 0);
  const sum = c + e || 1;
  const title = [compute != null ? `Compute ${formatMoney(compute, currency)}` : null, external != null ? `External ${formatMoney(external, currency)}` : null].filter(Boolean).join(" · ");
  return (
    <div className="cost-split" title={title || undefined} data-cost-split>
      {label && <span className="cost-split-label">{label}</span>}
      <span className="cost-split-track" style={{ width: `${Math.max(2, scale * 100)}%` }}>
        {c > 0 && <span className={show === "external" ? "cost-split-part is-faint" : "cost-split-part"} style={{ flexGrow: c / sum, background: COMPUTE_COLOR }} />}
        {e > 0 && <span className={show === "compute" ? "cost-split-part is-faint" : "cost-split-part"} style={{ flexGrow: e / sum, background: EXTERNAL_COLOR }} />}
      </span>
    </div>
  );
}

const partOf = (r: SplitRow, side: "compute" | "external") => {
  const parts = r.parts.filter((p) => (p.family === COMPUTE) === (side === "compute")).map((p) => p.amount);
  return parts.length ? sumMoney(parts) : null;
};

function SplitTable({ rows, loading, show, lead, name, sub, onClick, empty, compact }: { rows: SplitRow[]; loading: boolean; show: CostShow; lead: string; name: (r: SplitRow) => ReactNode; sub?: (r: SplitRow) => ReactNode; onClick?: (r: SplitRow) => void; empty: string; compact?: boolean }) {
  // Bars are as long as a row's whole cost against the largest in its currency: a ratio for drawing, never a sum across currencies.
  const max = new Map<string, number>();
  for (const r of rows) max.set(r.currency, Math.max(max.get(r.currency) ?? 0, Number(r.all)));
  const cols: Column<SplitRow>[] = [
    {
      key: "name",
      header: lead,
      lead: true,
      cell: (r) => (
        <span className="cost-name">
          <span className="ellipsis">{name(r)}</span>
          {sub && <span className="cost-name-sub">{sub(r)}</span>}
        </span>
      ),
    },
    ...(compact ? [] : [{ key: "split", header: "Split", width: "30%", cell: (r: SplitRow) => <SplitBar compute={partOf(r, "compute")} external={partOf(r, "external")} whole={r.all} currency={r.currency} show={show} scale={Number(r.all) / (max.get(r.currency) || 1)} /> }]),
    // Ordering only: the figure is formatted from the string.
    { key: "amount", header: "Cost", cell: (r) => <Money amount={r.amount} currency={r.currency} decimals={CENTS} />, align: "right", mono: true, width: 110 },
  ];
  return <Table columns={cols} rows={rows} rowKey={(r) => `${r.currency}:${r.key}`} loading={loading} loadingRows={3} onRowClick={onClick} empty={empty} dense={compact} />;
}

function FamilyTable({ rows, meta, loading }: { rows: FamilyRow[]; meta: ReturnType<typeof familyMeta>; loading: boolean }) {
  const multi = new Set(rows.map((r) => r.currency)).size > 1;
  const cols: Column<FamilyRow>[] = [
    { key: "family", header: "Family", lead: true, cell: (r) => <FamilyKey family={r.family} displayName={meta.get(r.family)?.displayName ?? (r.family === COMPUTE ? "Compute" : undefined)} color={meta.get(r.family)?.color} /> },
    { key: "kind", header: "Kind", width: 96, cell: (r) => <Badge tone="neutral">{r.family === COMPUTE ? "Compute" : "External"}</Badge> },
    { key: "amount", header: "Cost", align: "right", mono: true, width: 104, cell: (r) => <Money amount={r.amount} currency={r.currency} decimals={CENTS} /> },
    { key: "share", header: multi ? "Share (per currency)" : "Share", align: "right", mono: true, width: multi ? 92 : 64, cell: (r) => pct(r.share) ?? "–" },
  ];
  return <Table columns={cols} rows={rows} rowKey={(r) => `${r.currency}:${r.family}`} loading={loading} loadingRows={2} empty="No family has a cost in this range." dense />;
}

function Unallocated({ amounts, loading }: { amounts: CostSummaryRow[] | undefined; loading: boolean }) {
  return (
    <div className="cost-unallocated" title="Host time no Run reserved: apart from the Runs' cost, never added to it">
      <span>Unallocated host time</span>
      {loading ? <Skeleton width={56} height={14} /> : <MoneyList amounts={amounts} decimals={CENTS} />}
    </div>
  );
}
