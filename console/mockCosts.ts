// GET /v1/costs for the mock: hourly cost rows for the last 30 days, of a
// few Runs across two tenants, in two families (compute, and AI models from
// a plugin), with a quiet night with no rows (gaps, not zeros) and an AI
// spike two hours ago. MOCK_COST_CURRENCIES=USD,EUR adds a second currency.
// Amounts are integers of micro-units, so every sum is exact.

interface Row {
  hour: number;
  tenant: string;
  run: string;
  family: string;
  currency: string;
  micros: number;
}

const RUNS = [
  { id: "run_a4aao3fvcuzpa2k", name: "jervasion-abs-pr-5336-review", tenant: "ten_acme", ai: 9, compute: 1 },
  { id: "run_jd2inqktcsax7m1", name: "jervasion-jervasion-pr-126-review", tenant: "ten_acme", ai: 4, compute: 1 },
  { id: "run_6y52awww5ede2qf", name: "jervasion-sdk-plugins-pr-12-review", tenant: "ten_globex", ai: 4, compute: 2 },
  { id: "run_qlie6ofisqmbu9d", name: "jervasion-abs-pr-5334-review", tenant: "ten_acme", ai: 3, compute: 1 },
  { id: "run_lsdhtq2lotpvi3s", name: "jervasion-abs-pr-4982-review", tenant: "ten_globex", ai: 3, compute: 1 },
  { id: "run_6a62z4nsfa3jsx0", name: "jervasion-jervasion-pr-127-review", tenant: "ten_acme", ai: 2, compute: 1 },
  { id: "run_bsn6jhmfcejni8a", name: "fix wi_0mun9t: retry lease on host loss", tenant: "ten_acme", ai: 0, compute: 3 },
  { id: "run_5m4ji5pcevbiwq2", name: "", tenant: "ten_globex", ai: 1, compute: 1 },
];

/** A deterministic 0..1 from a seed: the same data on every reload. */
function noise(seed: number): number {
  const x = Math.sin(seed * 12.9898) * 43758.5453;
  return x - Math.floor(x);
}

const HOUR = 3600;
const currencies = (process.env.MOCK_COST_CURRENCIES ?? "USD").split(",").filter(Boolean);

function generate(): { rows: Row[]; unallocated: Map<number, Map<string, number>> } {
  const nowHour = Math.floor(Date.now() / 1000 / HOUR) * HOUR;
  const rows: Row[] = [];
  const unallocated = new Map<number, Map<string, number>>();
  for (let h = 30 * 24; h >= 0; h--) {
    const hour = nowHour - h * HOUR;
    const local = new Date(hour * 1000).getHours();
    // A quiet night: nothing recorded at all from 01:00 to 04:00.
    if (local >= 1 && local < 4) continue;
    currencies.forEach((currency, ci) => {
      const scale = ci === 0 ? 1 : 0.3;
      RUNS.forEach((r, ri) => {
        const seed = hour / HOUR + ri * 101 + ci * 7;
        // Each Run is active a share of the hours.
        if (noise(seed) > 0.45) return;
        const compute = Math.round((20_000 + noise(seed + 1) * 60_000) * r.compute * scale);
        rows.push({ hour, tenant: r.tenant, run: r.id, family: "compute", currency, micros: compute });
        if (r.ai > 0) {
          let ai = Math.round((40_000 + noise(seed + 2) * 400_000) * r.ai * scale);
          if (h === 2 && ri === 0) ai = Math.round(34_812_345 * scale);
          if (h === 15 && ri === 1) ai = Math.round(4_620_000 * scale);
          rows.push({ hour, tenant: r.tenant, run: r.id, family: "ai", currency, micros: ai });
        }
      });
      const idle = new Map(unallocated.get(hour) ?? []);
      idle.set(currency, Math.round((30_000 + noise(hour / HOUR + ci) * 70_000) * scale));
      unallocated.set(hour, idle);
    });
  }
  return { rows, unallocated };
}

const money = (micros: number) => {
  const neg = micros < 0;
  const s = String(Math.abs(micros)).padStart(7, "0");
  return `${neg ? "-" : ""}${s.slice(0, -6)}.${s.slice(-6)}`.replace(/\.?0+$/, "");
};

const SINCE: Record<string, number> = { "1h": 1, "6h": 6, "24h": 24, "7d": 168, "30d": 720 };

/** The summary for a request's range, groups and interval, as luxd answers it. */
export function costSummary(u: URL): unknown {
  const q = u.searchParams;
  const ceilHour = (t: number) => Math.ceil(t / 1000 / HOUR) * HOUR;
  const floorHour = (t: number) => Math.floor(t / 1000 / HOUR) * HOUR;
  const to = q.get("to") ? ceilHour(Date.parse(q.get("to")!)) : ceilHour(Date.now());
  const from = q.get("from") ? floorHour(Date.parse(q.get("from")!)) : to - (SINCE[q.get("since") ?? "1h"] ?? 1) * HOUR;
  const groups = q.getAll("group");
  const interval = q.get("interval");
  const tenant = q.get("tenant");
  const { rows, unallocated } = generate();
  const inRange = rows.filter((r) => r.hour >= from && r.hour < to && (!tenant || r.tenant === tenant) && (!q.get("family") || r.family === q.get("family")));
  const value = (r: Row, g: string) => (g === "tenant" ? r.tenant : g === "run" ? r.run : g === "family" ? r.family : "(none)");
  const bucket = (h: number) => (interval === "day" ? Math.floor(h / 86400) * 86400 : h);
  const aggregate = (withAt: boolean) => {
    const m = new Map<string, { at?: number; group: Record<string, string>; currency: string; micros: number }>();
    for (const r of inRange) {
      const group = Object.fromEntries(groups.map((g) => [g, value(r, g)]));
      const at = withAt ? bucket(r.hour) : undefined;
      const k = JSON.stringify([at, group, r.currency]);
      const e = m.get(k) ?? { at, group, currency: r.currency, micros: 0 };
      e.micros += r.micros;
      m.set(k, e);
    }
    return [...m.values()]
      .sort((a, b) => (a.at ?? 0) - (b.at ?? 0) || JSON.stringify(a.group).localeCompare(JSON.stringify(b.group)) || a.currency.localeCompare(b.currency))
      .map((e) => ({ ...(e.at != null ? { at: new Date(e.at * 1000).toISOString() } : {}), ...(groups.length ? { group: e.group } : {}), currency: e.currency, amount: money(e.micros) }));
  };
  const totals = aggregate(false);
  const body: Record<string, unknown> = { from: new Date(from * 1000).toISOString(), to: new Date(to * 1000).toISOString(), basis: "list", totals };
  if (interval) body.series = aggregate(true);
  if (groups.includes("family")) body.families = [...new Set(totals.map((t) => t.group!.family!))].sort().map((f) => (f === "ai" ? { family: f, displayName: "AI models", color: "violet" } : f === "compute" ? { family: f, displayName: "Compute" } : { family: f }));
  if (groups.includes("run")) body.runs = [...new Set(totals.map((t) => t.group!.run!))].sort().map((id) => ({ id, name: RUNS.find((r) => r.id === id)?.name ?? "" }));
  if (!tenant) {
    const idle = new Map<string, number>();
    for (const [h, by] of unallocated) if (h >= from && h < to) for (const [c, v] of by) idle.set(c, (idle.get(c) ?? 0) + v);
    body.unallocated = [...idle].sort(([a], [b]) => a.localeCompare(b)).map(([currency, v]) => ({ currency, amount: money(v) }));
  }
  return body;
}

export const COST_TENANTS = [
  { id: "ten_acme", name: "acme" },
  { id: "ten_globex", name: "globex" },
];
